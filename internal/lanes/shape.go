// lanes 的请求整形部分，对应 Python 版 lanes/autoclaw.py 的
// neutralize_foreign_identity / sanitize_body / ensure_banner / build_request /
// auth_sign_headers / client_identity_headers。
package lanes

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Banner2X 是 main.cjs ZWORK_DEFAULT_SYSTEM_PROMPT 的首句。上游对 system 做
// 包含匹配，后面四段（FILE_CITATION / OUTPUT_LANGUAGE / USER_COMMUNICATION /
// DECISION_AUTONOMY）是应用自己的输出规范，不过网关。
const Banner2X = "You are AutoClaw. Answer the user directly and concisely."

// ForeignIdentityPhrases 是 2.x 上游的 system-prompt denylist（引用级证据，来自
// glmproxy shape.js 的实测矩阵）。大小写不敏感的字面子串匹配；命中即 406 空 body。
//
// 表顺序即替换顺序，必须与 Python 版 FOREIGN_IDENTITY_PHRASES 逐行一致：
// "you are claude code" 排在最前，所以 "You are Claude Code, built on Anthropic's
// Claude Agent SDK" 先被整句替换成 "You are AutoClaw, built on Anthropic's
// AutoClaw"，之后 claude agent sdk 那条已不再命中。换序会产出不同文本。
var ForeignIdentityPhrases = []string{
	"you are claude code",
	"you are cursor",
	"you are cline",
	"you are codex",
	"you are qwen code",
	"you are opencode",
	"you are an interactive cli tool that helps users with software engineering tasks",
	"you are an interactive agent in traecode",
	"claude agent sdk",
}

const foreignIdentityReplacement = "You are AutoClaw"

// foreignIdentityReplacements 给「补语而非身份主语」的条目单独指定替换值，
// 否则 "built on Anthropic's Claude Agent SDK" 会变成语法不通的句子。
var foreignIdentityReplacements = map[string]string{
	"claude agent sdk": "AutoClaw",
}

// ForbiddenBodyFields：上游拒绝客户端自带 max_tokens 时回的 406 是空 body，与其它
// 406 无法从响应体区分——所以这三个字段在发出前就必须消失，不能靠响应码事后补救。
// max_output_tokens 是 Responses 协议的同名字段，一并禁发。
var ForbiddenBodyFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// StripBodyFields 是上游不接受、直接剥掉的字段（OpenAI 客户端普遍会带）。
var StripBodyFields = []string{
	"frequency_penalty", "presence_penalty", "logit_bias",
	"logprobs", "top_logprobs", "n", "seed", "user",
	"parallel_tool_calls", "store", "metadata", "service_tier",
}

// NeutralizeForeignIdentity 把外来 harness 身份句中和掉，返回 (新文本, 命中短语)。
// 就地中和而不是把整段 system 降级成 user——后者（glmproxy 的 quarantine）会改变
// 客户端的语义，这里只做最小必要修改。
func NeutralizeForeignIdentity(text string) (string, []string) {
	if text == "" {
		return text, nil
	}
	var hits []string
	low := strings.ToLower(text)
	for _, phrase := range ForeignIdentityPhrases {
		if !strings.Contains(low, phrase) {
			continue
		}
		hits = append(hits, phrase)
		repl := foreignIdentityReplacement
		if r, ok := foreignIdentityReplacements[phrase]; ok {
			repl = r
		}
		// 大小写不敏感替换，保留原文其余部分
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(phrase))
		text = re.ReplaceAllString(text, repl)
		low = strings.ToLower(text)
	}
	return text, hits
}

// StripProviderPrefix "zai_auto" -> "auto"；无前缀原样返回。
// 上游按 X-Request-Model 路由，body 里的 model 不校验。
func StripProviderPrefix(model string) string {
	for _, sep := range []string{"_", "-"} {
		if i := strings.Index(model, sep); i >= 0 {
			head, tail := model[:i], model[i+1:]
			if slices.Contains([]string{"zai", "zaicoding", "tdpsk", "zhipu"}, head) {
				return tail
			}
		}
	}
	return model
}

// SanitizeBody OpenAI 请求体 → AutoClaw 上游请求体。
// 三件事：① 删 max_tokens 族（406 根因）② 剥上游不认识的字段
// ③ model 去 provider 前缀。
func SanitizeBody(payload map[string]any, model string) map[string]any {
	body := make(map[string]any, len(payload))
	for k, v := range payload {
		if slices.Contains(StripBodyFields, k) {
			continue
		}
		body[k] = v
	}
	for _, f := range ForbiddenBodyFields {
		delete(body, f)
	}
	body["model"] = StripProviderPrefix(model)
	return body
}

// EnsureBanner 保证 system 里含 2.x banner，并中和外来身份句。
// 返回 (messages, injected, hits)：
//   - 已有 system：banner 缺失则前置，身份句就地中和
//   - 无 system：新建一条只带 banner 的 system，放在最前
func EnsureBanner(messages []any) (out []any, injected bool, hits []string) {
	out = make([]any, 0, len(messages)+1)
	for _, mi := range messages {
		m, ok := mi.(map[string]any)
		if !ok {
			out = append(out, mi)
			continue
		}
		// 浅拷贝：Python 版 [dict(m) for m in messages]，不改调用方的对象
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out = append(out, cp)
		if asStr(cp["role"]) != "system" {
			continue
		}
		switch content := cp["content"].(type) {
		case string:
			newText, found := NeutralizeForeignIdentity(content)
			if len(found) > 0 {
				cp["content"] = newText
				hits = append(hits, found...)
			}
			if !strings.Contains(newText, Banner2X) {
				cp["content"] = Banner2X + "\n\n" + newText
				injected = true
			}
		case []any:
			// 多段 content：只处理 text 段
			for _, pi := range content {
				part, ok := pi.(map[string]any)
				if !ok || asStr(part["type"]) != "text" {
					continue
				}
				text := asStr(part["text"])
				newText, found := NeutralizeForeignIdentity(text)
				if len(found) > 0 {
					part["text"] = newText
					hits = append(hits, found...)
					text = newText
				}
				if !strings.Contains(text, Banner2X) {
					part["text"] = Banner2X + "\n\n" + text
					injected = true
				}
			}
		}
	}

	hasSystem := false
	for _, mi := range out {
		if m, ok := mi.(map[string]any); ok && asStr(m["role"]) == "system" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		out = append([]any{map[string]any{"role": "system", "content": Banner2X}}, out...)
		injected = true
	}
	return out, injected, hits
}

// AuthSignHeaders 每次现算（X-Auth-Sign 带时间戳）：md5(appId&timestamp&appKey)。
func AuthSignHeaders(appID, appKey string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sum := md5.Sum([]byte(appID + "&" + ts + "&" + appKey))
	return map[string]string{
		"X-Auth-Appid":     appID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      hex.EncodeToString(sum[:]),
	}
}

// ClientIdentityHeaders 官方客户端的静态身份头。
// platform 对应 main.cjs managedPlatform()：win32->win, darwin->mac。
// 注意：绝不为「更像官方」而补 X-Harness-Type —— 2.x 上它是 406。
func ClientIdentityHeaders(version, platform string) map[string]string {
	return map[string]string{
		"X-Tm":          platform,
		"X-Version":     version,
		"X-Product":     "autoclaw",
		"X-Channel":     "official",
		"X-Lang":        "zh-CN",
		"X-Client-Type": "pc",
		"x_trace_id":    "autoclaw-desktop",
	}
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}
