// Package account 对应 Python 版 lanes/account.py —— AutoClaw 账号面：
// 积分任务领取、余额与流水查询。
//
// 这些端点都是 2026-10-19 在桌面端凭据下实测打通的（国内域
// autoglm-acceleration-api.zhipuai.cn，token 来自桌面端 DPAPI 解密）：
//
//	ClaimTask()        POST /autoclaw-proxy/proxy/autoclaw-task-complete
//	ListTasks()        GET  /autoclaw-proxy/proxy/autoclaw-task-list
//	WalletBalance()    GET  /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw
//	CreditLedgers()    GET  /agent-assetmgr/api/v1/ledgers_std
//	PointsExpiring()   GET  /agent-assetmgr/api/v1/points/expiring
//
// **签到路径反直觉**：文档里写的 POST /userapi/v1/user-info 是桌面端 main.cjs 的
// DAILY_CREDITS_CLAIM_PATH，实测在当前账号上一分没给（code:0 但 completed:false）。
// 真正给分的是任务面的 autoclaw-task-complete（task_id=daily_signin → 200 分）。
//
// **拿不到不等于零**：查余额失败时返回错误，调用方必须区分「查不到」与
// 「余额是 0」，混成 0 会让人以为号废了。
package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autoclaw2api/internal/lanes"
)

const (
	SourceID        = "autoclaw"
	pathWallets     = "/agent-assetmgr/api/v2/wallets"
	pathPointsExp   = "/agent-assetmgr/api/v1/points/expiring"
	pathLedgers     = "/agent-assetmgr/api/v1/ledgers_std"
	maxResponseByte = 256 * 1024

	// 任务端点的完整路径。**注意形状**：是 /autoclaw-proxy/proxy/autoclaw-task-list，
	// 不是 /autoclaw-proxy/proxy/autoclaw/autoclaw-task-list（那个 404）。也就是说它
	// 挂在 /autoclaw-proxy/proxy/ 之下，与 UPSTREAM_BASE 的 /autoclaw-proxy/proxy/autoclaw
	// 是**兄弟**关系，不是父子——所以 base 要传裸域名（ACCOUNT_BASE），不传 UPSTREAM_BASE。
	pathTaskComplete = "/autoclaw-proxy/proxy/autoclaw-task-complete"
	pathTaskList     = "/autoclaw-proxy/proxy/autoclaw-task-list"

	// TaskDailySignin 每日签到任务 id。
	TaskDailySignin = "daily_signin"
)

// walletLabels 钱包分类 → 中文名。上游已给 display_name，这里只作兜底。
var walletLabels = map[string]string{
	"reward":       "奖励积分",
	"daily":        "每日积分",
	"subscription": "订阅积分",
	"fuel_pack":    "加油包积分",
	"other":        "其他积分",
}

// nonzeroOnly 只把有余额的钱包返回给上层：五个里通常四个是 0，全塞给面板只是噪音。
const nonzeroOnly = true

// Error 账号面失败。Code 是上游业务码（0 且 Err 为网络错时 nil 语义）。
type Error struct {
	Message string
	Code    float64
	HasCode bool
	Raw     map[string]any
}

func (e *Error) Error() string { return e.Message }

// Client 账号面通道。
type Client struct {
	DumpDir  string // 非空时账号面原始响应落盘（排障）
	Base     string // 裸域名 ACCOUNT_BASE
	CNBase   string // 国内版账号面裸域（任务/签到/钱包端点两域同路径，只换 base）
	RegionFn func() string
	Version  string
	Platform string
	AppID    string
	AppKey   string
	HTTP     *http.Client
}

func New(base, version, platform, appID, appKey string) *Client {
	return &Client{
		Base: base, Version: version, Platform: platform, AppID: appID, AppKey: appKey,
		HTTP: &http.Client{Timeout: lanes.AccountTimeout},
	}
}

// headers 签名头 + 身份头 + Bearer。
// token 直接透传：桌面端解出来的 accessToken 自带 "Bearer " 前缀（实测），
// 再套一层会变成 "Bearer Bearer ..."。
func (c *Client) headers(token string) map[string]string {
	h := map[string]string{
		"Content-Type":  "application/json",
		"User-Agent":    "AutoClaw/" + c.Version,
		"Authorization": token,
	}
	for k, v := range lanes.AuthSignHeaders(c.AppID, c.AppKey) {
		h[k] = v
	}
	for k, v := range lanes.ClientIdentityHeaders(c.Version, c.Platform) {
		h[k] = v
	}
	return h
}

