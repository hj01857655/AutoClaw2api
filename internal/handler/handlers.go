// 协议路由层，对应 Python 版 routes/{chat,anthropic,responses}.py 与
// server.py 的 _token / health / aggregate / _merge_tool_calls /
// _chat_sse_lines / _upstream_error / _stream_log。
//
// 本层只管协议形状；上游发送与重试在 upstream 包、请求整形在 lanes 包、
// 鉴权与日志在 server.go 的 middleware。
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"autoclaw2api/internal/adapters"
	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/upstream"
)

// ── 凭据访问 ─────────────────────────────────────────────────────────────

// token 对应 Python _token()：oauth 源走 OAuthCredentialStore，desktop 源走
// 解密缓存。
func (s *Server) token() (string, error) {
	if s.OAuth != nil {
		return s.OAuth.Get()
	}
	if s.Store == nil {
		return "", errors.New("凭据源未配置")
	}
	cred, err := s.Store.Get()
	if err != nil {
		return "", err
	}
	return cred.AccessToken, nil
}

// ── 健康检查 ─────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	notice := ""
	valid := false
	remain := ""
	accountName := ""
	// Python: (_store._cred or {}).get("account","")[:12]——只看解密缓存，
	// 不触发解密；oauth 模式下它依然是 desktop 那份（可能为空）。
	if s.Store != nil {
		if cred := s.Store.Cached(); cred != nil {
			accountName = firstN(cred.Account, 12)
		}
	}
	token, err := s.token()
	switch {
	case err != nil:
		notice = err.Error()
	case token != "":
		valid = true
		remain = lanes.FmtExp(token)
	}
	models := []string{}
	if valid {
		models = s.ListModels()
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": valid, "service": "AutoClaw2api",
		"base": s.Cfg.ClawBase, "upstream": s.Cfg.UpstreamBase,
		"account": accountName, "token_exp": remain, "notice": notice,
		"models": models,
	})
}

// ── 三协议统一路由 ───────────────────────────────────────────────────────

// protoShape 三协议路由的差异全部收在这里，主干 runChatProto 只写一遍。
type protoShape struct {
	// body 不是 JSON object 时的 400 响应体；nil = chat 协议（缺 messages 时报）
	notObjectBody map[string]any
	missingField  string
	// 请求翻译：chat 原样、anthropic/responses 先转成 Chat
	translateIn func(body map[string]any) map[string]any
	// 错误体构造（三协议形状不同）
	errBody func(errType, message string) map[string]any
	// 上游连接失败的错误类型：chat=upstream_error，另两家=api_error
	upstreamType string
	// 非流式结果翻译；nil = chat 原样
	translateJSON func(result map[string]any, model string) map[string]any
	// 流式帧翻译（吃带 "data: " 前缀的本地 Chat SSE 行）；nil = chat 原样
	translateSSE func(lines iter.Seq[[]byte], model string) iter.Seq[[]byte]
	// anthropic/responses 不读 stream_options.include_usage（Python 传 False）
	forceNoUsage bool
}

func openAIErrorBody(errType, message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType}}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.runChatProto(w, r, protoShape{
		missingField: "invalid request: 'messages' is required",
		errBody:      openAIErrorBody,
		upstreamType: "upstream_error",
	})
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	s.runChatProto(w, r, protoShape{
		notObjectBody: adapters.AnthropicErrorBody("invalid_request_error",
			"invalid request: JSON object body required"),
		missingField:  "invalid request: 'messages' is required",
		translateIn:   adapters.AnthropicToChat,
		errBody:       adapters.AnthropicErrorBody,
		upstreamType:  "api_error",
		translateJSON: adapters.ChatJSONToAnthropic,
		translateSSE:  adapters.ChatSSEToAnthropic,
		forceNoUsage:  true,
	})
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.runChatProto(w, r, protoShape{
		notObjectBody: adapters.ResponsesErrorBody("invalid_request_error",
			"invalid request: JSON object body required"),
		missingField:  "invalid request: 'input' is required",
		translateIn:   adapters.ResponsesToChat,
		errBody:       adapters.ResponsesErrorBody,
		upstreamType:  "api_error",
		translateJSON: adapters.ChatJSONToResponses,
		translateSSE:  adapters.ChatSSEToResponses,
		forceNoUsage:  true,
	})
}

