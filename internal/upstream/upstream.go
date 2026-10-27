// Package upstream 对应 Python 版 server.py 的 _open_upstream：上游 chat POST
// 与退避重试、401 自愈。
//
// 401/406/403 都是请求形状或账号状态问题，重试无意义；只对连接层失败与
// 429/5xx 退避重试。max_tokens 造成的 406 在发出前就已消除，不会到这里。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/proxyclient"
)

// MaxAttempts 总尝试次数（含首次）。
const MaxAttempts = 4

// Backoff 各次重试前的等待（长度 = MaxAttempts-1）。
var Backoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// TransientStatus 只有这些状态码值得重试。
var TransientStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// DialTimeout / ReadTimeout 对应 Python _HTTP_TIMEOUT = (10, 300)。
const (
	DialTimeout = 10 * time.Second
	ReadTimeout = 300 * time.Second
)

// NewHTTPClient 上游用的 client。
//
// 不设 Client.Timeout：流式响应要挂 300s 以上，用 Client.Timeout 会把正常长
// 回答掐断；改为 connect 10s + ResponseHeaderTimeout 300s（等价 Python 的
// requests timeout=(10, 300) 两段式）。
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ResponseHeaderTimeout: ReadTimeout,
			MaxIdleConns:          8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   DialTimeout,
		},
	}
}

// Client 上游通道。
type Client struct {
	Base string // 带路径前缀的模型网关 base，如 …/autoclaw-proxy/proxy/autoclaw
	// 双域路由：RegionFn 返回当前激活账号的域（cn|global|空=global）。
	// global 用 Base/Path；cn 用 CNBase/CNPath（managed gateway，路径无 /v1）。
	CNBase   string
	CNPath   string
	RegionFn func() string
	Path     string // /chat/completions
	Version  string
	Platform string
	AppID    string
	AppKey   string
	HTTP     *http.Client
	Log      *slog.Logger

	// Token 取当前可用 accessToken（对应 Python 的 _token()）。
	Token func() (string, error)
	// Invalidate 让凭据缓存失效（对应 _store.invalidate()）。
	Invalidate func()

	// 多账号轮转的触发点（main 注入 oauth.Store 的对应方法；nil = 单账号语义）：
	//   AccountLimited 当前账号被上游限流/拒绝，凭据层把它放进冷却表，下次
	//                  Token() 自动换备选账号；
	//   AccountOK      请求成功，清掉当前账号的冷却。
	AccountLimited func()
	AccountOK      func()

	// 每账号出网代理：返回当前激活账号的代理 URL（空=直连）。main 绑定
	// oauth.Store.ActiveProxy；Open 每轮请求重新取值——轮转换号后下一个
	// 请求自动用新账号的代理。
	ProxyFn func() string

	// Sleep 退避等待；测试里注入 no-op，避免真等 2/4/8 秒。
	Sleep func(time.Duration)
}

func New(base, path, version, platform, appID, appKey string,
	token func() (string, error), invalidate func(), log *slog.Logger) *Client {
	return &Client{
		Base: base, Path: path, Version: version, Platform: platform,
		AppID: appID, AppKey: appKey, HTTP: NewHTTPClient(),
		Token: token, Invalidate: invalidate, Log: log,
		Sleep: time.Sleep,
	}
}

