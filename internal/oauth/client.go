// oauth 的账号面请求层，对应 Python 版 lanes/oauth.py 的 _post / oauth_url /
// exchange_oauth / refresh。
package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"autoclaw2api/internal/lanes"
)

// HTTPTimeout 账号面单次请求超时（main.cjs AUTH_TIMEOUT_MS = 15e3，放宽到 20s）。
const HTTPTimeout = 20 * time.Second

// Client 账号面 OAuth 通道。Base 是裸域名（与模型网关带路径的 base 不同）。
type Client struct {
	Base     string
	Version  string
	Platform string
	AppID    string
	AppKey   string
	DeviceID string
	Extra    []string // 授权链接 host 白名单补充项（已小写）
	HTTP     *http.Client
}

func NewClient(base, version, platform, appID, appKey, deviceID string, extra []string) *Client {
	return &Client{
		Base: base, Version: version, Platform: platform,
		AppID: appID, AppKey: appKey, DeviceID: deviceID, Extra: extra,
		HTTP: &http.Client{Timeout: HTTPTimeout},
	}
}

// signHeaders 账号面完整身份头。
//
// X-Auth-* 三件套只是中间件签名，**不是全部**。main.cjs 里 auth 与模型两个
// client 都过同一个 managed 层（createZworkManagedRequestHeaders），那层还会
// 注入 X-Version / X-Tm / X-Product / X-Lang / X-Channel。实测只发三件套时
// zai-oauth-url 回 631000 User login error——签名过了（否则是 400002 sign
// error），业务层因缺少客户端身份标识而拒。补齐后同一请求即通过。
//
// 与 lanes.ClientIdentityHeaders 复用同一套（账号面与模型网关的 managed 层是
// 同一个，多带的 X-Client-Type / x_trace_id 实测不影响）。
func (c *Client) signHeaders() map[string]string {
	h := map[string]string{}
	for k, v := range lanes.AuthSignHeaders(c.AppID, c.AppKey) {
		h[k] = v
	}
	for k, v := range lanes.ClientIdentityHeaders(c.Version, c.Platform) {
		h[k] = v
	}
	return h
}

