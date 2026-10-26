// lanes 的事件流部分，对应 Python 版 lanes/autoclaw.py 的
// iter_sse / iter_body / stream_events / make_chunk / normalize_usage /
// classify_upstream_error / extract_model_ids。
package lanes

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"regexp"
	"strings"
	"time"
)

// Event 是统一的事件契约（对应 Python 的 (kind, value) 元组）。
type Event struct {
	Kind  string
	Value any
}

const (
	EvContent   = "content"
	EvReasoning = "reasoning"
	EvToolCalls = "tool_calls"
	EvFinish    = "finish"
	EvUsage     = "usage"
	EvQueue     = "queue"
	EvError     = "error"
	EvDone      = "done"
)

// HexID 生成 n 个 hex 字符的随机 id（对应 Python uuid4().hex[:n]）。
func HexID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

// ParseSSE 解析 SSE 流（"data:" 行），产出与 iter_sse 同构的事件序列。
func ParseSSE(body io.Reader) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		var finishReason string
		var usage any
		sawToolCalls := false

		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[len("data:"):])
			if data == "[DONE]" {
				break
			}
			var frame map[string]any
			if json.Unmarshal([]byte(data), &frame) != nil || frame == nil {
				continue
			}

			// 排队事件（MANAGED_MODEL_QUEUE_SSE_EVENT = "zwork_queue"）
			if asStr(frame["event"]) == "zwork_queue" {
				q := frame["zwork_queue"]
				if q == nil {
					q = frame
				}
				if !yield(Event{EvQueue, q}) {
					return
				}
				continue
			}
			if _, hasQueue := frame["zwork_queue"]; hasQueue {
				q := frame["zwork_queue"]
				if q == nil {
					q = frame
				}
				if !yield(Event{EvQueue, q}) {
					return
				}
				continue
			}

			if u, ok := frame["usage"].(map[string]any); ok && len(u) > 0 {
				usage = u
			}
			for _, ci := range asList(frame["choices"]) {
				choice, ok := ci.(map[string]any)
				if !ok {
					continue
				}
				if fr := asStr(choice["finish_reason"]); fr != "" {
					finishReason = fr
				}
				delta, _ := choice["delta"].(map[string]any)
				if delta == nil {
					continue
				}
				if c := asStr(delta["content"]); c != "" {
					if !yield(Event{EvContent, c}) {
						return
					}
				}
				if rc := asStr(delta["reasoning_content"]); rc != "" {
					if !yield(Event{EvReasoning, rc}) {
						return
					}
				}
				if tcs := asList(delta["tool_calls"]); len(tcs) > 0 {
					sawToolCalls = true
					if !yield(Event{EvToolCalls, delta["tool_calls"]}) {
						return
					}
				}
			}
		}

		if finishReason == "" {
			if sawToolCalls {
				finishReason = "tool_calls"
			} else {
				finishReason = "stop"
			}
		}
		if !yield(Event{EvFinish, finishReason}) {
			return
		}
		if usage != nil {
			if !yield(Event{EvUsage, usage}) {
				return
			}
		}
		yield(Event{EvDone, nil})
	}
}

// ParseBody 上游非流式响应 → 与 ParseSSE 同构的事件序列。
//
// 实测：请求体 stream:false 时上游回的是**单个 chat.completion JSON 对象**
// （Content-Type: application/json），而 stream:true 才是 SSE 帧。只认 "data:"
// 行的解析器会把非流式响应整个漏掉、吐出空 content——所以两个入口都要有，
// 且产出契约一致。
func ParseBody(raw []byte) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		text := strings.TrimSpace(string(raw))
		if text == "" {
			if !yield(Event{EvFinish, "stop"}) {
				return
			}
			yield(Event{EvDone, nil})
			return
		}
		var payload map[string]any
		if json.Unmarshal([]byte(text), &payload) != nil || payload == nil {
			// 既不是 JSON：可能是上游把错误写进了 body
			peek := text
			if len(peek) > 500 {
				peek = peek[:500]
			}
			yield(Event{EvError, peek})
			return
		}
		if u, ok := payload["usage"].(map[string]any); ok && len(u) > 0 {
			if !yield(Event{EvUsage, u}) {
				return
			}
		}
		for _, ci := range asList(payload["choices"]) {
			choice, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			message, _ := choice["message"].(map[string]any)
			if message == nil {
				message = map[string]any{}
			}
			if rc := asStr(message["reasoning_content"]); rc != "" {
				if !yield(Event{EvReasoning, rc}) {
					return
				}
			}
			if c := asStr(message["content"]); c != "" {
				if !yield(Event{EvContent, c}) {
					return
				}
			}
			for _, call := range asList(message["tool_calls"]) {
				if !yield(Event{EvToolCalls, []any{call}}) {
					return
				}
			}
			if fr := asStr(choice["finish_reason"]); fr != "" {
				if !yield(Event{EvFinish, fr}) {
					return
				}
				return // Python: 有 finish_reason 即 return，不再补 stop
			}
		}
		if !yield(Event{EvFinish, "stop"}) {
			return
		}
		yield(Event{EvDone, nil})
	}
}

