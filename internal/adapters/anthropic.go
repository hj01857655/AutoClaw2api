// adapters 的 Anthropic 侧，对应 Python 版 adapters/anthropic.py。
//
// **本模块是 406 的高发地**：Anthropic 协议里 max_tokens 是必填字段，翻译结果
// 必然带它，而上游一见就 406。所以 AnthropicToChat 出口处必须删掉，不能指望
// lanes 层兜底（虽然 lanes 也会删，双保险）。
//
// 帧内 JSON 的键序：Go 的 map 序列化是字典序，Python 是插入序，两者语义一致但
// 字节不同。金样本测试按 JSON 语义比对，不比字节。
package adapters

import (
	"bytes"
	"encoding/json"
	"iter"
	"strings"
)

// anthropicStripFields 出口处删除：Anthropic 必填、上游见即 406。
var anthropicStripFields = []string{"max_tokens", "max_completion_tokens"}

// antFrame 组装一帧 `event: <type>\ndata: <json>\n\n`。
// data 里已含 "type" 键（与 Python 的 frame() 调用点逐字对齐），这里不补。
func antFrame(eventType string, data map[string]any) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteString("\ndata: ")
	buf.Write(marshalCompact(data))
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// ════════════════════════════════════════════════════════════════════════════
// 一、请求侧：Anthropic Messages -> Chat Completions
// ════════════════════════════════════════════════════════════════════════════

// anthropicContentToText content 可能是 str 或 block 数组，取纯文本。
// 与 responses 侧不同：这里用 "\n" 连接（Python adapters/anthropic.py:37）。
func anthropicContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := []string{}
		for _, bi := range c {
			switch b := bi.(type) {
			case map[string]any:
				if asStr(b["type"]) == "text" {
					parts = append(parts, firstNonEmptyStr(b["text"]))
				}
			case string:
				parts = append(parts, b)
			}
		}
		// Python: "\n".join(p for p in parts if p) —— 空串段直接丢弃
		keep := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		return strings.Join(keep, "\n")
	}
	return ""
}