// do 发一个账号面请求，返回原始 body 与 HTTP 状态码。
//
// 双域分流与 upstream.Client 同一模式：激活账号是国内号（RegionFn()=="cn"）
// 就走 CNBase——账号面 token 归属校验按域走，国际 token 打国内域 401、反之亦然，
// 签到/余额必须跟账号的域走。
func (c *Client) do(method, path, token string, payload []byte) ([]byte, int, error) {
	base := c.Base
	if c.RegionFn != nil && c.RegionFn() == "cn" && c.CNBase != "" {
		base = c.CNBase
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, 0, &Error{Message: "账号面请求失败: " + err.Error()}
	}
	for k, v := range c.headers(token) {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, &Error{Message: "账号面请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte))
	if err != nil {
		return nil, resp.StatusCode, &Error{Message: "账号面响应读取失败: " + err.Error()}
	}
	if c.DumpDir != "" {
		if dir, err := os.MkdirTemp(c.DumpDir, "acc-*"); err == nil {
			os.Remove(dir)
			f := filepath.Join(c.DumpDir, fmt.Sprintf("acc-%d.r%s.txt", time.Now().UnixMilli(), strings.ToLower(path[:min(12, len(path))])+fmt.Sprint(resp.StatusCode)))
			_ = os.WriteFile(strings.ReplaceAll(f, "/", string(os.PathSeparator)), append([]byte(method+" "+path+"\n"), body...), 0644)
		}
	}
	return body, resp.StatusCode, nil
}

// unwrap 解信封：HTTP 200 不代表成功，业务结果全在 body 的 code 里——
// **不看 HTTP 码看 code**，与官方客户端 `if (envelope.code === 0)` 一致。
func unwrap(body []byte, status int) (map[string]any, error) {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		peek := body
		if len(peek) > 120 {
			peek = peek[:120]
		}
		return nil, &Error{
			Message: fmt.Sprintf("账号面返回非 JSON（HTTP %d）: %s", status, string(peek)),
		}
	}
	code, hasCode := numberVal(envelope["code"])
	if !hasCode || code != 0 {
		msg, _ := envelope["msg"].(string)
		if msg == "" {
			msg = "账号面业务失败"
		}
		return nil, &Error{Message: msg, Code: code, HasCode: hasCode, Raw: envelope}
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// Wallet 单个分类钱包。
type Wallet struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	Balance int64  `json:"balance"`
	View    string `json:"view"`
}

// BalanceResult 余额快照。
type BalanceResult struct {
	Total   int64    `json:"total"`
	Wallets []Wallet `json:"wallets"`
}

// WalletBalance 查积分余额。查不到时返回 error——调用方应区分「查不到」与「余额为 0」。
func (c *Client) WalletBalance(token string) (*BalanceResult, error) {
	body, status, err := c.do(http.MethodGet, pathWallets+"?biz_app_id="+SourceID, token, nil)
	if err != nil {
		return nil, err
	}
	data, err := unwrap(body, status)
	if err != nil {
		return nil, err
	}
	out := &BalanceResult{}
	for _, wi := range asList(data["wallets"]) {
		item, ok := wi.(map[string]any)
		if !ok {
			continue
		}
		balance, _ := numberVal(item["balance"])
		wtype := strings.TrimSpace(asStr(item["public_wallet_type"]))
		label := asStr(item["display_name"])
		if label == "" {
			label = walletLabels[wtype]
			if label == "" {
				label = wtype
			}
		}
		view := asStr(item["balance_view"])
		if view == "" {
			view = fmt.Sprintf("%d", int64(balance))
		}
		if nonzeroOnly && balance <= 0 {
			continue
		}
		out.Wallets = append(out.Wallets, Wallet{Type: wtype, Label: label, Balance: int64(balance), View: view})
	}
	total, _ := numberVal(data["total_balance"])
	out.Total = int64(total)
	return out, nil
}

// Expiring 即将过期的积分（expiring<=0 时返回 nil，调用方据此判断要不要提示）。
type Expiring struct {
	Expiring int64  `json:"expiring"`
	When     string `json:"when"`
	Text     string `json:"text"`
}

var expireUnits = map[string]string{"day": "天", "month": "个月", "year": "年"}

// PointsExpiring 查即将过期的积分。expire_time_type 是 day/month，
// expire_time_value 是数量（3 = 3 天后）。
func (c *Client) PointsExpiring(token string) (*Expiring, error) {
	body, status, err := c.do(http.MethodGet, pathPointsExp, token, nil)
	if err != nil {
		return nil, err
	}
	data, err := unwrap(body, status)
	if err != nil {
		return nil, err
	}
	expiring, _ := numberVal(data["expiring_points"])
	if expiring <= 0 {
		return nil, nil
	}
	when := ""
	if v, ok := numberVal(data["expire_time_value"]); ok {
		if unit := strings.TrimSpace(asStr(data["expire_time_type"])); unit != "" {
			zh := expireUnits[unit]
			if zh == "" {
				zh = unit
			}
			when = fmt.Sprintf("%d %s后", int64(v), zh)
		}
	}
	return &Expiring{Expiring: int64(expiring), When: when, Text: asStr(data["expiring_points_text"])}, nil
}

// Ledger 一条积分流水。
type Ledger struct {
	ID          string `json:"id"`
	Amount      int64  `json:"amount"`
	Kind        string `json:"kind"`
	Wallet      string `json:"wallet"`
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Model       string `json:"model"`
	Mode        string `json:"mode"`
	CreatedAt   string `json:"created_at"`
}

// CreditLedgers 查积分流水。
//
// **时间字段是 occurred_at，不是 created_at**（实测响应里根本没有后者的键，
// 取错了会拿到空串，面板上的时间列就全空）。另外还有 wallet_expires_at——
// 钱包本身的到期时间，不是这笔流水的发生时间。
//
// 每条 mutation_type 是 debit（花）/ credit（得），amount 带符号；metadata 里有
// actual_model 与 autoclaw_model_mode——所以这不只是账本，能回答「这次对话花了
// 多少分、走的哪个模型」。
func (c *Client) CreditLedgers(token string, page, pageSize int) ([]Ledger, error) {
	path := fmt.Sprintf("%s?page=%d&page_size=%d", pathLedgers, page, pageSize)
	body, status, err := c.do(http.MethodGet, path, token, nil)
	if err != nil {
		return nil, err
	}
	data, err := unwrap(body, status)
	if err != nil {
		return nil, err
	}
	out := []Ledger{}
	for _, li := range asList(data["entries"]) {
		item, ok := li.(map[string]any)
		if !ok {
			continue
		}
		amount, _ := numberVal(item["amount"])
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		created := asStr(item["occurred_at"])
		if created == "" {
			created = asStr(item["created_at"])
		}
		if created == "" {
			created = asStr(item["createdAt"])
		}
		out = append(out, Ledger{
			ID:          asStr(item["id"]),
			Amount:      int64(amount),
			Kind:        asStr(item["mutation_type"]),
			Wallet:      asStr(item["wallet_type"]),
			Scope:       asStr(item["wallet_scope"]),
			Description: asStr(item["description"]),
			Model:       asStr(meta["actual_model"]),
			Mode:        asStr(meta["autoclaw_model_mode"]),
			CreatedAt:   created,
		})
	}
	return out, nil
}

// ClaimResult 领取结果。四种必须分开，提示语完全不同：
//
//	Completed=true  真领到了
//	Already=true    今天已经领过了（幂等，正常）
//	Rejected=true   服务端不认这个客户端触发（invite_friends / upgrade_pc_app
//	                就是这种）——**不是失败，是「这个任务不该在这里领」**
//	三者皆 false    服务端没认这次请求（真失败）
type ClaimResult struct {
	Completed bool
	Already   bool
	Rejected  bool
	Points    int64
	Message   string
}

// ClaimTask 领指定任务的积分。base 必须是 ACCOUNT_BASE（裸域名），任务端点
// 与 UPSTREAM_BASE 是 /autoclaw-proxy/proxy/ 下的兄弟路径。
//
// **受限任务的响应形状和成功时不一样**（实测）：成功/已签过是 {"data":{...}}，
// 而受限任务是 HTTP 400 + 顶层 {"message": "..."}，压根没有 data。所以不能看到
// 没有 data 就报「结构不对」——那会把一个已知的正常情况当成故障。
func (c *Client) ClaimTask(token, taskID string) (*ClaimResult, error) {
	payload, _ := json.Marshal(map[string]string{"task_id": taskID})
	body, status, err := c.do(http.MethodPost, pathTaskComplete, token, payload)
	if err != nil {
		return nil, err
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		peek := body
		if len(peek) > 120 {
			peek = peek[:120]
		}
		return nil, &Error{Message: fmt.Sprintf("领取返回非 JSON（HTTP %d）: %s", status, string(peek))}
	}

	if _, hasData := envelope["data"]; !hasData {
		message := asStr(envelope["message"])
		if status >= 400 && message != "" {
			return &ClaimResult{Message: message, Rejected: true}, nil
		}
		code, hasCode := numberVal(envelope["code"])
		// HTTP 200 且 code==0 但无 data = 今日已签（幂等，与官方插件 dailySignin
		// 对 data 的 null 容错同语义），不是错误。
		if status == http.StatusOK && (!hasCode || code == 0) {
			return &ClaimResult{Already: true, Message: "今日已签到", Points: 0}, nil
		}
		if message == "" {
			message = "领取响应里没有 data"
		}
		return nil, &Error{Message: message, Code: code, HasCode: hasCode, Raw: envelope}
	}

	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil, &Error{Message: fmt.Sprintf("领取响应结构不对: %T", envelope["data"]), Raw: envelope}
	}
	already := truthy(data["already_completed"])
	points, _ := numberVal(data["reward_points"])
	success := truthy(data["success"])
	message := asStr(data["message"])
	// 兜底：万一上游哪天把「不支持」放回 200 的 data 里，也照样认出来
	rejected := !success && !already && strings.Contains(message, "不支持")
	return &ClaimResult{
		Completed: success && !already,
		Already:   already,
		Rejected:  rejected,
		Points:    int64(points),
		Message:   message,
	}, nil
}

// Task 一个可领取任务。
type Task struct {
	TaskID    string `json:"task_id"`
	Title     string `json:"title"`
	Points    int64  `json:"points"`
	Status    string `json:"status"`
	Type      string `json:"type"`
	Claimable bool   `json:"claimable"`
}

// ListTasks 列任务（每日签到 / 灵感中心 / 邀请好友 / 升级客户端）。
//
// 响应带信封 {"data":[...]}，**没有 code 字段**——所以这里不能走 unwrap 的
// code 判定，自己解。client_triggered=False 的任务点了也领不到，这个字段必须
// 带出去，让 UI 别渲染领取按钮。
func (c *Client) ListTasks(token string) ([]Task, error) {
	body, status, err := c.do(http.MethodGet, pathTaskList+"?lang=zh-CN", token, nil)
	if err != nil {
		return nil, err
	}
	var envelope any
	if json.Unmarshal(body, &envelope) != nil {
		peek := body
		if len(peek) > 120 {
			peek = peek[:120]
		}
		return nil, &Error{Message: fmt.Sprintf("任务列表返回非 JSON（HTTP %d）: %s", status, string(peek))}
	}
	items := envelope
	if m, ok := envelope.(map[string]any); ok {
		if d, has := m["data"]; has {
			items = d
		}
	}
	list, ok := items.([]any)
	if !ok {
		return nil, &Error{Message: fmt.Sprintf("任务列表结构不对（期望 data 为数组）: %T", items)}
	}
	out := []Task{}
	for _, ii := range list {
		item, ok := ii.(map[string]any)
		if !ok {
			continue
		}
		points, _ := numberVal(item["reward_points"])
		claimable := true
		if v, has := item["client_triggered"]; has {
			claimable = truthy(v)
		}
		// 默认 True：老字段万一哪天上游不给了，宁可让用户点一下看报错，
		// 也别把本来能领的任务藏起来。
		out = append(out, Task{
			TaskID:    asStr(item["task_id"]),
			Title:     asStr(item["title"]),
			Points:    int64(points),
			Status:    asStr(item["status"]),
			Type:      asStr(item["task_type"]),
			Claimable: claimable,
		})
	}
	return out, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func numberVal(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b != ""
	case nil:
		return false
	default:
		if f, ok := numberVal(v); ok {
			return f != 0
		}
		return true
	}
}
