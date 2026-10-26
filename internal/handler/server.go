// Package handler 对应 Python 版 server.py + routes/：路由调度、鉴权、rid、
// in:/done: 日志、请求落盘、上游错误归一、聚合与 SSE 产出。协议形状在
// adapters 与 lanes 包里。
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"autoclaw2api/internal/account"
	"autoclaw2api/internal/config"
	"autoclaw2api/internal/dump"
	"autoclaw2api/internal/oauth"
	"autoclaw2api/internal/upstream"
)

// Server 聚合全部依赖；Handler() 挂到 http.ServeMux。
type Server struct {
	Cfg  *config.Config
	Dump *dump.Writer
	Log  *slog.Logger

	// ── 运行期依赖（main 注入；测试注入假通道）──
	Up      *upstream.Client
	OAuth   *oauth.Store // oauth 凭据源（唯一通道，独立网关不读桌面端）
	Account *account.Client

	// 进行中的 OAuth 授权流程（begin 只起流程存句柄，后台 goroutine 等回调）
	oauthMu   sync.Mutex
	oauthFlow *oauthFlowState

	// 模型目录缓存：上游每次拉都算一次出站，缓存 10 分钟，失败用兜底清单
	modelsTTL   time.Duration
	modelsMu    sync.Mutex
	modelsCache []string
	modelsAt    time.Time
	fetchModels func() []string
	Fallback    []string

	// 逐模型探测结果缓存（探测要出网且花几秒，只在用户点按钮时跑）
	probeMu   sync.Mutex
	probeData map[string]any

	reqSeq   int64
	reqMu    sync.Mutex
	bootTag  string
	authFail int64
}

// New 构造调度层。fetchModels 注入 lanes.FetchModels（测试注入假上游）。
func New(cfg *config.Config, log *slog.Logger, d *dump.Writer,
	fetchModels func() []string) *Server {
	return &Server{
		Cfg: cfg, Log: log, Dump: d,
		modelsTTL:   600 * time.Second,
		fetchModels: fetchModels,
		Fallback:    []string{"zai_auto", "zai_auto-fast", "zaicoding_glm-5.3", "zai_glm-5.3-flash"},
		bootTag:     fmt.Sprintf("%s-p%d", time.Now().Format("0102-150405"), os.Getpid()),
		probeData: map[string]any{"done": 0, "total": 0, "unavailable": 0,
			"at": "", "results": []any{}},
	}
}

// ── 请求上下文 ─────────────────────────────────────────────────────────────

type reqInfo struct {
	rid       string
	t0        time.Time
	payload   map[string]any
	rawBody   []byte
	upstreamC map[string]any // 上游 401 自愈用的小工具位（未使用，留作可读性）
}

type reqInfoKey struct{}

func withReqInfo(ctx context.Context, info *reqInfo) context.Context {
	return context.WithValue(ctx, reqInfoKey{}, info)
}

func infoOf(r *http.Request) *reqInfo {
	info, _ := r.Context().Value(reqInfoKey{}).(*reqInfo)
	if info == nil {
		info = &reqInfo{rid: "?", t0: time.Now()}
	}
	return info
}

func (s *Server) nextRID() string {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	s.reqSeq++
	return fmt.Sprintf("r%04d", s.reqSeq)
}

// logQuietPaths 这些路径不进 in:/done: 日志（面板轮询会刷满屏）。
var logQuietPaths = map[string]bool{
	"/": true, "/admin": true, "/health": true, "/favicon.ico": true,
}

// noDumpPaths 请求体不落 dumps/ 的路径——短信登录的 body 带明文手机号与
// 验证码，落盘就是泄露（2026-10-27 实锤后修复：dumps 里抓到过明文）。
var noDumpPaths = map[string]bool{
	"/admin/api/oauth/sms/send":   true,
	"/admin/api/oauth/sms/verify": true,
	"/admin/api/login-cn/send":    true,
	"/admin/api/login-cn/verify":  true,
}