// firstNonEmptyStr 对应 Python 的 `x or ""`。
func firstNonEmptyStr(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// anthropicImageBlockToPart Anthropic image block → OpenAI image_url part
// （支持 base64 与 url）。
func anthropicImageBlockToPart(block map[string]any) map[string]any {
	source := asMapOrEmpty(block["source"])
	var url string
	if asStr(source["type"]) == "base64" {
		mt := firstNonEmptyStr(source["media_type"])
		if mt == "" {
			mt = "image/png"
		}
		url = "data:" + mt + ";base64," + firstNonEmptyStr(source["data"])
	} else {
		url = firstNonEmptyStr(source["url"])
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// anthropicToolsToChat Anthropic tools（input_schema）-> Chat tools。
// Python 返回空 list 时 `if tools:` 为假，等价于这里返回 nil。
func anthropicToolsToChat(tools any) []any {
	list, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := []any{}
	for _, ti := range list {
		t, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		schema := t["input_schema"]
		if !truthy(schema) {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        firstNonEmptyStr(t["name"]),
				"description": firstNonEmptyStr(t["description"]),
				"parameters":  schema,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoiceToChat Anthropic tool_choice -> Chat tool_choice。
// 非 map（含字符串形式）一律返回 nil，与 Python isinstance(dict) 判定一致。
func anthropicToolChoiceToChat(choice any) any {
	m, ok := choice.(map[string]any)
	if !ok {
		return nil
	}
	switch asStr(m["type"]) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": firstNonEmptyStr(m["name"])},
		}
	}
	return nil
}

// AnthropicToChat Anthropic /v1/messages 请求体 → Chat Completions 请求体。
func AnthropicToChat(body map[string]any) map[string]any {
	messages := []any{}

	// system：Anthropic 允许 str 或 block 数组，且与 messages 分开
	if sys := body["system"]; truthy(sys) {
		if text := anthropicContentToText(sys); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	for _, mi := range asList(body["messages"]) {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role := asStr(msg["role"])
		if role != "user" && role != "assistant" {
			continue
		}
		content := msg["content"]

		if s, ok := content.(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, ok := content.([]any)
		if !ok {
			continue
		}

		texts, images, toolCalls, toolResults := []string{}, []any{}, []any{}, []any{}
		for _, bi := range blocks {
			b, ok := bi.(map[string]any)
			if !ok {
				continue
			}
			switch asStr(b["type"]) {
			case "text":
				texts = append(texts, firstNonEmptyStr(b["text"]))
			case "image":
				images = append(images, anthropicImageBlockToPart(b))
			case "tool_use":
				id := firstNonEmptyStr(b["id"])
				if id == "" {
					id = "call_" + hexID(20)
				}
				input := b["input"]
				if !truthy(input) {
					input = map[string]any{}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name": firstNonEmptyStr(b["name"]),
						// Python 用 json.dumps(..., ensure_ascii=False)（默认分隔符），
						// 这里是紧凑形式：语义等价 JSON，上游按对象解析，不受影响
						"arguments": string(marshalCompact(input)),
					},
				})
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": firstNonEmptyStr(b["tool_use_id"]),
					"content":      anthropicContentToText(b["content"]),
				})
			}
		}

		// tool_result 必须紧跟在自己那条消息里，不能和文本混在一条
		for _, r := range toolResults {
			messages = append(messages, r)
		}
		joined := strings.Join(texts, "\n")
		switch {
		case len(toolCalls) > 0:
			// Python: "\n".join(texts) or None
			var contentVal any
			if joined != "" {
				contentVal = joined
			}
			messages = append(messages, map[string]any{
				"role": "assistant", "content": contentVal, "tool_calls": toolCalls,
			})
		case len(texts) > 0 || len(images) > 0:
			// Python 的 `elif texts or images:` 判的是**列表非空**，不是内容非空：
			// texts=[""] 也会产出一条 content="" 的消息，这里照搬
			if len(images) > 0 {
				parts := []any{}
				for _, t := range texts {
					if t != "" {
						parts = append(parts, map[string]any{"type": "text", "text": t})
					}
				}
				parts = append(parts, images...)
				messages = append(messages, map[string]any{"role": role, "content": parts})
			} else {
				messages = append(messages, map[string]any{"role": role, "content": joined})
			}
		}
	}

	chat := map[string]any{
		"model":    firstNonEmptyStr(body["model"]),
		"messages": messages,
		"stream":   truthy(body["stream"]),
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v := body[k]; v != nil {
			chat[k] = v
		}
	}
	if ss := body["stop_sequences"]; truthy(ss) {
		chat["stop"] = ss
	}
	if tools := anthropicToolsToChat(body["tools"]); tools != nil {
		chat["tools"] = tools
	}
	if tc := anthropicToolChoiceToChat(body["tool_choice"]); truthy(tc) {
		chat["tool_choice"] = tc
	}

	// 出口处删 max_tokens：Anthropic 必填，上游见即 406
	for _, f := range anthropicStripFields {
		delete(chat, f)
	}
	return chat
}

// ════════════════════════════════════════════════════════════════════════════
// 二、非流式：聚合 Chat JSON -> Anthropic message JSON
// ════════════════════════════════════════════════════════════════════════════

// anthropicStopReason Chat finish_reason -> Anthropic stop_reason。
// Python 用 dict.get(finish, "end_turn")，未覆盖的值（含 None）落到 end_turn。
func anthropicStopReason(finish any) string {
	switch asStr(finish) {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// ChatJSONToAnthropic 聚合后的 Chat completion JSON → Anthropic message JSON。
func ChatJSONToAnthropic(result map[string]any, model string) map[string]any {
	msg := map[string]any{}
	var finish any
	if choices := asList(result["choices"]); len(choices) > 0 {
		if c := asMap(choices[0]); c != nil {
			if m := asMap(c["message"]); m != nil {
				msg = m
			}
			finish = c["finish_reason"]
		}
	}

	content := []any{}
	if rc := msg["reasoning_content"]; truthy(rc) {
		// Anthropic 的 thinking block；签名缺失时给空串，客户端据 type 处理
		content = append(content, map[string]any{
			"type": "thinking", "thinking": rc, "signature": "",
		})
	}
	if text := msg["content"]; truthy(text) {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, ci := range asList(msg["tool_calls"]) {
		call := asMap(ci)
		if call == nil {
			continue
		}
		fn := asMapOrEmpty(call["function"])
		var args any = map[string]any{}
		if s := firstNonEmptyStr(fn["arguments"]); s != "" {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil && truthy(parsed) {
				args = parsed
			} else {
				args = map[string]any{}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": firstNonEmptyStr(call["id"]),
			"name": firstNonEmptyStr(fn["name"]), "input": args,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usageRaw := asMapOrEmpty(result["usage"])
	return map[string]any{
		"id":            "msg_" + hexID(24),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(finish),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  orZero(usageRaw["prompt_tokens"]),
			"output_tokens": orZero(usageRaw["completion_tokens"]),
		},
	}
}

// orZero 对应 Python 的 `x or 0`：nil/0/false 都成 0，数值原样保留。
func orZero(v any) any {
	if truthy(v) {
		return v
	}
	return 0
}

// ════════════════════════════════════════════════════════════════════════════
// 三、流式：Chat SSE 行 → Anthropic SSE
// ════════════════════════════════════════════════════════════════════════════

// antBlock 已开出的 content block（对应 Python open_blocks 的三元组）。
type antBlock struct {
	index int
	kind  string
	id    string
}

// ChatSSEToAnthropic Chat SSE 行 → Anthropic 事件流。
//
// 输入是 handler.chatSSELines 的产出（带 "data: " 前缀的帧），只做协议外皮，
// 不改内容。事件序列：message_start → content_block_start/delta/stop →
// message_delta → message_stop。
func ChatSSEToAnthropic(lines iter.Seq[[]byte], model string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		msgID := "msg_" + hexID(24)

		textIdx := 0
		var thinkIdx *int
		openBlocks := []antBlock{}
		started := false
		var usageOut map[string]any
		textOpen := false
		toolCallsSeen := map[float64]int{}

		messageStart := func() bool {
			return yield(antFrame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": msgID, "type": "message", "role": "assistant",
					"model": model, "content": []any{}, "stop_reason": nil,
					"stop_sequence": nil,
					"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			}))
		}

		for raw := range lines {
			s := bytes.TrimRight(raw, "\r\n")
			if !bytes.HasPrefix(s, []byte("data: ")) {
				continue
			}
			data := bytes.TrimSpace(s[6:])
			if bytes.Equal(data, []byte("[DONE]")) {
				break
			}
			var chunk map[string]any
			if json.Unmarshal(data, &chunk) != nil || chunk == nil {
				continue
			}

			if !started {
				started = true
				if !messageStart() {
					return
				}
			}

			if u := asMap(chunk["usage"]); truthy(u) {
				usageOut = u
			}

			for _, ci := range asList(chunk["choices"]) {
				choice := asMapOrEmpty(ci)
				delta := asMapOrEmpty(choice["delta"])

				if rc := delta["reasoning_content"]; truthy(rc) {
					if thinkIdx == nil {
						i := len(openBlocks)
						thinkIdx = &i
						openBlocks = append(openBlocks, antBlock{i, "thinking", ""})
						if !yield(antFrame("content_block_start", map[string]any{
							"type": "content_block_start", "index": i,
							"content_block": map[string]any{"type": "thinking", "thinking": ""},
						})) {
							return
						}
					}
					if !yield(antFrame("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": *thinkIdx,
						"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
					})) {
						return
					}
				}

				if ct := delta["content"]; truthy(ct) {
					if !textOpen {
						if thinkIdx != nil {
							if !yield(antFrame("content_block_stop", map[string]any{
								"type": "content_block_stop", "index": *thinkIdx})) {
								return
							}
							thinkIdx = nil
						}
						textIdx = len(openBlocks)
						openBlocks = append(openBlocks, antBlock{textIdx, "text", ""})
						textOpen = true
						if !yield(antFrame("content_block_start", map[string]any{
							"type": "content_block_start", "index": textIdx,
							"content_block": map[string]any{"type": "text", "text": ""},
						})) {
							return
						}
					}
					if !yield(antFrame("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": textIdx,
						"delta": map[string]any{"type": "text_delta", "text": ct},
					})) {
						return
					}
				}

				for _, tci := range asList(delta["tool_calls"]) {
					call := asMapOrEmpty(tci)
					key := callIndex(call)
					fn := asMapOrEmpty(call["function"])

					idx, seen := toolCallsSeen[key]
					if !seen {
						if !textOpen && thinkIdx != nil {
							if !yield(antFrame("content_block_stop", map[string]any{
								"type": "content_block_stop", "index": *thinkIdx})) {
								return
							}
							thinkIdx = nil
						} else if textOpen {
							if !yield(antFrame("content_block_stop", map[string]any{
								"type": "content_block_stop", "index": textIdx})) {
								return
							}
							textOpen = false
						}
						idx = len(openBlocks)
						callID := firstNonEmptyStr(call["id"])
						openBlocks = append(openBlocks, antBlock{idx, "tool_use", callID})
						toolCallsSeen[key] = idx
						if !yield(antFrame("content_block_start", map[string]any{
							"type": "content_block_start", "index": idx,
							"content_block": map[string]any{
								"type": "tool_use", "id": callID,
								"name": firstNonEmptyStr(fn["name"]), "input": map[string]any{},
							},
						})) {
							return
						}
					}
					if a := fn["arguments"]; truthy(a) {
						if !yield(antFrame("content_block_delta", map[string]any{
							"type": "content_block_delta", "index": idx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": a},
						})) {
							return
						}
					}
				}

				if truthy(choice["finish_reason"]) {
					for _, b := range openBlocks {
						if !yield(antFrame("content_block_stop", map[string]any{
							"type": "content_block_stop", "index": b.index})) {
							return
						}
					}
					openBlocks = nil
				}
			}
		}

		// 一行有效数据都没来：补一组空 message_start + 空 text block，
		// 保证下游拿到的永远是合法（只是空）的 Anthropic 流
		if !started {
			started = true
			if !messageStart() {
				return
			}
			textIdx = 0
			openBlocks = append(openBlocks, antBlock{0, "text", ""})
			if !yield(antFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})) {
				return
			}
		}

		for _, b := range openBlocks {
			if !yield(antFrame("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": b.index})) {
				return
			}
		}

		var outTokens any
		if usageOut != nil {
			outTokens = orZero(usageOut["completion_tokens"])
		} else {
			outTokens = 0
		}
		if !yield(antFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outTokens},
		})) {
			return
		}
		yield(antFrame("message_stop", map[string]any{"type": "message_stop"}))
	}
}

// callIndex Chat tool_calls 分片的合并键（Python `call.get("index", 0)`）。
// JSON 数字在 Go 里是 float64；缺失或非数字都归 0。
func callIndex(call map[string]any) float64 {
	if f, ok := call["index"].(float64); ok {
		return f
	}
	return 0
}

func asMapOrEmpty(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}
