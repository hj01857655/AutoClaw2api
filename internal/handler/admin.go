// 管理面 API，对应 Python 版 server.py 的 admin_* 路由与 _account_token /
// _query_credits / _oauth_wait_worker。
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"autoclaw2api/internal/account"
	"autoclaw2api/internal/config"
	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/oauth"
)

// oauthFlowState 一次 OAuth 授权流程的实时状态（面板轮询 status 读它）。
type oauthFlowState struct {
	flow      *oauth.Flow
	provider  string
	url       string
	port      int
	state     string // waiting | ok | failed | cancelled
	err       string
	code      any
	cred      *oauth.Credential
	startedAt time.Time
	running   bool // 等待回调的 goroutine 还在跑
}

// ── 账号面 token ──────────────────────────────────────────────────────────

// accountToken 拿一个能打账号面的 token。续期（提前量/双检/落盘）全部由
// oauth.Store.Get() 自己负责。
func (s *Server) accountToken() (string, error) {
	if s.OAuth == nil {
		return "", errors.New("凭据源未配置")
	}
	return s.OAuth.Get()
}

// queryCredits 查积分余额，供 overview 带出去。
//
// **失败时返回 available=False，不返回 0**：「余额查不到」和「余额是 0」
// 对用户是两件事，混成 0 会让人以为号废了。
func (s *Server) queryCredits() map[string]any {
	token, err := s.accountToken()
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	bal, err := s.Account.WalletBalance(token)
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	if bal.Wallets == nil {
		bal.Wallets = []account.Wallet{}
	}
	payload := map[string]any{"available": true, "total": bal.Total, "wallets": bal.Wallets}
	// 过期积分：面板要在余额旁边标一句「3 天后有 N 分过期」。查询失败不影响余额展示。
	if token2, err := s.accountToken(); err == nil {
		if expiring, err := s.Account.PointsExpiring(token2); err == nil && expiring != nil {
			payload["expiring"] = expiring
		} else if err != nil {
			s.Log.Info("过期积分查询失败（不影响余额展示）", slog.String("err", err.Error()))
		}
	}
	return payload
}

// accountErrCode 上游业务码带不带出来（Python error.code）。
func accountErrCode(err error) any {
	if e, ok := err.(*account.Error); ok && e.HasCode {
		return e.Code
	}
	return nil
}

// ── 积分/流水/领取/任务 ──────────────────────────────────────────────────

func (s *Server) adminCredits(w http.ResponseWriter, r *http.Request) {
	credits := s.queryCredits()
	if avail, _ := credits["available"].(bool); !avail {
		msg, _ := credits["error"].(string)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "message": msg, "credits": credits})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": credits})
}

func (s *Server) adminLedgers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err1 := atoiOr(q.Get("page"), 1)
	size, err2 := atoiOr(q.Get("page_size"), 20)
	if err1 != nil || err2 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": "page / page_size 必须是数字"})
		return
	}
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	token, err := s.accountToken()
	if err == nil {
		var entries []account.Ledger
		entries, err = s.Account.CreditLedgers(token, page, size)
		if err == nil {
			if entries == nil {
				entries = []account.Ledger{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "page": page, "page_size": size, "entries": entries})
			return
		}
	}
	if _, ok := err.(*account.Error); ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": err.Error(), "code": accountErrCode(err)})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"ok": false, "message": err.Error()})
}

// atoiOr 解不出返回错误（Python int() 的 ValueError 语义），而不是静默 0。
func atoiOr(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n := 0
	if s[0] == '-' || s[0] == '+' {
		return 0, fmt.Errorf("非法数字 %q", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("非法数字 %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// taskIDValid 对应 Python 的 replace("_","").replace("-","").isalnum()。
func taskIDValid(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		alnum := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !alnum && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func (s *Server) adminClaim(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r)
	taskID := strings.TrimSpace(str(info.payload["task_id"]))
	if taskID == "" {
		// 缺失时退回每日签到——顶栏「领当日积分」按钮是快捷入口
		taskID = account.TaskDailySignin
	}
	if !taskIDValid(taskID) || len(taskID) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": "task_id 不合法"})
		return
	}
	token, err := s.accountToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "message": err.Error()})
		return
	}
	result, err := s.Account.ClaimTask(token, taskID)
	if err != nil {
		if _, ok := err.(*account.Error); ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "message": err.Error(), "code": accountErrCode(err)})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "message": err.Error()})
		return
	}
	message := "没领到，稍后再试"
	switch {
	case result.Completed:
		message = fmt.Sprintf("已领 %d 积分", result.Points)
	case result.Already:
		message = "今天已经领过了"
	case result.Rejected:
		// 这个任务不该在客户端领（invite_friends / upgrade_pc_app 都是这种）。
		// 不是失败，别报红；上游给的话就带上它的原话。
		message = result.Message
		if message == "" {
			message = "这个任务不支持在这里领取"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "task_id": taskID,
		"completed": result.Completed, "already": result.Already,
		"rejected": result.Rejected, "points": result.Points,
		"message": message, "credits": s.queryCredits(),
	})
}