func (s *Server) runChatProto(w http.ResponseWriter, r *http.Request, shape protoShape) {
	info := infoOf(r)

	var chatBody map[string]any
	if info.payload != nil {
		chatBody = info.payload
		if shape.translateIn != nil {
			chatBody = shape.translateIn(chatBody)
		}
	}
	if !truthyVal(chatBody["messages"]) {
		writeJSON(w, http.StatusBadRequest,
			shape.errBody("invalid_request_error", shape.missingField))
		return
	}

	wantStream := truthyVal(chatBody["stream"])
	wantUsage := false
	if !shape.forceNoUsage {
		if so := asMap(chatBody["stream_options"]); so != nil {
			wantUsage = truthyVal(so["include_usage"])
		}
	}
	chunkID := "chatcmpl-" + lanes.HexID(24)

	resp, err := s.Up.Open(info.rid, chatBody)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, shape.errBody(shape.upstreamType, err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, status := s.upstreamError(info.rid, resp)
		writeJSON(w, status, body)
		return
	}

	model := str(chatBody["model"])
	if model == "" {
		model = s.Cfg.DefaultModelName
	}

	if !wantStream {
		raw, err := upstream.ReadAll(resp)
		if err != nil {
			writeJSON(w, http.StatusBadGateway,
				shape.errBody(shape.upstreamType, "上游响应读取失败: "+err.Error()))
			return
		}
		events, err := lanes.StreamEvents(false, resp.Header.Get("Content-Type"), raw, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway,
				shape.errBody(shape.upstreamType, err.Error()))
			return
		}
		result := s.aggregate(info.rid, events, model, chunkID)
		if shape.translateJSON != nil {
			result = shape.translateJSON(result, model)
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	events, err := lanes.StreamEvents(true, resp.Header.Get("Content-Type"), nil, resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway,
			shape.errBody(shape.upstreamType, err.Error()))
		return
	}
	lines := s.chatSSELines(events, model, chunkID, wantUsage)
	if shape.translateSSE != nil {
		lines = shape.translateSSE(lines, model)
	}
	s.writeSSE(w, lines, info)
}