// StreamEvents 按上游实际形态选解析器（以 Content-Type 为准，wantStream 只作提示：
// 实测 stream:false 回 JSON、stream:true 回 SSE，但这条对应关系上游不承诺）。
// raw 为非流式时预读的 body；isStream 时忽略 raw、直接包 body。
func StreamEvents(isStream bool, ctype string, raw []byte, body io.Reader) (iter.Seq[Event], error) {
	if strings.Contains(strings.ToLower(ctype), "event-stream") {
		return ParseSSE(body), nil
	}
	if !isStream && len(raw) > 0 {
		return ParseBody(raw), nil
	}
	// Content-Type 既不是 SSE 也没预读到 body：把整包读进来按 JSON 处理
	all, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(ctype), "json") || json.Valid(all) {
		return ParseBody(all), nil
	}
	// JSON 解析失败时回退当 SSE 文本试一次（与 Python iter_body 的容错一致）
	return ParseSSE(strings.NewReader(string(all))), nil
}

// MakeChunk 标准 OpenAI chat.completion.chunk。
func MakeChunk(chunkID, model string, delta map[string]any, finishReason string, usage map[string]any) map[string]any {
	if delta == nil {
		delta = map[string]any{}
	}
	frame := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nilOrStr(finishReason)}},
	}
	if usage != nil {
		frame["usage"] = usage
	}
	return frame
}

func nilOrStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NormalizeUsage AutoClaw 的 usage → OpenAI 形态。实测字段名不保证
// （prompt/completion/total 是最小集），缺的补 0，因为客户端普遍直接读 usage["prompt_tokens"]。
func NormalizeUsage(usage map[string]any) map[string]any {
	if usage == nil {
		usage = map[string]any{}
	}
	pick := func(names ...string) int64 {
		for _, n := range names {
			if v, ok := usage[n]; ok {
				if f, ok2 := numberVal(v); ok2 {
					return int64(f)
				}
			}
		}
		return 0
	}
	return map[string]any{
		"prompt_tokens":     pick("prompt_tokens", "input_tokens", "promptTokens"),
		"completion_tokens": pick("completion_tokens", "output_tokens", "completionTokens"),
		"total_tokens":      pick("total_tokens", "totalTokens"),
	}
}

// numberVal 只在值为数值时返回 true（Python isinstance(int)：不认浮点与字符串）。
func numberVal(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		if n == float64(int64(n)) {
			return n, true
		}
		return n, true
	}
	return 0, false
}

// ClassifyUpstreamError 406 空 body 是请求形状问题、403 pay-view 是配额问题，
// 与其它错误无法从响应体区分——给出可操作提示而不是裸状态码。
func ClassifyUpstreamError(status int, body string) string {
	if status == 406 && strings.TrimSpace(body) == "" {
		return "上游 406 空响应：请求形状不被接受。检查①请求体是否带了 max_tokens ②system 是否含 2.x banner ③是否误发了 X-Harness-Type"
	}
	if status == 403 {
		return "上游 403：可能是积分不足（pay-view）或权限问题"
	}
	if status == 401 {
		return "上游 401：token 失效或过期，重新登录 AutoClaw 桌面端"
	}
	return fmt.Sprintf("上游 HTTP %d", status)
}

// longTokenRe 把疑似 token/base64 长串打码后再落日志（Python: [A-Za-z0-9_\-]{24,}）。
var longTokenRe = regexp.MustCompile(`[A-Za-z0-9_\-]{24,}`)

// PeekBody 日志摘要：压平空白 + 长串打码 + 截 200。
func PeekBody(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	masked := longTokenRe.ReplaceAllString(flat, "…")
	if len(masked) > 200 {
		masked = masked[:200]
	}
	return masked
}

// ExtractModelIDs 从形形色色的响应形状里抠出模型 id 列表。抠不出返回 nil。
func ExtractModelIDs(data any) []string {
	var found []string
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, key := range []string{"model", "id", "model_id"} {
				if mid, ok := n[key].(string); ok && mid != "" && len(mid) < 64 {
					found = append(found, mid)
					break
				}
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(data)

	seen := map[string]bool{}
	var out []string
	for _, mid := range found {
		if !seen[mid] {
			seen[mid] = true
			out = append(out, mid)
		}
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
