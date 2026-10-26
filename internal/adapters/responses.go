// adapters 的 Responses 侧，对应 Python 版 adapters/responses.py。
//
// **本模块是 406 的第二高发地**：Responses 的 max_output_tokens 与 Anthropic 的
// max_tokens 一样是调用方常填字段，上游一见就 406 空响应。出口处必须删掉；
// lanes 层也会删，双保险。
//
// 与 Cline 版的关键差异（不能照抄 Cline2api-go）：这里的帧**带 `event:` 前缀、
// 不带 sequence_number**；非流式 JSON 多 output_text / finish_reason /
// error:null 与 output_tokens_details.reasoning_tokens。
package adapters

import (
	"bytes"
	"encoding/json"
	"iter"
	"strings"
	"time"
)

// responsesStripTokenLimits 出口处删除：Responses 的 token 上限字段，上游见即 406
var responsesStripTokenLimits = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// responsesTextBlockTypes Responses 里算「文本」的 block 类型
var responsesTextBlockTypes = map[string]bool{
	"input_text": true, "output_text": true, "text": true,
	"summary_text": true, "refusal": true,
}

// ════════════════════════════════════════════════════════════════════════════
// 一、请求侧：Responses -> Chat Completions
// ════════════════════════════════════════════════════════════════════════════

// responsesContentToText content 可能是 str 或 block/item 数组，取纯文本。
// 与 anthropic 侧同样用 "\n" 连接非空段。
func responsesContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := []string{}
		for _, bi := range c {
			switch b := bi.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				if responsesTextBlockTypes[asStr(b["type"])] {
					parts = append(parts, firstNonEmptyStr(b["text"]))
				}
			}
		}
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