// writeSSE 下发 SSE：头三件套 + 逐帧 write/flush，包裹 streamLog。
func (s *Server) writeSSE(w http.ResponseWriter, lines iter.Seq[[]byte], info *reqInfo) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for frame := range s.streamLog(lines, info.rid, info.t0) {
		if _, err := w.Write(frame); err != nil {
			return // 客户端断开；streamLog 已记日志，断开信号经 yield 传回上游生成器
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// ── 聚合与 SSE 产出 ──────────────────────────────────────────────────────

// aggregate 把上游响应聚合成非流式 Chat completion。上游 stream:false 回单个
// JSON、stream:true 回 SSE，选哪个解析器由 lanes.StreamEvents 按 Content-Type
// 定，这里无感。
func (s *Server) aggregate(rid string, events iter.Seq[lanes.Event], model, chunkID string) map[string]any {
	var content, reasoning []string
	var toolCalls []any
	finishReason := ""
	var usage map[string]any
	for ev := range events {
		switch ev.Kind {
		case lanes.EvContent:
			content = append(content, str(ev.Value))
		case lanes.EvReasoning:
			reasoning = append(reasoning, str(ev.Value))
		case lanes.EvToolCalls:
			toolCalls = append(toolCalls, ev.Value)
		case lanes.EvFinish:
			finishReason = str(ev.Value)
		case lanes.EvUsage:
			usage = asMap(ev.Value)
		case lanes.EvQueue:
			s.Log.Info("上游排队事件", slog.String("rid", rid), slog.Any("event", ev.Value))
		}
	}
	message := map[string]any{"role": "assistant", "content": strings.Join(content, "")}
	if len(reasoning) > 0 {
		message["reasoning_content"] = strings.Join(reasoning, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = mergeToolCalls(toolCalls)
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	return map[string]any{
		"id":      chunkID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message,
			"finish_reason": finishReason}},
		"usage": lanes.NormalizeUsage(usage),
	}
}

// mergeToolCalls 流式 tool_calls 是按 index 分片的，拼回完整调用。
func mergeToolCalls(fragments []any) []any {
	byIndex := map[float64]map[string]any{}
	var order []float64
	for _, frag := range fragments {
		items, ok := frag.([]any)
		if !ok {
			if frag == nil {
				continue
			}
			items = []any{frag}
		}
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			idx := float64(0)
			if v, has := item["index"]; has {
				if f, ok2 := v.(float64); ok2 {
					idx = f
				}
			}
			slot, seen := byIndex[idx]
			if !seen {
				slot = map[string]any{
					"id":       "",
					"type":     "function",
					"function": map[string]any{"name": "", "arguments": ""},
				}
				byIndex[idx] = slot
				order = append(order, idx)
			}
			if v := str(item["id"]); v != "" {
				slot["id"] = v
			}
			fn := asMap(item["function"])
			if fn != nil {
				fnOut := slot["function"].(map[string]any)
				if v := str(fn["name"]); v != "" {
					fnOut["name"] = str(fnOut["name"]) + v
				}
				if v := str(fn["arguments"]); v != "" {
					fnOut["arguments"] = str(fnOut["arguments"]) + v
				}
			}
		}
	}
	sort.Float64s(order)
	out := make([]any, 0, len(order))
	for _, idx := range order {
		out = append(out, byIndex[idx])
	}
	return out
}

// chatSSELines 产出标准 Chat SSE 行（带 data: 前缀，含 [DONE] 收尾帧）。
// tool_calls 统一在收尾时一次性下发，避免分片错位；首帧合并 role。
func (s *Server) chatSSELines(events iter.Seq[lanes.Event], model, chunkID string,
	wantUsage bool) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		finishReason := ""
		var usage map[string]any
		sawToolCalls := false
		sentRole := false
		var toolBuf []any

		emit := func(delta map[string]any) bool {
			if !sentRole {
				merged := make(map[string]any, len(delta)+1)
				merged["role"] = "assistant"
				for k, v := range delta {
					merged[k] = v
				}
				delta = merged
				sentRole = true
			}
			frame := marshalCompact(lanes.MakeChunk(chunkID, model, delta, "", nil))
			return yield(append([]byte("data: "), append(frame, '\n', '\n')...))
		}

		done := false
	loop:
		for ev := range events {
			switch ev.Kind {
			case lanes.EvDone:
				done = true
				break loop
			case lanes.EvQueue:
				continue
			case lanes.EvFinish:
				finishReason = str(ev.Value)
			case lanes.EvUsage:
				usage = asMap(ev.Value)
			case lanes.EvContent:
				if !emit(map[string]any{"content": str(ev.Value)}) {
					return
				}
			case lanes.EvReasoning:
				if !emit(map[string]any{"reasoning_content": str(ev.Value)}) {
					return
				}
			case lanes.EvToolCalls:
				sawToolCalls = true
				toolBuf = append(toolBuf, ev.Value)
			}
		}
		_ = done

		if sawToolCalls {
			frame := marshalCompact(lanes.MakeChunk(chunkID, model,
				map[string]any{"tool_calls": mergeToolCalls(toolBuf)}, "", nil))
			if !yield(append([]byte("data: "), append(frame, '\n', '\n')...)) {
				return
			}
		}

		if finishReason == "" {
			if sawToolCalls {
				finishReason = "tool_calls"
			} else {
				finishReason = "stop"
			}
		}
		tail := lanes.MakeChunk(chunkID, model, map[string]any{}, finishReason, nil)
		if usage != nil && !wantUsage {
			tail["usage"] = lanes.NormalizeUsage(usage)
		}
		frame := marshalCompact(tail)
		if !yield(append([]byte("data: "), append(frame, '\n', '\n')...)) {
			return
		}
		if usage != nil && wantUsage {
			frame := marshalCompact(lanes.MakeChunk(chunkID, model, map[string]any{},
				"", lanes.NormalizeUsage(usage)))
			if !yield(append([]byte("data: "), append(frame, '\n', '\n')...)) {
				return
			}
		}
		yield([]byte("data: [DONE]\n\n"))
	}
}

