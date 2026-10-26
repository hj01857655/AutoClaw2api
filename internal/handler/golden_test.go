// handler 层的 httptest 假上游金样本测试：三协议路由的请求整形抽查、
// 聚合与 SSE 收尾帧序、上游错误归一、鉴权闸门、自检。
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autoclaw2api/internal/account"
	"autoclaw2api/internal/config"
	"autoclaw2api/internal/dump"
	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/oauth"
	"autoclaw2api/internal/upstream"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type testEnv struct {
	srv      *Server
	upstream *httptest.Server
	inval    atomic.Int32
}

// newTestServer 起一个假上游 + 装好依赖的 Server。upstreamHandler 每次收到
// POST /chat/completions 都会被调用。
func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) *testEnv {
	t.Helper()
	logger := discardLogger()
	cfg := &config.Config{
		Host: "127.0.0.1", Port: 0, StateDir: t.TempDir(),
		DumpOn: false, AppVersion: config.DefaultAppVersion, Platform: "win",
		AppKey:           config.DefaultAppKey,
		DefaultModelName: config.DefaultModel,
	}
	upSrv := httptest.NewServer(upstreamHandler)
	t.Cleanup(upSrv.Close)
	cfg.UpstreamBase = upSrv.URL + "/autoclaw-proxy/proxy/autoclaw"

	env := &testEnv{srv: New(cfg, logger, dump.New(false, t.TempDir(), 10), nil), upstream: upSrv}
	env.srv.Up = &upstream.Client{
		Base: cfg.UpstreamBase, Path: config.UpstreamPath,
		Version: cfg.AppVersion, Platform: cfg.Platform,
		AppID: config.AppID, AppKey: cfg.AppKey,
		HTTP:       upSrv.Client(),
		Token:      func() (string, error) { return "TESTTOKEN", nil },
		Log:        logger,
		Sleep:      func(time.Duration) {},
		Invalidate: func() { env.inval.Add(1) },
	}
	// oauth Store 注入假 DPAPI（xor 可逆）与假凭据：health/面板读 Cached 展示账号
	oauthStore := oauth.NewStore(t.TempDir(), "http://127.0.0.1:1", "http://127.0.0.1:1",
		cfg.AppVersion, "win", config.AppID, cfg.AppKey, nil, 0, logger)
	oauthStore.SetDPAPI(
		func(b []byte, _ string) ([]byte, error) { return xor5a(b), nil },
		func(b []byte) ([]byte, error) { return xor5a(b), nil },
	)
	oauthStore.SaveLogin(&oauth.Credential{
		AccessToken: "TESTTOKEN", RefreshToken: "rt", Account: "abcdef1234567890",
	})
	env.srv.OAuth = oauthStore
	env.srv.Account = account.New("http://127.0.0.1:1", cfg.AppVersion, "win",
		config.AppID, cfg.AppKey)
	return env
}

// checkShapedRequest 假上游侧断言：406 三条硬约束 + 身份头确实落实。
func checkShapedRequest(t *testing.T, r *http.Request, raw []byte, wantModel string) {
	t.Helper()
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		t.Errorf("上游路径=%s", r.URL.Path)
	}
	if r.Header.Get("Authorization") != "TESTTOKEN" {
		t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("X-Request-Model") != wantModel {
		t.Errorf("X-Request-Model=%q，期望 %q", r.Header.Get("X-Request-Model"), wantModel)
	}
	if r.Header.Get("X-Harness-Type") != "" {
		t.Errorf("不应发 X-Harness-Type")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	for _, f := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if _, ok := body[f]; ok {
			t.Errorf("请求体带了禁字段 %s", f)
		}
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("请求体没有 messages")
	}
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if !strings.Contains(content, lanes.Banner2X) {
		t.Errorf("messages[0] 缺 2.x banner: %q", content)
	}
}

func postJSON(t *testing.T, env *testEnv, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://gateway"+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	return rec.Result()
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("响应不是 JSON（%d）: %s", resp.StatusCode, raw)
	}
	return m
}