// responsesConvertTools Responses tools（扁平 function）→ Chat tools（嵌套）。
// 内置工具（web_search 等）type 不是 function，上游不认，直接丢。
func responsesConvertTools(tools any) []any {
	list, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := []any{}
	for _, ti := range list {
		tool, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		if t := tool["type"]; t != nil && t != "function" {
			continue
		}
		params := tool["parameters"]
		if !truthy(params) {
			params = tool["input_schema"]
		}
		if !truthy(params) {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        firstNonEmptyStr(tool["name"]),
				"description": firstNonEmptyStr(tool["description"]),
				"parameters":  params,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesConvertToolChoice Responses tool_choice -> Chat tool_choice。
func responsesConvertToolChoice(choice any) any {
	if !truthy(choice) {
		return nil
	}
	switch c := choice.(type) {
	case string:
		switch c {
		case "auto":
			return "auto"
		case "required":
			return "required"
		case "none":
			return "none"
		}
		return nil
	case map[string]any:
		if asStr(c["type"]) == "function" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": firstNonEmptyStr(c["name"])},
			}
		}
		switch asStr(c["type"]) {
		case "auto":
			return "auto"
		case "required":
			return "required"
		}
	}
	return nil
}

// responsesInputToMessages “input“（str 或 item 数组）→ Chat messages，追加到 out。
//
// 悬空的 function_call 在**整个 input 走完之后**才落地成一条 assistant 消息
// （Python 就是末尾统一 flush），因此多轮工具历史里 function_call 与其后
// function_call_output 的顺序与 Python 版一致，别「顺手优化」成即时下发。
func responsesInputToMessages(inp any, out *[]any) {
	if s, ok := inp.(string); ok {
		*out = append(*out, map[string]any{"role": "user", "content": s})
		return
	}

	pendingCalls := []any{}
	for _, ii := range asList(inp) {
		item, ok := ii.(map[string]any)
		if !ok {
			continue
		}
		switch itype := item["type"]; {
		case itype == nil || itype == "message":
			role := firstNonEmptyStr(item["role"])
			if role == "" {
				role = "user"
			}
			switch role {
			case "user", "assistant", "system", "developer":
			default:
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			content := item["content"]
			if list, ok := content.([]any); ok {
				// 多段 content：文本与图片混排
				parts := []any{}
				for _, bi := range list {
					block, ok := bi.(map[string]any)
					if !ok {
						continue
					}
					switch asStr(block["type"]) {
					case "input_text", "output_text", "text":
						parts = append(parts, map[string]any{
							"type": "text", "text": firstNonEmptyStr(block["text"]),
						})
					case "input_image":
						url := firstNonEmptyStr(block["image_url"])
						if url == "" {
							url = firstNonEmptyStr(block["url"])
						}
						parts = append(parts, map[string]any{
							"type": "image_url", "image_url": map[string]any{"url": url},
						})
					}
				}
				var val any
				if len(parts) > 0 {
					val = parts
				} else {
					val = responsesContentToText(content)
				}
				*out = append(*out, map[string]any{"role": role, "content": val})
			} else {
				*out = append(*out, map[string]any{
					"role": role, "content": responsesContentToText(content),
				})
			}

		case itype == "function_call":
			id := firstNonEmptyStr(item["call_id"])
			if id == "" {
				id = firstNonEmptyStr(item["id"])
			}
			if id == "" {
				id = "call_" + hexID(20)
			}
			args := firstNonEmptyStr(item["arguments"])
			if args == "" {
				args = "{}"
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{
					"name": firstNonEmptyStr(item["name"]), "arguments": args,
				},
			})

		case itype == "function_call_output":
			// 必须作为独立的 tool 消息，不能和文本混
			*out = append(*out, map[string]any{
				"role":         "tool",
				"tool_call_id": firstNonEmptyStr(item["call_id"]),
				"content":      responsesContentToText(item["output"]),
			})

		case itype == "reasoning":
			// 推理链不回灌，上游自己会重新想

		default:
			// 其它 item 类型（local_shell_call 等）上游不认，丢弃
		}
	}

	if len(pendingCalls) > 0 {
		// 悬空的 function_call：补一条 assistant 消息承载，否则 tool 配对断裂
		*out = append(*out, map[string]any{
			"role": "assistant", "content": nil, "tool_calls": pendingCalls,
		})
	}
}

// ResponsesToChat Responses “/v1/responses“ 请求体 → Chat Completions 请求体。
func ResponsesToChat(body map[string]any) map[string]any {
	messages := []any{}

	if ins := body["instructions"]; truthy(ins) {
		if text := responsesContentToText(ins); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	responsesInputToMessages(body["input"], &messages)

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
	if tools := responsesConvertTools(body["tools"]); tools != nil {
		chat["tools"] = tools
	}
	if tc := responsesConvertToolChoice(body["tool_choice"]); truthy(tc) {
		chat["tool_choice"] = tc
	}

	// 出口处删 token 上限：Responses 的 max_output_tokens，上游见即 406
	for _, f := range responsesStripTokenLimits {
		delete(chat, f)
	}
	return chat
}

// ════════════════════════════════════════════════════════════════════════════
// 二、非流式：聚合 Chat JSON -> Responses JSON
// ════════════════════════════════════════════════════════════════════════════

// makeRespID 有 chat id 就复用（resp_<chatcmpl-…>），否则随机。
func makeRespID(chatID any) string {
	if s := firstNonEmptyStr(chatID); s != "" {
		return "resp_" + s
	}
	return "resp_" + hexID(32)
}

// responsesReasoningTokens usage.completion_tokens_details.reasoning_tokens
func responsesReasoningTokens(usageRaw map[string]any) any {
	if d := asMap(usageRaw["completion_tokens_details"]); d != nil {
		return orZero(d["reasoning_tokens"])
	}
	return 0
}

// responsesUsage Responses 的 usage 形状（含 output_tokens_details）。
func responsesUsage(usageRaw map[string]any) map[string]any {
	return map[string]any{
		"input_tokens":  orZero(usageRaw["prompt_tokens"]),
		"output_tokens": orZero(usageRaw["completion_tokens"]),
		"total_tokens":  orZero(usageRaw["total_tokens"]),
		"output_tokens_details": map[string]any{
			"reasoning_tokens": responsesReasoningTokens(usageRaw),
		},
	}
}

// ChatJSONToResponses 聚合后的 Chat completion JSON → Responses JSON。
func ChatJSONToResponses(result map[string]any, model string) map[string]any {
	choice := map[string]any{}
	if choices := asList(result["choices"]); len(choices) > 0 {
		if c := asMap(choices[0]); c != nil {
			choice = c
		}
	}
	message := asMapOrEmpty(choice["message"])
	output := []any{}

	if rc := message["reasoning_content"]; truthy(rc) {
		output = append(output, map[string]any{
			"type": "reasoning",
			"summary": []any{map[string]any{
				"type": "summary_text", "text": rc,
			}},
			"content": []any{},
		})
	}
	if text := message["content"]; truthy(text) {
		output = append(output, responsesMessageItem(text))
	}
	for _, ci := range asList(message["tool_calls"]) {
		call := asMapOrEmpty(ci)
		fn := asMapOrEmpty(call["function"])
		output = append(output, map[string]any{
			"type": "function_call", "status": "completed",
			"call_id":   firstNonEmptyStr(call["id"]),
			"name":      firstNonEmptyStr(fn["name"]),
			"arguments": nonEmptyOrDefault(firstNonEmptyStr(fn["arguments"]), "{}"),
		})
	}
	if len(output) == 0 {
		output = append(output, responsesMessageItem(""))
	}

	usageRaw := asMapOrEmpty(result["usage"])
	finish := choice["finish_reason"]

	return map[string]any{
		"id": makeRespID(result["id"]),
		// Python: result.get("created") or int(time.time())
		"created_at":  orZeroOrNow(result["created"]),
		"object":      "response",
		"status":      "completed",
		"model":       model,
		"output":      output,
		"output_text": nonEmptyOrDefault(message["content"], ""),
		"usage":       responsesUsage(usageRaw),
		"finish_reason": func() any {
			if truthy(finish) {
				return finish
			}
			return "stop"
		}(),
		"error": nil,
	}
}

func nonEmptyOrDefault(v any, def string) any {
	if truthy(v) {
		return v
	}
	return def
}

func orZeroOrNow(v any) any {
	if truthy(v) {
		return v
	}
	return time.Now().Unix()
}

func responsesMessageItem(text any) map[string]any {
	return map[string]any{
		"type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 三、流式：Chat SSE 行 → Responses SSE
// ════════════════════════════════════════════════════════════════════════════

type respToolCall struct {
	id   string
	name string
	args []string
}

func respFrame(event string, data map[string]any) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(marshalCompact(data))
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// ChatSSEToResponses Chat SSE 行 → Responses 事件序列。
//
// 事件顺序：response.created → output_item.added → content_part.added →
// output_text.delta* → output_text.done → content_part.done →
// output_item.done → response.completed。
//
// Python 版里那个 seq 计数器只自增、从不写进帧（区别于 Cline 版每帧带
// sequence_number），Go 版不维护它。
func ChatSSEToResponses(lines iter.Seq[[]byte], model string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		respID := "resp_" + hexID(24)
		started := false
		text := []string{}
		reasoning := []string{}
		toolCalls := map[float64]*respToolCall{}
		toolOrder := []float64{}
		usage := map[string]any{}
		finishReason := "stop"

		start := func() []func() bool {
			return []func() bool{
				func() bool {
					return yield(respFrame("response.created", map[string]any{
						"type": "response.created",
						"response": map[string]any{
							"id": respID, "object": "response",
							"status": "in_progress", "model": model, "output": []any{},
						},
					}))
				},
				func() bool {
					return yield(respFrame("response.output_item.added", map[string]any{
						"type": "response.output_item.added", "output_index": 0,
						"item": map[string]any{
							"type": "message", "role": "assistant",
							"status": "in_progress", "content": []any{},
						},
					}))
				},
				func() bool {
					return yield(respFrame("response.content_part.added", map[string]any{
						"type": "response.content_part.added", "item_id": respID,
						"output_index": 0, "content_index": 0,
						"part": map[string]any{
							"type": "output_text", "text": "", "annotations": []any{},
						},
					}))
				},
			}
		}

		emitStart := func() bool {
			for _, f := range start() {
				if !f() {
					return false
				}
			}
			return true
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

			if u := asMap(chunk["usage"]); truthy(u) {
				usage = u
			}

			if !started {
				started = true
				if !emitStart() {
					return
				}
			}

			for _, ci := range asList(chunk["choices"]) {
				choice := asMapOrEmpty(ci)
				delta := asMapOrEmpty(choice["delta"])

				if rc := delta["reasoning_content"]; truthy(rc) {
					reasoning = append(reasoning, asStr(rc))
					if !yield(respFrame("response.reasoning_summary_text.delta", map[string]any{
						"type":    "response.reasoning_summary_text.delta",
						"item_id": respID, "output_index": 0, "summary_index": 0,
						"delta": rc,
					})) {
						return
					}
				}

				if ct := delta["content"]; truthy(ct) {
					if s := asStr(ct); s != "" {
						text = append(text, s)
					}
					if !yield(respFrame("response.output_text.delta", map[string]any{
						"type": "response.output_text.delta", "item_id": respID,
						"output_index": 0, "content_index": 0, "delta": ct,
					})) {
						return
					}
				}

				for _, tci := range asList(delta["tool_calls"]) {
					call := asMapOrEmpty(tci)
					key := callIndex(call)
					fn := asMapOrEmpty(call["function"])

					tc, seen := toolCalls[key]
					if !seen {
						// Python 先算 len(tool_order)+1 再 append，等价于 1-based 序号
						toolIdx := len(toolOrder) + 1
						tc = &respToolCall{
							id:   firstNonEmptyStr(call["id"]),
							name: firstNonEmptyStr(fn["name"]),
						}
						toolCalls[key] = tc
						toolOrder = append(toolOrder, key)
						if !yield(respFrame("response.output_item.added", map[string]any{
							"type": "response.output_item.added", "output_index": toolIdx,
							"item": map[string]any{
								"type": "function_call", "status": "in_progress",
								"call_id": tc.id, "name": tc.name, "arguments": "",
							},
						})) {
							return
						}
					}
					if a := fn["arguments"]; truthy(a) {
						if s := asStr(a); s != "" {
							tc.args = append(tc.args, s)
						}
						if !yield(respFrame("response.function_call_arguments.delta", map[string]any{
							"type":    "response.function_call_arguments.delta",
							"item_id": respID, "output_index": len(toolOrder), "delta": a,
						})) {
							return
						}
					}
				}

				if fr := choice["finish_reason"]; truthy(fr) {
					finishReason = asStr(fr)
				}
			}
		}

		if !started {
			if !emitStart() {
				return
			}
		}

		if len(reasoning) > 0 {
			joined := strings.Join(reasoning, "")
			if !yield(respFrame("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "item_id": respID,
				"output_index": 0, "summary_index": 0, "text": joined,
			})) {
				return
			}
			if !yield(respFrame("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{
					"type": "reasoning", "status": "completed",
					"summary": []any{map[string]any{
						"type": "summary_text", "text": joined,
					}},
					"content": []any{},
				},
			})) {
				return
			}
		}

		joinedText := strings.Join(text, "")
		if !yield(respFrame("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": respID,
			"output_index": 0, "content_index": 0, "text": joinedText,
		})) {
			return
		}
		if !yield(respFrame("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": respID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{
				"type": "output_text", "text": joinedText, "annotations": []any{},
			},
		})) {
			return
		}
		if !yield(respFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": joinedText, "annotations": []any{},
				}},
			},
		})) {
			return
		}

		for offset, key := range toolOrder {
			call := toolCalls[key]
			args := strings.Join(call.args, "")
			idx := offset + 1
			if !yield(respFrame("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": respID,
				"output_index": idx, "arguments": args,
			})) {
				return
			}
			if !yield(respFrame("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": idx,
				"item": map[string]any{
					"type": "function_call", "status": "completed",
					"call_id": call.id, "name": call.name, "arguments": args,
				},
			})) {
				return
			}
		}

		yield(respFrame("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": respID, "object": "response",
				"created_at": time.Now().Unix(),
				"status":     "completed", "model": model,
				"output":        responsesFinalOutput(joinedText, reasoning, toolCalls, toolOrder),
				"output_text":   joinedText,
				"usage":         responsesUsage(usage),
				"finish_reason": finishReason,
				"error":         nil,
			},
		}))
	}
}

func responsesFinalOutput(text string, reasoning []string,
	toolCalls map[float64]*respToolCall, toolOrder []float64) []any {
	out := []any{}
	if len(reasoning) > 0 {
		joined := strings.Join(reasoning, "")
		out = append(out, map[string]any{
			"type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": joined}},
			"content": []any{},
		})
	}
	out = append(out, responsesMessageItem(text))
	for _, key := range toolOrder {
		call := toolCalls[key]
		out = append(out, map[string]any{
			"type": "function_call", "status": "completed",
			"call_id": call.id, "name": call.name,
			"arguments": strings.Join(call.args, ""),
		})
	}
	return out
}