// ── 上游错误归一 ─────────────────────────────────────────────────────────

// upstreamError 上游错误 → OpenAI error 结构（三协议共用，对齐 Python
// _upstream_error），并把真实原因落到日志。401 时顺带作废凭据缓存。
func (s *Server) upstreamError(rid string, resp *http.Response) (map[string]any, int) {
	raw, _ := upstream.ReadAll(resp)
	full := string(raw)
	text := full
	if len(text) > 2000 {
		text = text[:2000]
	}
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if ctype == "" {
		ctype = "?"
	}
	s.Log.Warn("上游非 200",
		slog.String("rid", rid), slog.Int("status", resp.StatusCode),
		slog.String("ctype", ctype), slog.String("peek", lanes.PeekBody(text)))

	reason := lanes.ClassifyUpstreamError(resp.StatusCode, text)
	errType := "upstream_error"
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		if s.Store != nil {
			s.Store.Invalidate()
		}
	case http.StatusNotAcceptable:
		errType = "invalid_request_error"
	}

	if strings.Contains(ctype, "json") {
		var body map[string]any
		if json.Unmarshal([]byte(full), &body) == nil && body != nil {
			// pay-view 这类响应带 action/code，原样透出比翻译成英文更有用
			if truthyVal(body["action"]) || truthyVal(body["code"]) {
				return map[string]any{"error": map[string]any{
					"message": reason, "type": errType,
					"code": body["code"], "action": body["action"],
				}}, resp.StatusCode
			}
			msg := str(body["message"])
			if msg == "" {
				msg = str(body["msg"])
			}
			if msg == "" {
				msg = reason
			}
			return map[string]any{"error": map[string]any{
				"message": msg, "type": errType, "code": body["code"],
			}}, resp.StatusCode
		}
	}
	status := resp.StatusCode
	if strings.TrimSpace(text) == "" {
		status = http.StatusBadGateway
	}
	return map[string]any{"error": map[string]any{
		"message": reason, "type": errType, "code": resp.StatusCode,
	}}, status
}

// ── 流式日志包裹 ─────────────────────────────────────────────────────────

// streamLog 包裹 SSE 生成器：TTFT、字节数、原文落盘、done/断开日志。
// 消费方提前退出（客户端断开）经 yield 返回 false 传入。
func (s *Server) streamLog(seq iter.Seq[[]byte], rid string, t0 time.Time) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		total := 0
		var ttft time.Duration
		haveTTFT := false
		completed := false
		defer func() {
			if !completed {
				return
			}
			ttftStr := "-"
			if haveTTFT {
				ttftStr = fmt.Sprintf("%.1fs", ttft.Seconds())
			}
			s.Log.Info("done:",
				slog.String("rid", rid),
				slog.String("line", fmt.Sprintf("stream %dB %.1fs ttft=%s",
					total, time.Since(t0).Seconds(), ttftStr)))
		}()
		collect := s.Dump != nil && s.Cfg.DumpOn
		var buf []byte
		for piece := range seq {
			if !haveTTFT {
				ttft = time.Since(t0)
				haveTTFT = true
			}
			total += len(piece)
			if collect {
				buf = append(buf, piece...)
			}
			if !yield(piece) {
				s.Log.Warn("client 断开",
					slog.String("rid", rid),
					slog.String("line", fmt.Sprintf("stream %dB %.1fs",
						total, time.Since(t0).Seconds())))
				return
			}
		}
		completed = true
		if collect && len(buf) > 0 {
			s.Dump.Save(rid, "resp", string(buf))
		}
	}
}

// ── 小工具 ───────────────────────────────────────────────────────────────

// truthyVal 对应 Python 的 `if x:` 判定：None/False/0/""/空集合为假。
func truthyVal(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// firstN 按 rune 截前 n 个字符（Python s[:n] 语义）。
func firstN(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n])
	}
	return s
}