// Open 把（已翻译成 Chat 的）请求体发往上游，返回**尚未读取 body** 的 Response。
// 调用方负责关闭 body。
//
// 每轮重试都重跑 BuildRequest：X-Auth-Sign 带时间戳、X-Request-Id 每次新，
// 复用旧 header 会让上游按 sign 过期或 req_id 去重拒掉。
func (c *Client) Open(rid string, payload map[string]any) (*http.Response, error) {
	token, err := c.Token()
	if err != nil {
		return nil, err
	}
	retried401 := false
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		headers, body, meta := lanes.BuildRequest(
			payload, token, c.Version, c.Platform, c.AppID, c.AppKey)
		if hits := asStrings(meta["hits"]); len(hits) > 0 {
			c.Log.Info("已中和外来身份句",
				slog.String("rid", rid), slog.String("hits", strings.Join(hits, ", ")))
		}
		if injected, _ := meta["injected"].(bool); injected {
			c.Log.Info("已注入 2.x banner", slog.String("rid", rid))
		}

		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		base, path := c.Base, c.Path
		if c.RegionFn != nil && c.RegionFn() == "cn" && c.CNBase != "" && c.CNPath != "" {
			base, path = c.CNBase, c.CNPath
		}
		req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := c.HTTP
		if c.ProxyFn != nil {
			if pc := proxyclient.For(c.ProxyFn()); pc != nil {
				client = pc
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			if attempt < MaxAttempts-1 {
				c.Log.Warn("上游连接失败，稍后重试",
					slog.String("rid", rid),
					slog.Duration("wait", backoffFor(attempt)),
					slog.Int("attempt", attempt+1),
					slog.String("err", err.Error()))
				c.Sleep(backoffFor(attempt))
				continue
			}
			return nil, err
		}

		if resp.StatusCode == http.StatusOK {
			if c.AccountOK != nil {
				c.AccountOK() // 成功即清冷却（上游可能提前解除限流）
			}
			return resp, nil
		}

		// 401：第一次只重解凭据（token 可能刚被轮换）；再 401 说明账号本身
		// 被上游拒了，进冷却换备选账号。429：限流，直接进冷却换号——在
		// 同一账号上退避重试只会继续撞墙。最后一次尝试的 401/429 不再重试，
		// 原样透传给 handler 归一（与既有行为一致）。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= MaxAttempts-1 {
				return resp, nil
			}
			resp.Body.Close()
			retried401 = retried401 || resp.StatusCode == http.StatusUnauthorized
			marked := false
			if c.AccountLimited != nil && (resp.StatusCode == http.StatusTooManyRequests || retried401) {
				c.AccountLimited()
				marked = true
			}
			if c.Invalidate != nil {
				c.Invalidate()
			}
			if token, err = c.Token(); err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				c.Log.Warn("上游 429，当前账号进冷却，换号重试", slog.String("rid", rid))
			} else if marked {
				c.Log.Warn("上游持续 401，当前账号进冷却，换号重试", slog.String("rid", rid))
			} else {
				c.Log.Info("上游 401，重新解密凭据后重试", slog.String("rid", rid))
			}
			continue
		}

		if resp.StatusCode == 200 || !TransientStatus[resp.StatusCode] {
			return resp, nil
		}
		if attempt < MaxAttempts-1 {
			resp.Body.Close()
			c.Log.Warn("上游可重试状态码",
				slog.String("rid", rid), slog.Int("status", resp.StatusCode),
				slog.Duration("wait", backoffFor(attempt)),
				slog.Int("attempt", attempt+1))
			c.Sleep(backoffFor(attempt))
			continue
		}
		return resp, nil
	}
	return nil, errors.New("上游重试次数耗尽")
}

func backoffFor(attempt int) time.Duration {
	if attempt < len(Backoff) {
		return Backoff[attempt]
	}
	return Backoff[len(Backoff)-1]
}

func asStrings(v any) []string {
	l, _ := v.([]string)
	return l
}

// UnwrapDataLayer 上游非流式响应有时会多包一层 {"data":{...}}：Python 版原样
// 透传是已知缺陷（客户端拿不到顶层 choices）。内层确实像 chat completion
// （有 choices 或 object 字段）才解包，避免误伤正常响应。
//
// 注：本仓库的 dumps 里没有出现过这种包法，按 brief 要求仍做兜底解包——它只在
// 「外层带 data 且内层像 completion」时生效，正常响应零影响。
func UnwrapDataLayer(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	var outer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &outer); err != nil || len(outer.Data) == 0 {
		return body
	}
	var inner struct {
		Choices json.RawMessage `json:"choices"`
		Object  string          `json:"object"`
	}
	if err := json.Unmarshal(outer.Data, &inner); err != nil {
		return body
	}
	if len(inner.Choices) == 0 && inner.Object == "" {
		return body
	}
	return outer.Data
}

// ReadAll 读完并关闭 body（非流式场景）。限 8MB，和 SSE 单行上限同量级。
func ReadAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