func (s *Server) adminTasks(w http.ResponseWriter, r *http.Request) {
	token, err := s.accountToken()
	if err == nil {
		var tasks []account.Task
		tasks, err = s.Account.ListTasks(token)
		if err == nil {
			if tasks == nil {
				tasks = []account.Task{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
			return
		}
	}
	if _, ok := err.(*account.Error); ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": err.Error(), "code": accountErrCode(err)})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"ok": false, "message": err.Error()})
}

// ── overview ─────────────────────────────────────────────────────────────

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"valid": false, "account": "", "token_exp": "", "notice": "",
		"upstream": s.Cfg.UpstreamBase, "app_version": s.Cfg.AppVersion,
		"platform": s.Cfg.Platform}
	if s.OAuth != nil {
		// OAuth 模式：账号面自己换的 token，没有桌面端那个 account 目录概念
		token, err := s.OAuth.Get()
		if err != nil {
			info["notice"] = err.Error()
		} else {
			info["valid"] = true
			info["account"] = "oauth"
			info["token_exp"] = oauth.FmtExp(token)
			info["updatedAt"] = time.Now().Format("2006-01-02 15:04:05")
		}
	} else {
		info["notice"] = "凭据源未配置"
	}

	// 续期状态与 Python 版 _refresh_state 一致：只记录（排障日志用），不进响应。
	s.probeMu.Lock()
	probe := map[string]any{
		"done": s.probeData["done"], "total": s.probeData["total"],
		"unavailable": s.probeData["unavailable"], "at": s.probeData["at"],
		"results": s.probeData["results"],
	}
	s.probeMu.Unlock()

	models := s.ListModels()
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "AutoClaw2api", "credential": info,
		"cred_source": "oauth", "oauth_available": s.OAuth != nil,
		"credits": s.queryCredits(), "models": models, "probe": probe,
		"local_auth": s.Cfg.APIKey != "", "app_version": s.Cfg.AppVersion,
		"platform": s.Cfg.Platform,
		"constraints": []string{
			"请求体禁 max_tokens / max_completion_tokens / max_output_tokens（否则 406 空响应）",
			"system 必须含 2.x banner（否则 406 空响应）",
			"X-Request-Model 必填，且必须是上游模型 id（否则 400 非法模型）",
			"不发 X-Harness-Type（2.x 上 406）",
		},
	})
}

// ── OAuth 三件套 ─────────────────────────────────────────────────────────