// middleware 鉴权 + rid + in: 日志 + req 落盘 + CORS（对应 before/after_request）。
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := s.nextRID()
		t0 := time.Now()

		// OPTIONS 预检直接放行（Python 版每个路由都先处理 OPTIONS）
		if r.Method == http.MethodOptions {
			s.cors(w)
			w.WriteHeader(http.StatusOK)
			return
		}

		// 鉴权闸门：未设 API_KEY 时仅监听回环、不校验；设了就要求 Bearer 匹配
		if !s.authorized(r) {
			s.cors(w)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "invalid api key", "type": "auth_error"},
			})
			return
		}

		var payload map[string]any
		var rawBody []byte
		if r.Body != nil { // 直连 ServeHTTP 的测试请求 GET 时 Body 为 nil
			rawBody, _ = io.ReadAll(io.LimitReader(r.Body, 64<<20))
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(rawBody))
			if len(rawBody) > 0 {
				_ = json.Unmarshal(rawBody, &payload)
			}
		}

		info := &reqInfo{rid: rid, t0: t0, payload: payload, rawBody: rawBody}
		if !logQuietPaths[r.URL.Path] {
			s.logIn(rid, r, payload, len(rawBody))
			if !noDumpPaths[r.URL.Path] {
				s.Dump.Save(rid, "req", string(rawBody))
			}
		}

		rec := &recorder{ResponseWriter: w, status: 200}
		s.cors(w)
		next.ServeHTTP(rec, r.WithContext(withReqInfo(r.Context(), info)))
		if rec.wroteHeader {
			s.cors(rec)
		}

		if logQuietPaths[r.URL.Path] || rec.streamed {
			return
		}
		s.Log.Info("done:",
			slog.String("rid", rid),
			slog.String("line", fmt.Sprintf("status=%d %dB %.1fs",
				rec.status, rec.body.Len(), time.Since(t0).Seconds())),
		)
		s.Dump.Save(rid, "resp", rec.body.String())
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Cfg.APIKey == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.Cfg.APIKey
}

func (s *Server) cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func (s *Server) logIn(rid string, r *http.Request, payload map[string]any, bodyLen int) {
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		ua = "-"
	}
	ua = strings.ReplaceAll(strings.ReplaceAll(ua, "\r", " "), "\n", " ")
	msgs, tools := len(asList(payload["messages"])), len(asList(payload["tools"]))
	summary := fmt.Sprintf("model=%s msgs=%d tools=%d stream=%v",
		str(payload["model"]), msgs, tools, payload["stream"])
	s.Log.Info("in:",
		slog.String("rid", rid),
		slog.String("line", fmt.Sprintf("%s %s | %s | ua=%s | body=%dB",
			r.Method, r.URL.Path, orNone(summary), ua, bodyLen)),
	)
}

func orNone(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

// ── 路由表 ─────────────────────────────────────────────────────────────────

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handlePanel) // 含 / 与 /admin
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/messages", s.handleAnthropic)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/admin/api/overview", s.adminOverview)
	mux.HandleFunc("/admin/api/credits", s.adminCredits)
	mux.HandleFunc("/admin/api/ledgers", s.adminLedgers)
	mux.HandleFunc("/admin/api/claim", s.adminClaim)
	mux.HandleFunc("/admin/api/tasks", s.adminTasks)
	mux.HandleFunc("/admin/api/oauth/begin", s.adminOAuthBegin)
	mux.HandleFunc("/admin/api/oauth/status", s.adminOAuthStatus)
	mux.HandleFunc("/admin/api/oauth/cancel", s.adminOAuthCancel)
	mux.HandleFunc("/admin/api/oauth/sms/send", s.adminSmsSend)
	mux.HandleFunc("/admin/api/oauth/sms/verify", s.adminSmsVerify)
	mux.HandleFunc("/admin/api/accounts", s.adminAccounts)
	mux.HandleFunc("/admin/api/accounts/select", s.adminAccountSelect)
	mux.HandleFunc("/admin/api/accounts/delete", s.adminAccountDelete)
	mux.HandleFunc("/admin/api/accounts/import", s.adminAccountImport)
	mux.HandleFunc("/admin/api/login-cn/send", s.adminCNSendCode)
	mux.HandleFunc("/admin/api/login-cn/verify", s.adminCNVerify)
	mux.HandleFunc("/admin/api/refresh-token", s.adminRefreshToken)
	mux.HandleFunc("/admin/api/self-check", s.adminSelfCheck)
	mux.HandleFunc("/admin/api/probe-models", s.adminProbeModels)
	return s.middleware(mux)
}