// Post POST 一个账号面端点，返回 data 字段。
//
// 上游成功时 HTTP 也是 200，业务结果全在 body 的 code 里——所以这里
// **不看 HTTP 码看 code**，与官方客户端 if (envelope.code === 0) 一致。
func (c *Client) Post(path string, payload map[string]any) (map[string]any, error) {
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "AutoClaw/" + c.Version,
	}
	for k, v := range c.signHeaders() {
		headers[k] = v
	}
	// source_id 与 device_id 由官方客户端固定写进请求体，服务端认这两个键
	body := map[string]any{}
	for k, v := range payload {
		body[k] = v
	}
	body["source_id"] = SourceID
	body["device_id"] = c.DeviceID
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, newErr("账号面请求失败: "+err.Error(), nil, nil)
	}

	req, err := http.NewRequest(http.MethodPost, c.Base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, newErr("账号面请求失败: "+err.Error(), nil, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, newErr("账号面请求失败: "+err.Error(), nil, nil)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))

	var envelope map[string]any
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		peek := data
		if len(peek) > 120 {
			peek = peek[:120]
		}
		return nil, newErr(
			fmt.Sprintf("账号面返回非 JSON（HTTP %d）: %s", resp.StatusCode, peek), nil, nil)
	}

	code, hasCode := numeric(envelope["code"])
	if !hasCode || code != 0 {
		var codePtr *float64
		if hasCode {
			v := code
			codePtr = &v
		}
		msg, _ := envelope["msg"].(string)
		reason, advice := Explain(envelope["code"])
		var message string
		if reason != "" {
			// 有映射就用映射的话（上游 msg 对 400001 这类码毫无信息量），
			// 没有就退回上游原文，别把信息吞了。
			message = reason
			if advice != "" {
				message += " —— " + advice
			}
			if msg != "" {
				message += "（上游：" + msg + "）"
			}
		} else if msg != "" {
			message = msg
		} else {
			message = "账号面业务失败"
		}
		return nil, newErr(message, codePtr, envelope)
	}
	out, _ := envelope["data"].(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// OAuthURL 第一步：拿授权页链接。
func (c *Client) OAuthURL(provider, navigateURI string) (string, error) {
	if _, ok := OAuthHosts[provider]; !ok {
		return "", newErr("provider 只能是 "+providerList(), nil, nil)
	}
	data, err := c.Post(fmt.Sprintf(PathOAuthURL, provider),
		map[string]any{"navigate_uri": navigateURI})
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(asString(data["oauth_url"]))
	if url == "" {
		return "", newErr("响应里没有 oauth_url", nil, data)
	}
	if !c.Trusted(url, provider) {
		return "", newErr("授权链接不在预期 host（"+OAuthHosts[provider]+
			"），已拒绝: "+url, nil, data)
	}
	return url, nil
}

// Trusted 授权链接是否落在该 provider 应有的 host 上。
//
// 抄 main.cjs isTrustedOAuthUrl：必须 https、不带用户凭证、不带端口、host
// 精确匹配。这一步是把 code 交出去前的最后一道校验——授权页跑到别家域名上，
// 说明链接被换过，交 code 等于把账号交出去。
func (c *Client) Trusted(url, provider string) bool {
	u, err := parseURL(url)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	if u.User || u.Port != "" {
		return false
	}
	host := strings.ToLower(u.Host)
	expected := OAuthHosts[provider]
	if expected != "" && host == expected {
		return true
	}
	for _, extra := range c.Extra {
		if extra == host {
			return true
		}
	}
	return false
}

// Credential OAuth 凭据（与桌面端解出的那份结构对齐，camelCase）。
type Credential struct {
	AccessToken  string  `json:"accessToken"`
	RefreshToken string  `json:"refreshToken"`
	Account      string  `json:"account"`
	ExpiresAt    *int64  `json:"expiresAt"`
	Phone        string  `json:"phone,omitempty"`
	AccountType  string  `json:"accountType,omitempty"`
	SavedAt      float64 `json:"savedAt"`
}

// ExchangeOAuth 第三步：code + state 换 token。
//
// 上游字段是 snake_case（loginPayload 读 access_token / refresh_token /
// user_id），这里归一成 camelCase，与桌面端那份凭据结构对齐，下游不用分心。
func (c *Client) ExchangeOAuth(provider, code, state, navigateURI string) (*Credential, error) {
	if code == "" || state == "" {
		return nil, newErr("回调里没有 code 或 state（用户取消了授权？）", nil, nil)
	}
	data, err := c.Post(fmt.Sprintf(PathOAuthLogin, provider), map[string]any{
		"code": code, "state": state, "navigate_uri": navigateURI,
	})
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(asString(data["access_token"]))
	refreshToken := strings.TrimSpace(asString(data["refresh_token"]))
	account := strings.TrimSpace(asString(data["user_id"]))
	if token == "" || refreshToken == "" || account == "" {
		// 官方客户端同样把这三样齐全才算成功（loginPayloadIncomplete）
		return nil, newErr("登录响应不完整（需要 access_token / refresh_token / user_id）",
			nil, data)
	}
	cred := &Credential{
		AccessToken: token, RefreshToken: refreshToken, Account: account,
		Phone:       asString(data["user_phone"]),
		AccountType: asString(data["account_type"]),
		SavedAt:     float64(time.Now().Unix()),
	}
	if exp, ok := JWTExp(token); ok {
		v := exp
		cred.ExpiresAt = &v
	}
	return cred, nil
}

// Refresh 用 refreshToken 换新 token。
//
// main.cjs 的刷新策略是主路径失败后走兜底路径，这里照做：
// /userapi/v1/refresh → /userapi/v1/agent-refresh。
//
// 实测（2026-10-19，桌面端凭据）这条是通的，code:0。两个反直觉的点：
//   - 响应里**没有 user_id**（account 只能由调用方传进来保留，否则续期后账号
//     会变成空串）；
//   - 响应带 refresh_token（新的）与 refresh（**bool，不是 token**，别被字段名
//     骗了）。实测 refreshToken 不是一次性的——旧值与新值都能再刷，所以存错了
//     也不会把账号锁死。
//
// 这也是「不靠 OAuth 也能续期」的那条路：桌面端凭据里本就带 refreshToken。
func (c *Client) Refresh(refreshToken, account string) (*Credential, error) {
	var last *Error
	for _, path := range []string{PathRefresh, PathRefreshAlt} {
		data, err := c.Post(path, map[string]any{"refresh_token": refreshToken})
		if err != nil {
			if e, ok := err.(*Error); ok {
				last = e
			} else {
				last = newErr(err.Error(), nil, nil)
			}
			continue
		}
		token := strings.TrimSpace(asString(data["access_token"]))
		if token == "" {
			last = newErr("刷新响应里没有 access_token", nil, data)
			continue
		}
		newRefresh := strings.TrimSpace(asString(data["refresh_token"]))
		if newRefresh == "" {
			newRefresh = refreshToken
		}
		cred := &Credential{
			AccessToken: token, RefreshToken: newRefresh, Account: account,
			SavedAt: float64(time.Now().Unix()),
		}
		if exp, ok := JWTExp(token); ok {
			v := exp
			cred.ExpiresAt = &v
		}
		return cred, nil
	}
	if last != nil {
		return nil, last
	}
	return nil, newErr("刷新失败", nil, nil)
}

// LoadDeviceID 读 device_id，没有就生成一个并落盘。
//
// 必须稳定：oauth-url / oauth-login / refresh 要用同一个。官方客户端同样持久
// 化它（main.cjs 的 DeviceIdentityStore）。
func LoadDeviceID(dir string, log *slog.Logger) string {
	path := dir + string(byteSep) + DeviceFile
	if b, err := readFile(path); err == nil {
		if existing := strings.TrimSpace(string(b)); existing != "" {
			return existing
		}
	}
	id := newUUID4()
	// 先写临时文件再替换：避免进程在半途被杀留下截断的文件
	if err := writeFileAtomic(path, []byte(id)); err != nil {
		// 落盘失败不阻塞登录——本次进程内仍可用，只是下次会换一个 id
		log.Warn("device_id 落盘失败（下次登录会换 id）", slog.String("err", err.Error()))
	}
	return id
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func numeric(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