// adminOAuthBegin 授权第一步：起 loopback、取授权链接。不阻塞——由后台
// goroutine 等回调，status 端点轮询结果。
func (s *Server) adminOAuthBegin(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "凭据源未配置（oauth Store 未注入）"})
		return
	}
	info := infoOf(r)
	provider := strings.ToLower(strings.TrimSpace(str(info.payload["provider"])))
	if provider == "" {
		provider = "zai"
	}
	if _, ok := oauth.OAuthHosts[provider]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "provider 只能是 google / zai"})
		return
	}
	s.oauthMu.Lock()
	if s.oauthFlow != nil && s.oauthFlow.running {
		s.oauthMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false,
			"message": "已有一个授权流程在进行中，先取消或等它完成"})
		return
	}
	s.oauthMu.Unlock()

	flow, err := s.OAuth.Flow(provider)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": err.Error(), "code": oauthErrCode(err)})
		return
	}
	url, err := flow.Begin()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": err.Error(), "code": oauthErrCode(err)})
		return
	}

	st := &oauthFlowState{flow: flow, provider: provider, url: url,
		port: flow.Port(), state: "waiting", startedAt: time.Now(), running: true}
	s.oauthMu.Lock()
	s.oauthFlow = st
	s.oauthMu.Unlock()

	go func() {
		defer func() {
			flow.Cancel() // 关掉 loopback，释放端口
			s.oauthMu.Lock()
			st.running = false
			s.oauthMu.Unlock()
		}()
		cred, err := flow.Wait(oauth.AttemptTimeoutS * time.Second)
		if err != nil {
			s.oauthMu.Lock()
			st.state, st.err, st.code = "failed", err.Error(), oauthErrCode(err)
			s.oauthMu.Unlock()
			s.Log.Warn("OAuth 授权失败", slog.String("err", err.Error()))
			return
		}
		s.oauthMu.Lock()
		st.state, st.cred = "ok", cred
		s.oauthMu.Unlock()
		account := cred.Account
		if account == "" {
			account = "?"
		}
		s.Log.Info("OAuth 授权完成",
			slog.String("account", firstN(account, 8)+"…"),
			slog.String("exp", oauth.FmtExp(cred.AccessToken)))
	}()

	s.Log.Info("OAuth 授权已开始",
		slog.String("provider", provider), slog.Int("port", flow.Port()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": url,
		"provider": provider, "port": flow.Port(), "expiresIn": oauth.AttemptTimeoutS})
}

func oauthErrCode(err error) any {
	if e, ok := err.(*oauth.Error); ok && e.HasCode {
		return e.Code
	}
	return nil
}

func (s *Server) adminOAuthStatus(w http.ResponseWriter, r *http.Request) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	st := s.oauthFlow
	if st == nil || st.state == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "idle"})
		return
	}
	payload := map[string]any{"ok": true, "state": st.state,
		"provider": st.provider, "url": st.url}
	if st.err != "" {
		payload["error"] = st.err
		payload["code"] = st.code
	}
	if st.cred != nil {
		payload["token_exp"] = oauth.FmtExp(st.cred.AccessToken)
		payload["has_refresh"] = st.cred.RefreshToken != ""
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) adminOAuthCancel(w http.ResponseWriter, r *http.Request) {
	s.oauthMu.Lock()
	if st := s.oauthFlow; st != nil {
		if st.flow != nil {
			st.flow.Cancel()
		}
		st.state = "cancelled"
	}
	s.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已取消"})
}

// ── 多账号管理（oauth 源）────────────────────────────────────────────────

// adminAccounts 落盘账号列表（含激活标记 / 到期 / refreshToken 有无）。
func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "凭据源未配置（oauth Store 未注入）"})
		return
	}
	accounts := s.OAuth.ListAccounts()
	if accounts == nil {
		accounts = []oauth.AccountInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": accounts})
}

// adminAccountSelect 切换激活账号：网关后续请求立刻走新号（含模型目录）。
func (s *Server) adminAccountSelect(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "凭据源未配置（oauth Store 未注入）"})
		return
	}
	name := str(infoOf(r).payload["account"])
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "缺少 account 参数"})
		return
	}
	if err := s.OAuth.SelectAccount(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false,
			"message": err.Error()})
		return
	}
	token, err := s.OAuth.Get()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false,
			"message": err.Error()})
		return
	}
	s.Log.Info("已切换 OAuth 账号", slog.String("account", name))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"message": "已切换到 " + name, "token_exp": oauth.FmtExp(token)})
}

// adminAccountImport 手动导入凭据（免浏览器授权）：手上已有 access/refresh
// token（比如外部登录脚本产出的）时直接入库，复用 SaveLogin 的落盘与切换语义。
// account 缺省时从 JWT payload 里解（uid / user_id / sub 依次试）。
func (s *Server) adminAccountImport(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "凭据源未配置（oauth Store 未注入）"})
		return
	}
	p := infoOf(r).payload
	token := str(p["accessToken"])
	refreshToken := str(p["refreshToken"])
	account := str(p["account"])
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "缺少 accessToken"})
		return
	}
	if account == "" {
		claims := oauth.JWTClaims(token)
		for _, k := range []string{"uid", "user_id", "sub"} {
			if v, ok := claims[k].(string); ok && v != "" {
				account = v
				break
			}
		}
	}
	if account == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "解不出账号名，请在 account 字段里指定"})
		return
	}
	s.OAuth.SaveLogin(&oauth.Credential{
		AccessToken: token, RefreshToken: refreshToken, Account: account,
	})
	s.Log.Info("已导入 OAuth 凭据", slog.String("account", account))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"account": account, "token_exp": oauth.FmtExp(token),
		"has_refresh": refreshToken != ""})
}

