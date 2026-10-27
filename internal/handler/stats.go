// 请求统计：按模型聚合请求数 / 成功率 / token 消耗 / 耗时，内存态
// （重启清零——排障口径够用，持久化报表等有真实需求再做）。
// 数据源是 middleware 的收尾点：流式与非流式都从 recorder 拿到的 body
// 里抽 usage.total_tokens（JSON 响应与 SSE usage 帧都带这个键）。
package handler

import (
	"net/http"
	"regexp"
	"sort"
	"time"
)

// statAgg 单模型聚合项。
type statAgg struct {
	Requests int64 `json:"requests"`
	OK       int64 `json:"ok"`
	Fail     int64 `json:"fail"`
	Tokens   int64 `json:"tokens"`
	TotalMs  int64 `json:"-"`
}

// recordStat 收尾埋点。body 是响应累积（done: 口径，前 1MB）——流式时是
// SSE 帧、非流式是 JSON，两种形状都带 "total_tokens"，取最后一帧的值。
func (s *Server) recordStat(model string, status int, took time.Duration, body string) {
	if model == "" {
		model = "-"
	}
	tokens := int64(lastTotalTokens(body))
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.stats == nil {
		s.stats = map[string]*statAgg{}
		s.statsSince = time.Now()
	}
	a := s.stats[model]
	if a == nil {
		a = &statAgg{}
		s.stats[model] = a
	}
	a.Requests++
	if status == http.StatusOK {
		a.OK++
		s.tokensTotal += tokens
	} else {
		a.Fail++
	}
	a.Tokens += tokens
	a.TotalMs += took.Milliseconds()
}

// totalTokensRe 抓 total_tokens（JSON 里键值相邻；SSE 帧 data: 前缀不影响）。
var totalTokensRe = regexp.MustCompile(`"total_tokens"\s*:\s*(\d+)`)

func lastTotalTokens(body string) int {
	ms := totalTokensRe.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		return 0
	}
	n := 0
	for _, c := range ms[len(ms)-1][1] {
		n = n*10 + int(c-'0')
	}
	return n
}

// StatsSnapshot 面板报表输出。
type StatsSnapshot struct {
	Since   string           `json:"since"`
	Total   statAggView      `json:"total"`
	Models  []statAggView    `json:"models"`
	Cooldown []map[string]any `json:"cooldown"`
}

type statAggView struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	OK       int64   `json:"ok"`
	Fail     int64   `json:"fail"`
	Tokens   int64   `json:"tokens"`
	AvgMs    float64 `json:"avg_ms"`
}

func (s *Server) snapshotStats() StatsSnapshot {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	// 没有任何请求时 Since 留空（零值时间戳面板没法看），前端兜底 "—"
	out := StatsSnapshot{}
	if !s.statsSince.IsZero() {
		out.Since = s.statsSince.Format("2006-01-02 15:04:05")
	}
	models := []statAggView{}
	var total statAgg
	for m, a := range s.stats {
		models = append(models, statAggView{
			Model: m, Requests: a.Requests, OK: a.OK, Fail: a.Fail,
			Tokens: a.Tokens, AvgMs: avgMs(a),
		})
		total.Requests += a.Requests
		total.OK += a.OK
		total.Fail += a.Fail
		total.Tokens += a.Tokens
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Requests > models[j].Requests })
	out.Models = models
	out.Total = statAggView{Model: "ALL", Requests: total.Requests, OK: total.OK,
		Fail: total.Fail, Tokens: total.Tokens, AvgMs: avgMs(&total)}
	out.Cooldown = s.OAuth.CooldownStatus()
	return out
}

func avgMs(a *statAgg) float64 {
	if a.Requests == 0 {
		return 0
	}
	return float64(a.TotalMs) / float64(a.Requests)
}

// adminStats GET /admin/api/stats。
func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stats": s.snapshotStats()})
}
