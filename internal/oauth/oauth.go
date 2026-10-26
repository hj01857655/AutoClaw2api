// Package oauth 对应 Python 版 lanes/oauth.py —— AutoClaw 账号面的 OAuth 登录
// 与 token 续期。
//
// 这一层存在的理由：原先凭据只能从桌面端的 DPAPI 密文里解，于是**必须同机、
// 同 Windows 用户、装桌面端**，桌面端一退出登录本服务就断。账号面本来就开放了
// OAuth 换 token，所以凭据可以自己拿、自己续、自己存，不再依赖桌面端。
//
// 流程分三步（端点全部来自 main.cjs 2.1.0 的 AuthApiAdapter，非猜测）：
//
//  1. Begin()     起 loopback → POST /userapi/overseasv1/{p}-oauth-url
//     传 navigate_uri，拿授权链接
//  2. 用户浏览器  在授权页登录，provider 302 回
//     http://localhost:{port}/auth/callback-{p}?code=&state=
//  3. Wait()      POST /userapi/overseasv1/{p}-oauth-login 用
//     code+state+navigate_uri 换 token，DPAPI 加密后落盘
//
// **签名**：账号面的 X-Auth-Sign = md5(appId&ts&appKey) 与模型网关那套是同一个
// （实测：缺这三个头时 /userapi/v1/refresh 回 code:400002 Middleware sign error，
// 带上就变成业务码）。请求体还必须带 source_id:"autoclaw" 与 device_id——缺
// device_id 服务端回 code:400001 请求数据有问题（实测）。这两个字段由官方客户端
// 固定写入，调用方无法覆盖，所以本模块同样固定写入。
//
// **device_id 必须稳定且是 UUID 形**：oauth-url / oauth-login / refresh 要用
// 同一个，官方客户端用 crypto.randomUUID() 生成后持久化（DeviceIdentityStore）。
// 本模块同样生成后落盘，不每次随机；格式取标准 UUID（带横线）。
//
// **token 字段是 snake_case**：登录响应里是 access_token / refresh_token /
// user_id（loginCandidate 明写），不是 camelCase。本模块在边界上归一成
// camelCase，内部与桌面端那份凭据结构保持一致。
package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 账号面端点路径（与模型网关同主机，但 base 必须是裸域名）。
const (
	PathOAuthURL     = "/userapi/overseasv1/%s-oauth-url"
	PathOAuthLogin   = "/userapi/overseasv1/%s-oauth-login"
	PathRefresh      = "/userapi/v1/refresh"
	PathRefreshAlt   = "/userapi/v1/agent-refresh"
	SourceID         = "autoclaw"
	DeviceFile       = "device-id.txt"
	CredentialFile   = "oauth-credential.bin" // 旧版单账号文件（读兼容，写入即迁移）
	ActiveFile       = "active-account.txt"   // 多账号：当前选中的账号名
	MaxResponseBytes = 256 * 1024
)

// OAuthHosts provider → 授权页必须落在哪个 host。抄自 main.cjs
// isTrustedOAuthUrl：返回的链接不是这个 host 就判失败，防止把 code 交到第三方。
//
// zai 落到 chatglm.cn（国内智谱账号），google 落到 accounts.google.com。
// 国内手机号注册的账号用 zai，与桌面端同一套账号体系，token 通用。
var OAuthHosts = map[string]string{
	"google": "accounts.google.com",
	"zai":    "chatglm.cn",
}

// CallbackPaths loopback 回调路径与 provider 绑定（同 main.cjs）。
var CallbackPaths = map[string]string{
	"google": "/auth/callback-google",
	"zai":    "/auth/callback-zai",
}

// CallbackPorts 端口按序试，全占用才失败（抄自 main.cjs）。
var CallbackPorts = []int{18432, 19654, 19723, 53699}

// AttemptTimeoutS 一次授权流程的等待上限（main.cjs OAUTH_ATTEMPT_TIMEOUT_MS）。
const AttemptTimeoutS = 600

// Error 账号面失败。Code 是上游业务码（HasCode=false 表示网络/协议层失败）。
type Error struct {
	Message string
	Code    float64
	HasCode bool
	Raw     map[string]any
}

func (e *Error) Error() string { return e.Message }

func newErr(msg string, code *float64, raw map[string]any) *Error {
	e := &Error{Message: msg, Raw: raw}
	if e.Raw == nil {
		e.Raw = map[string]any{}
	}
	if code != nil {
		e.Code, e.HasCode = *code, true
	}
	return e
}

// businessCodes 上游业务码 → 人话。官方客户端自己也是先把码翻成语义再决定提示
// 什么（main.cjs 的 mapBusinessFailure3），直接显示 msg 会得到「请求数据有问题」
// 这种对用户毫无信息量的句子（实测缺 device_id 就是 400001）。
var businessCodes = map[int64][2]string{
	400001: {"请求没被接受", "参数缺失或格式不对"},
	400002: {"签名没通过", "检查 appId / appKey"},
	429:    {"操作太频繁", "等一会儿再试"},
	630203: {"操作太频繁", "等一会儿再试"},
	630012: {"账号最近注销过", "换个账号"},
	631010: {"不是该企业成员", "用个人账号登"},
	631011: {"企业已停用", "联系管理员"},
	631012: {"企业服务不可用", "稍后再试"},
	631013: {"企业不支持此操作", "用个人账号登"},
	631014: {"所在地区不支持", "换网络环境"},
	631015: {"需要选身份", "企业账号要先选身份"},
	631016: {"身份凭证过期", "重新登录"},
}

// Explain 业务码 → (一句话原因, 建议)。认不出的码返回两个空串。
// Python 用 isinstance(code, int) 判类型，所以这里只认整数码。
func Explain(code any) (string, string) {
	f, ok := code.(float64)
	if !ok {
		return "", ""
	}
	// Python 的 json.loads 把 429.0 解成 float 时不会被 isinstance(int) 命中；
	// 只有整数值才算码
	if f != float64(int64(f)) {
		return "", ""
	}
	got, ok := businessCodes[int64(f)]
	if !ok {
		return "", ""
	}
	return got[0], got[1]
}

// FmtExp 到期文案 "2026-01-02 15:04:05 (16.2h)"；解不出返回 "?"。
// 横幅 / 面板 / probe 共用这一份。
func FmtExp(token string) string {
	exp, ok := JWTExp(token)
	if !ok || exp == 0 {
		return "?"
	}
	left := float64(exp-time.Now().Unix()) / 3600.0
	return fmt.Sprintf("%s (%.1fh)", time.Unix(exp, 0).Format("2006-01-02 15:04:05"), left)
}

// JWTExp 解 JWT payload 里的 exp（秒）。解不开返回 (0, false)。
func JWTExp(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// 上游偶尔给标准 base64（含 +/），兜一次
		if raw, err = base64.StdEncoding.DecodeString(payload); err != nil {
			return 0, false
		}
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return 0, false
	}
	switch v := data["exp"].(type) {
	case float64:
		return int64(v), true
	}
	return 0, false
}

// JWTClaims 解 JWT payload 成 map。导入凭据时从 token 里取账号名用。
func JWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(payload); err != nil {
			return nil
		}
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	return data
}

// providerList 报错文案里的 provider 清单（Python 用 " / ".join(OAUTH_HOSTS)，
// dict 顺序即插入序：google 在前）。
func providerList() string { return "google / zai" }

func fmtCode(v *float64) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprintf("%.0f", *v)
}
