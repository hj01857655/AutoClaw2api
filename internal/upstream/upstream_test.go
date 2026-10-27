// upstream.Open 的重试/换号状态机直接测试（handler golden 测试只覆盖端到端
// 形状）。假上游 httptest，Sleep 注入 no-op——退避节奏不在此验证范围。
package upstream

import (
	"io"
	"time"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type openFixture struct {
	tokenCalls     atomic.Int32
	invalidates    atomic.Int32
	limitedCalls   atomic.Int32
	okCalls        atomic.Int32
	proxyLookups   atomic.Int32
	upstreamStatus func(call int) (int, string) // 每次命中上游的状态码与 body
}

func newOpenClient(t *testing.T, fx *openFixture) *Client {
	t.Helper()
	c := New("http://placeholder", "/chat/completions", "2.1.0", "win",
		"100003", "testkey",
		func() (string, error) { fx.tokenCalls.Add(1); return "tok", nil },
		func() { fx.invalidates.Add(1) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Sleep = func(time.Duration) {}
	c.AccountLimited = func() { fx.limitedCalls.Add(1) }
	c.AccountOK = func() { fx.okCalls.Add(1) }
	c.ProxyFn = func() string { fx.proxyLookups.Add(1); return "" } // 空 = 直连
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(fx.tokenCalls.Load()) // 每次重试前 Token 都会重取，用它区分轮次
		code, body := fx.upstreamStatus(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c.Base = srv.URL
	return c
}

func openOnce(t *testing.T, c *Client) *http.Response {
	t.Helper()
	resp, err := c.Open("test-rid", map[string]any{"messages": []any{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return resp
}

// 首次 401 只重解凭据重试（不标记冷却）；第二次成功 → 200 + AccountOK。
func TestOpen401RekeysThenSucceeds(t *testing.T) {
	fx := &openFixture{upstreamStatus: func(call int) (int, string) {
		if call <= 1 {
			return 401, `{"message":"token rotated"}`
		}
		return 200, `{"choices":[]}`
	}}
	c := newOpenClient(t, fx)
	resp := openOnce(t, c)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if fx.invalidates.Load() < 1 {
		t.Errorf("401 后应 Invalidate")
	}
	if fx.limitedCalls.Load() != 0 {
		t.Errorf("首次 401 不应标记冷却，实际 %d 次", fx.limitedCalls.Load())
	}
	if fx.okCalls.Load() != 1 {
		t.Errorf("成功应清冷却（AccountOK），实际 %d 次", fx.okCalls.Load())
	}
	if fx.tokenCalls.Load() < 2 {
		t.Errorf("401 重试应重取 token，实际 %d 次", fx.tokenCalls.Load())
	}
}

// 持续 401：第一次不标记，之后的重试轮次标记；次数耗尽后最后一次的
// response 原样透传（body 完整，handler 负责归一）。
func TestOpenPersistent401MarksAndPassesThrough(t *testing.T) {
	fx := &openFixture{upstreamStatus: func(int) (int, string) {
		return 401, `{"message":"token expired"}`
	}}
	c := newOpenClient(t, fx)
	resp := openOnce(t, c)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("耗尽后应透传 401，实际 %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"message":"token expired"}` {
		t.Errorf("透传的 body 应完整，实际 %q", string(body))
	}
	if got := fx.limitedCalls.Load(); got != MaxAttempts-2 { // attempt1、attempt2 标记，attempt0 与耗尽轮不标
		t.Errorf("AccountLimited 应调 %d 次，实际 %d", MaxAttempts-2, got)
	}
}

// 429：从第一轮起就标记冷却换号，耗尽后透传 429。
func TestOpen429MarksImmediately(t *testing.T) {
	fx := &openFixture{upstreamStatus: func(int) (int, string) {
		return 429, `{"error":"rate limited"}`
	}}
	c := newOpenClient(t, fx)
	resp := openOnce(t, c)
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("耗尽后应透传 429，实际 %d", resp.StatusCode)
	}
	if got := fx.limitedCalls.Load(); got != MaxAttempts-1 { // 除耗尽轮外每轮重试前都标记
		t.Errorf("429 每轮都应标记冷却，应 %d 次，实际 %d", MaxAttempts-1, got)
	}
	if fx.invalidates.Load() != MaxAttempts-1 {
		t.Errorf("每轮重试前都应 Invalidate")
	}
}

// 200：AccountOK 清冷却，token 只取一次。
func TestOpenSuccessHappyPath(t *testing.T) {
	fx := &openFixture{upstreamStatus: func(int) (int, string) {
		return 200, `{"choices":[]}`
	}}
	c := newOpenClient(t, fx)
	resp := openOnce(t, c)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || fx.okCalls.Load() != 1 || fx.tokenCalls.Load() != 1 {
		t.Fatalf("status=%d ok=%d token=%d", resp.StatusCode, fx.okCalls.Load(), fx.tokenCalls.Load())
	}
}

// ProxyFn 返回空串必须走默认 client（直连路径不受代理接线影响）。
func TestOpenEmptyProxyKeepsDefaultClient(t *testing.T) {
	fx := &openFixture{upstreamStatus: func(int) (int, string) {
		return 200, `{"choices":[]}`
	}}
	c := newOpenClient(t, fx)
	if _, err := c.Open("rid-direct", map[string]any{}); err != nil {
		t.Fatalf("直连路径不应出错: %v", err)
	}
	if fx.proxyLookups.Load() == 0 {
		t.Errorf("ProxyFn 应被查询")
	}
}