// ── 鉴权 ─────────────────────────────────────────────────────────────────

func TestUnauthorized(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("未鉴权请求不应到达上游")
	})
	env.srv.Cfg.APIKey = "secret"
	req, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d，期望 401", rec.Code)
	}
	body := decode(t, rec.Result())
	errObj := body["error"].(map[string]any)
	if errObj["type"] != "auth_error" || errObj["message"] != "invalid api key" {
		t.Errorf("错误体=%v", errObj)
	}

	req2, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	env.srv.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("正确 key status=%d", rec2.Code)
	}
}

// ── chat：非流式 ─────────────────────────────────────────────────────────

func TestChatNonStream(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		checkShapedRequest(t, r, raw, "zai_auto")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant",
			"content":"hi","reasoning_content":"th"},"finish_reason":"stop"}],
			"usage":{"input_tokens":7,"output_tokens":3}}`)
	})
	resp := postJSON(t, env, "/v1/chat/completions",
		`{"model":"zai_auto","messages":[{"role":"user","content":"yo"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["object"] != "chat.completion" {
		t.Errorf("object=%v", body["object"])
	}
	if !strings.HasPrefix(body["id"].(string), "chatcmpl-") {
		t.Errorf("id=%v", body["id"])
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi" || msg["reasoning_content"] != "th" {
		t.Errorf("message=%v", msg)
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 7 || usage["completion_tokens"].(float64) != 3 {
		t.Errorf("usage 未归一: %v", usage)
	}
}

// ── chat：流式 ───────────────────────────────────────────────────────────

const streamFixture = "data: " + `{"choices":[{"delta":{"content":"he"}}]}` + "\n\n" +
	"data: " + `{"event":"zwork_queue","zwork_queue":{"position":2}}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"reasoning_content":"thin"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_","arguments":"{\"a\""}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"input_tokens":10,"output_tokens":5}}` + "\n\n" +
	"data: [DONE]\n\n"

func sseUpstream(t *testing.T, wantModel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		checkShapedRequest(t, r, raw, wantModel)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamFixture)
	}
}

func frames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, chunk := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(chunk, "data: ") {
			continue
		}
		data := strings.TrimPrefix(chunk, "data: ")
		if data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("帧不是 JSON: %q", data)
		}
		out = append(out, m)
	}
	return out
}

func TestChatStream(t *testing.T) {
	env := newTestServer(t, sseUpstream(t, "zai_auto"))
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zai_auto","stream":true,
			"messages":[{"role":"user","content":"yo"}]}`))
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, no-transform" {
		t.Errorf("Cache-Control=%q", cc)
	}
	fs := frames(t, rec.Body.String())
	if len(fs) != 4 {
		t.Fatalf("帧数=%d，期望 4（role+content / reasoning / tool_calls 收尾 / tail）\n%s",
			len(fs), rec.Body.String())
	}
	d0 := fs[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d0["role"] != "assistant" || d0["content"] != "he" {
		t.Errorf("首帧 delta=%v，期望合并 role", d0)
	}
	d1 := fs[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d1["reasoning_content"] != "thin" {
		t.Errorf("reasoning 帧=%v", d1)
	}
	// tool_calls 分片按 index 拼回，且只在收尾一次性下发
	d2 := fs[2]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	tcs := d2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%v", tcs)
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Errorf("tool_call=%v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_" || fn["arguments"] != `{"a":1}` {
		t.Errorf("function 分片未拼回: %v", fn)
	}
	// tail 帧：finish_reason 补 tool_calls，usage 挂在 tail 上（未开 include_usage）
	tail := fs[3]["choices"].([]any)[0].(map[string]any)
	if tail["finish_reason"] != "tool_calls" {
		t.Errorf("tail finish_reason=%v", tail["finish_reason"])
	}
	if u := fs[3]["usage"].(map[string]any); u["prompt_tokens"].(float64) != 10 ||
		u["completion_tokens"].(float64) != 5 {
		t.Errorf("tail usage=%v", u)
	}
	// [DONE] 收尾
	if !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "data: [DONE]") {
		t.Errorf("缺 [DONE] 收尾:\n%s", rec.Body.String())
	}
}

// include_usage=true：usage 从 tail 挪到独立的末帧
func TestChatStreamIncludeUsage(t *testing.T) {
	env := newTestServer(t, sseUpstream(t, "zai_auto"))
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zai_auto","stream":true,
			"stream_options":{"include_usage":true},
			"messages":[{"role":"user","content":"yo"}]}`))
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	fs := frames(t, rec.Body.String())
	if len(fs) != 5 {
		t.Fatalf("帧数=%d，期望 5（多一帧独立 usage）\n%s", len(fs), rec.Body.String())
	}
	if _, has := fs[3]["usage"]; has {
		t.Errorf("tail 不应带 usage: %v", fs[3])
	}
	if u := fs[4]["usage"].(map[string]any); u["total_tokens"].(float64) != 0 {
		t.Errorf("usage 帧=%v", u)
	}
}