// ── 模型目录 ───────────────────────────────────────────────────────────────

// ListModels 带 TTL 的模型目录；拉不到用兜底清单。
//
// 兜底清单只影响 /v1/models 的显示内容，**不参与路由裁决**（模型名直接透传
// 给上游，上游是唯一裁决者）。
func (s *Server) ListModels() []string {
	now := time.Now()
	s.modelsMu.Lock()
	if len(s.modelsCache) > 0 && now.Sub(s.modelsAt) < s.modelsTTL {
		ids := s.modelsCache
		s.modelsMu.Unlock()
		return ids
	}
	s.modelsMu.Unlock()

	var ids []string
	if s.fetchModels != nil {
		ids = s.fetchModels()
	}
	if len(ids) == 0 {
		ids = append([]string{}, s.Fallback...)
		s.Log.Info("模型目录用兜底清单", slog.String("ids", strings.Join(ids, ", ")))
	}
	s.modelsMu.Lock()
	s.modelsCache = ids
	s.modelsAt = now
	s.modelsMu.Unlock()
	return ids
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ids := s.ListModels()
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id": id, "object": "model", "created": 0, "owned_by": "autoclaw",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ── 面板与健康 ─────────────────────────────────────────────────────────────

func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	path := s.Cfg.StateFilePath("panel.html")
	raw, err := os.ReadFile(path)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("AutoClaw2api running. (panel.html not installed)"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(raw)
}

// ── 响应辅助 ───────────────────────────────────────────────────────────────

// recorder 记录状态码与 body（非流式），并标记是否流式。
type recorder struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
	streamed    bool
}

func (rec *recorder) WriteHeader(code int) {
	rec.status = code
	rec.wroteHeader = true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(p []byte) (int, error) {
	if rec.body.Len()+len(p) <= 1<<20 { // done: 行只统计前 1MB，够排障
		rec.body.Write(p)
	}
	return rec.ResponseWriter.Write(p)
}

// Flush 透出 http.Flusher，供 SSE 逐帧下发；一旦用它就标记为流式（不进 done:）。
func (rec *recorder) Flush() {
	rec.streamed = true
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body := marshalCompact(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSONRaw 透出「已经构造好的 JSON」，形状与 Python jsonify 一致。
func marshalCompact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// ── 日志：request.log 2MB×3 轮转 + stdout（与 Python RotatingFileHandler 对齐）──

// NewLogger 建 slog logger：同时写 stdout 与 request.log（2MB 轮转，保 3 份）。
func NewLogger(stateDir string) *slog.Logger {
	path := filepath.Join(stateDir, "request.log")
	rw := &rotateWriter{path: path, maxBytes: 2 << 20, backupCount: 3}
	return slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, rw), &slog.HandlerOptions{
		// 与 Python "%(asctime)s %(levelname)s %(message)s" 同量级的信息密度
		Level: slog.LevelInfo,
	}))
}

type rotateWriter struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	backupCount int
	f           *os.File
	written     int64
}

func (rw *rotateWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f == nil {
		if err := rw.open(); err != nil {
			return 0, err
		}
	}
	if rw.written+int64(len(p)) > rw.maxBytes {
		rw.rotate()
	}
	n, err := rw.f.Write(p)
	rw.written += int64(n)
	return n, err
}

func (rw *rotateWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(rw.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	rw.f = f
	if info, err := f.Stat(); err == nil {
		rw.written = info.Size()
	}
	return nil
}

// rotate 关闭当前文件，.log.(n-1) → .log.n 链式改名，重开新文件。
// 命名与 Python RotatingFileHandler 一致（request.log.1 最新）。
func (rw *rotateWriter) rotate() {
	if rw.f != nil {
		rw.f.Close()
		rw.f = nil
	}
	for i := rw.backupCount - 1; i > 0; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", rw.path, i), fmt.Sprintf("%s.%d", rw.path, i+1))
	}
	_ = os.Rename(rw.path, rw.path+".1")
	rw.written = 0
	_ = rw.open()
}

// serveHTTP 供 main 与测试复用：绑定在任意 listener 上。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}