// adminAccountDelete 删除一个落盘账号的凭据（删激活账号 = 退出该号）。
func (s *Server) adminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "凭据源未配置（oauth Store 未注入）"})
		return
	}
	name := str(infoOf(r).payload["account"])
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "缺少 account 参数"})
		return
	}
	if err := s.OAuth.DeleteAccount(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false,
			"message": err.Error()})
		return
	}
	s.Log.Info("已删除 OAuth 账号凭据", slog.String("account", name))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"message": "已删除 " + name})
}

// ── 凭据重载 / 自检 / 探测 ───────────────────────────────────────────────

func (s *Server) adminRefreshToken(w http.ResponseWriter, r *http.Request) {
	if s.OAuth != nil {
		if infoOf(r).payload["force"] == true {
			// force：跳过「快到期才刷」的判断，立刻用 refreshToken 换新
			token, err := s.OAuth.ForceRefresh()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"ok": false, "message": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true,
				"message": "OAuth token 已强制刷新", "token_exp": oauth.FmtExp(token)})
			return
		}
		s.OAuth.Invalidate()
		token, err := s.OAuth.Get()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true,
			"message": "OAuth 凭据已重新载入", "token_exp": oauth.FmtExp(token)})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"ok": false, "message": "凭据源未配置"})
}

// adminSelfCheck 验证三条硬约束**此刻**是否真的生效：把「全字段污染的请求」
// 喂给 BuildRequest，检查出口。纯本地断言，不出网、无副作用。
func (s *Server) adminSelfCheck(w http.ResponseWriter, r *http.Request) {
	// 用上游目录里第一个真实模型名做样本：自检要验证的是「整形逻辑能否把脏
	// 请求洗干净」，不是某个特定名字能否解析（模型名本来就原样透传）。
	available := s.ListModels()
	sample := s.Cfg.DefaultModelName
	if len(available) > 0 {
		sample = available[0]
	}
	dirty := map[string]any{
		"model":                 sample,
		"max_tokens":            4096,
		"max_completion_tokens": 2048,
		"max_output_tokens":     1024,
		"frequency_penalty":     0.5,
		"stream":                false,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code. Answer directly."},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	headers, body, meta := lanes.BuildRequest(dirty, "TOKEN", s.Cfg.AppVersion,
		s.Cfg.Platform, config.AppID, s.Cfg.AppKey)

	firstMsg := ""
	if msgs := asList(body["messages"]); len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			firstMsg = str(m["content"])
		}
	}
	bannerOK := strings.Contains(firstMsg, lanes.Banner2X)
	hits := asStringList(meta["hits"])

	checks := []map[string]any{
		{"id": "no_max_tokens", "label": "① 出口无 max_tokens 族",
			"ok":     !hasAny(body, "max_tokens", "max_completion_tokens", "max_output_tokens"),
			"detail": "出口字段: " + strings.Join(sortedKeys(body), ", ")},
		{"id": "banner_present", "label": "② system 含 2.x banner",
			"ok":     bannerOK,
			"detail": firstN(firstMsg, 80)},
		{"id": "identity_neutralized", "label": "② 外来身份句已中和",
			"ok":     !strings.Contains(strings.ToLower(firstMsg), "claude code"),
			"detail": "命中: " + orNone(strings.Join(hits, ", "))},
		{"id": "request_model", "label": "③ X-Request-Model 已填",
			"ok":     headers["X-Request-Model"] != "",
			"detail": orNone(headers["X-Request-Model"])},
		{"id": "model_passthrough", "label": "③ 模型名原样透传（不本地改写）",
			"ok": headers["X-Request-Model"] == sample,
			"detail": fmt.Sprintf("body.model=%s → X-Request-Model=%s",
				sample, headers["X-Request-Model"])},
		{"id": "no_harness_type", "label": "未发 X-Harness-Type",
			"ok":     headers["X-Harness-Type"] == "",
			"detail": fmt.Sprintf("已发送 %d 个头", len(headers))},
		{"id": "auth_sign", "label": "X-Auth-Sign 用非空 appKey 计算",
			"ok":     s.Cfg.AppKey != "" && len(headers["X-Auth-Sign"]) == 32,
			"detail": fmt.Sprintf("appKey %d 位", len(s.Cfg.AppKey))},
	}
	passed := 0
	for _, c := range checks {
		if c["ok"] == true {
			passed++
		}
	}
	sampleHeaders := []string{}
	for k := range headers {
		if strings.HasPrefix(k, "X-") {
			sampleHeaders = append(sampleHeaders, k)
		}
	}
	sort.Strings(sampleHeaders)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": passed == len(checks), "checks": checks,
		"passed": passed, "total": len(checks), "sample_headers": sampleHeaders,
	})
}