// ── 请求校验与上游错误 ───────────────────────────────────────────────────

func TestChatValidation(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("非法请求不应到达上游")
	})
	for _, body := range []string{`{}`, `not json`, `{"messages":[]}`} {
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(body))
		env.srv.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("body=%q status=%d，期望 400", body, rec.Code)
		}
		errObj := decode(t, rec.Result())["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("错误体=%v", errObj)
		}
	}
}

func TestUpstreamError401(t *testing.T) {
	var hits atomic.Int32
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"message":"token expired"}`)
	})
	resp := postJSON(t, env, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"yo"}]}`)
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	errObj := decode(t, resp)["error"].(map[string]any)
	if errObj["type"] != "authentication_error" {
		t.Errorf("type=%v", errObj["type"])
	}
	if errObj["message"] != "token expired" {
		t.Errorf("message=%v", errObj["message"])
	}
	// upstream.Open 的 401 自愈：重试前 invalidate
	if env.inval.Load() < 1 {
		t.Errorf("Invalidate 未被调用")
	}
}

func TestUpstreamError406EmptyBody(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(406) // 空 body
	})
	resp := postJSON(t, env, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"yo"}]}`)
	// 空 body → 502 + 请求形状提示
	if resp.StatusCode != 502 {
		t.Fatalf("status=%d，期望 502", resp.StatusCode)
	}
	errObj := decode(t, resp)["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("type=%v", errObj["type"])
	}
	if !strings.Contains(errObj["message"].(string), "406") {
		t.Errorf("message=%v", errObj["message"])
	}
}

func TestUpstreamErrorPayView(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"code":810001,"action":"pay-view","message":"quota"}`)
	})
	resp := postJSON(t, env, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"yo"}]}`)
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	errObj := decode(t, resp)["error"].(map[string]any)
	// pay-view 的 code/action 原样透出
	if errObj["code"].(float64) != 810001 || errObj["action"] != "pay-view" {
		t.Errorf("code/action 未透出: %v", errObj)
	}
}

// ── anthropic / responses ────────────────────────────────────────────────

func TestAnthropicStream(t *testing.T) {
	env := newTestServer(t, sseUpstream(t, "claude-x"))
	// anthropic 请求带 max_tokens（协议必填）——翻译层出口必须删掉
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-x","max_tokens":100,"stream":true,
			"messages":[{"role":"user","content":"yo"}]}`))
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") ||
		!strings.Contains(body, "event: content_block_delta") {
		t.Errorf("缺 Anthropic 关键事件:\n%s", body[:min(len(body), 500)])
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("缺 message_stop 收尾:\n%s", body)
	}
}

func TestAnthropicValidation(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("非法请求不应到达上游")
	})
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`[1,2,3]`))
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d", rec.Code)
	}
	body := decode(t, rec.Result())
	if body["type"] != "error" {
		t.Errorf("Anthropic 错误体形状=%v", body)
	}
}

