// Package adapters 对应 Python 版 adapters/：协议桥（Anthropic Messages /
// OpenAI Responses ⇄ Chat Completions）。
//
// 与 Cline2api-go 的同名包是**同源不同形**：两边的 Python 适配器已经有实质差异
// （AutoClaw 版非流式 JSON 会产出 thinking block、出口删 max_tokens 族、文本段用
// \n 连接），所以这里按 AutoClaw 的 Python 源码逐条移植，不直接复制。
//
// 输入契约也与 Cline 版不同：AutoClaw 的适配器吃的是**本地规范化的 Chat SSE 行**
// （handler.chatSSELines 的产出，带 "data: " 前缀），与 Python 的
// chat_sse_to_anthropic(core._chat_sse_lines(...)) 完全一致 —— 保留那一次
// JSON 编解码往返，是为了让帧序列与 Python 逐字节可比，金样本测试才有意义。
package adapters

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// hexID 生成 n 个 hex 字符的随机 id（对应 Python uuid4().hex[:n]）。
func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

// marshalCompact JSON 紧凑序列化，不转义 HTML 字符（对齐 Python
// json.dumps(..., ensure_ascii=False, separators=(",",":"))）。
func marshalCompact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// truthy 对应 Python 的 `if x:` 判定：None/False/0/""/空集合为假。
func truthy(v any) bool {
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

// AnthropicErrorBody Anthropic 错误响应体（adapters/anthropic.py error_body）。
func AnthropicErrorBody(errType, message string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	}
}

// ResponsesErrorBody Responses 的错误结构（与 Chat 的 error 对象形状不同：
// 外层有 type；Python adapters/responses.py error_body 里 code 恒为 None）。
func ResponsesErrorBody(errType, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{"type": errType, "code": nil, "message": message},
	}
}