// adminProbeModels 逐模型发一个极小请求，实测哪些真能调。「目录里有」不等于
// 「现在能调」——pay-view 是分钟级波动，只有实调能区分。
func (s *Server) adminProbeModels(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r)
	var models []string
	for _, m := range asList(info.payload["models"]) {
		if v := str(m); v != "" {
			models = append(models, v)
		}
	}
	if len(models) == 0 {
		models = s.ListModels()
	}
	token, err := s.token()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "message": err.Error()})
		return
	}

	results := []any{}
	for _, mid := range models {
		entry := map[string]any{"id": mid, "ok": false, "status": 0, "note": ""}
		payload := map[string]any{"model": mid, "max_tokens": 8,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		headers, body, _ := lanes.BuildRequest(payload, token, s.Cfg.AppVersion,
			s.Cfg.Platform, config.AppID, s.Cfg.AppKey)
		raw, _ := json.Marshal(body)
		req, rerr := http.NewRequest(http.MethodPost, s.Cfg.UpstreamBase+config.UpstreamPath,
			bytes.NewReader(raw))
		if rerr != nil {
			entry["note"] = "RequestException: " + rerr.Error()
			results = append(results, entry)
			continue
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, derr := s.Up.HTTP.Do(req)
		if derr != nil {
			entry["note"] = "RequestException: " + derr.Error()
			results = append(results, entry)
			s.Log.Info("[probe]",
				slog.String("line", fmt.Sprintf("%s -> %d %s", mid, 0, entry["note"])))
			continue
		}
		respRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		text := string(respRaw)
		entry["status"] = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			entry["ok"] = true
			entry["note"] = "可调"
		} else {
			peek := text
			if len(peek) > 300 {
				peek = peek[:300]
			}
			entry["note"] = lanes.ClassifyUpstreamError(resp.StatusCode, peek)
			if strings.Contains(text, "pay-view") || strings.Contains(text, "810001") {
				entry["note"] = "账号配额暂不可用（pay-view，分钟级波动）"
			} else if resp.StatusCode == 400 && strings.Contains(text, "非法模型") {
				entry["note"] = "上游拒绝该模型 id"
			}
		}
		results = append(results, entry)
		s.Log.Info("[probe]",
			slog.String("line", fmt.Sprintf("%s -> %d %s", mid, resp.StatusCode, entry["note"])))
	}

	okCount := 0
	for _, re := range results {
		if m, _ := re.(map[string]any); m != nil && m["ok"] == true {
			okCount++
		}
	}
	s.probeMu.Lock()
	s.probeData = map[string]any{
		"done": len(results), "total": len(results),
		"unavailable": len(results) - okCount,
		"at":          time.Now().Format("2006-01-02 15:04:05"),
		"results":     results,
	}
	s.probeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(results), "callable": okCount,
		"unavailable": len(results) - okCount, "results": results,
		"message": fmt.Sprintf("探测 %d 个模型，%d 个可调", len(results), okCount),
	})
}

// ── 小工具 ───────────────────────────────────────────────────────────────

func hasAny(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func asStringList(v any) []string {
	if l, ok := v.([]string); ok {
		return l
	}
	if l, ok := v.([]any); ok {
		out := make([]string, 0, len(l))
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