func TestResponsesValidation(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("非法请求不应到达上游")
	})
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":[]}`))
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d", rec.Code)
	}
	errObj := decode(t, rec.Result())["error"].(map[string]any)
	if errObj["message"] != "invalid request: 'input' is required" {
		t.Errorf("message=%v", errObj["message"])
	}
}

// ── health / admin ───────────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("/health 不应出网")
	})
	req, _ := http.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	body := decode(t, rec.Result())
	if body["ok"] != true {
		t.Fatalf("ok=%v notice=%v", body["ok"], body["notice"])
	}
	if body["account"] != "abcdef123456" {
		t.Errorf("account=%v，期望截 12 位", body["account"])
	}
	if body["token_exp"] != "?" {
		t.Errorf("token_exp=%v（TESTTOKEN 非 JWT）", body["token_exp"])
	}
	models := body["models"].([]any)
	if len(models) != 4 {
		t.Errorf("models=%v，期望兜底清单 4 个", models)
	}
}

func TestSelfCheck(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("自检不应出网")
	})
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/self-check", nil)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec.Result())
	if body["ok"] != true || body["total"].(float64) != 7 || body["passed"].(float64) != 7 {
		for _, c := range body["checks"].([]any) {
			item := c.(map[string]any)
			if item["ok"] != true {
				t.Errorf("自检项 %v 失败: %v", item["id"], item["detail"])
			}
		}
		t.Fatalf("ok=%v passed=%v", body["ok"], body["passed"])
	}
}

func TestOAuthBeginWithoutStore(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/oauth/begin",
		strings.NewReader(`{"provider":"zai"}`))
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(decode(t, rec.Result())["message"].(string), "oauth") {
		t.Errorf("message=%v", rec.Body.String())
	}
}

func TestOAuthStatusIdle(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/oauth/status", nil)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	body := decode(t, rec.Result())
	if body["state"] != "idle" {
		t.Errorf("state=%v", body["state"])
	}
}

// probe：逐模型实调假上游
func TestProbeModels(t *testing.T) {
	var hits atomic.Int32
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		// SanitizeBody 会剥 body.model 的 zai_ 前缀（X-Request-Model 才是完整 id）
		if body["model"] == "auto-fast" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `pay-view`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/probe-models",
		strings.NewReader(`{"models":["zai_auto","zai_auto-fast"]}`))
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec.Result())
	if body["total"].(float64) != 2 || body["callable"].(float64) != 1 {
		t.Fatalf("total=%v callable=%v", body["total"], body["callable"])
	}
	rs := body["results"].([]any)
	if rs[0].(map[string]any)["ok"] != true {
		t.Errorf("zai_auto 应可调: %v", rs[0])
	}
	note := rs[1].(map[string]any)["note"].(string)
	if !strings.Contains(note, "pay-view") {
		t.Errorf("403 应归类为 pay-view: %v", note)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// xor5a 测试用可逆"加密"（假 DPAPI）。
func xor5a(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0x5a
	}
	return out
}

// 导入端点：不带 refresh 的裸 token 也能入库并立即生效。
func TestAccountImport(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	body := `{"accessToken":"a.b.c","account":"imported12345678"}`
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/accounts/import",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("导入应成功: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"account":"imported12345678"`) {
		t.Fatalf("响应应带回账号名: %s", rec.Body.String())
	}
	// 落盘后列表里可见且是激活账号
	rec2 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil))
	if !strings.Contains(rec2.Body.String(), "imported1234") {
		t.Fatalf("列表应包含导入的账号: %s", rec2.Body.String())
	}
}

// 导入缺 accessToken → 400。
func TestAccountImportMissingToken(t *testing.T) {
	env := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/admin/api/accounts/import", strings.NewReader(`{}`)))
	if rec.Code != 400 {
		t.Fatalf("缺 accessToken 应 400: %d", rec.Code)
	}
}
