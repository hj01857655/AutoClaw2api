// lanes 的上游调用部分，对应 Python 版 lanes/autoclaw.py 的
// build_request / fetch_models。
package lanes

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// 超时对应 Python _HTTP_TIMEOUT = (10, 300)：connect 10s、read 300s。
// Go 的 http.Client.Timeout 不好按请求区分（流式要 300s+，账号面要 20s），
// 故各调用点自建 client。
const (
	DialTimeout    = 10 * time.Second
	ReadTimeout    = 300 * time.Second
	AccountTimeout = 20 * time.Second // main.cjs 各超时 15e3，放宽到 20s
)

// BuildRequest 组装 (headers, body, meta)。
//
// payload 是**已翻译成 Chat 的请求体**（OpenAI 客户端原样、Anthropic/Responses
// 经 adapters/ 转过）；406 三条硬约束在这里落实。
func BuildRequest(payload map[string]any, token, version, platform, appID, appKey string) (map[string]string, map[string]any, map[string]any) {
	model := asStr(payload["model"])
	if model == "" {
		model = "zai_auto" // 连「空名补默认」都只有这一处：路由层不再兜，避免重复
	}
	var messages []any
	if l, ok := payload["messages"].([]any); ok {
		messages = l
	}
	bannered, injected, hits := EnsureBanner(messages)

	body := make(map[string]any, len(payload))
	for k, v := range payload {
		body[k] = v
	}
	body["messages"] = bannered
	body = SanitizeBody(body, model)

	reqID := HexID(32)
	headers := map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "*/*",
		"Authorization":   token,
		"X-Authorization": token,
		// X-Request-Model 决定上游路由，必须是完整 id（原样透传，不解析、不映射）
		"X-Request-Model": model,
		"X-Request-Id":    reqID,
		"X-Trace-Id":      reqID,
		"X-Client-Type":   "pc",
		"X-Agent-Id":      "main",
	}
	for k, v := range AuthSignHeaders(appID, appKey) {
		headers[k] = v
	}
	for k, v := range ClientIdentityHeaders(version, platform) {
		headers[k] = v
	}
	meta := map[string]any{"injected": injected, "hits": hits, "model": model}
	return headers, body, meta
}

// FetchModels 拉服务端模型目录；失败返回 nil（调用方用兜底清单）。
//
// 该 host 有间歇性 TLS EOF（实测约三成概率），所以重试三次再放弃——模型目录
// 只在启动和首次请求时拉，重试成本远低于落进兜底清单的代价。
// url 形如 <UPSTREAM_BASE>/… 时按 Python 的规则替换成模型配置端点。
func FetchModels(httpc *http.Client, upstreamBase, token, version, platform, appID, appKey string) []string {
	url := strings.Replace(upstreamBase, "/autoclaw-proxy/proxy/autoclaw", "", 1) +
		"/autoclaw-proxy/proxy/autoclaw-model-config"
	headers := map[string]string{"Accept": "*/*", "Authorization": token, "X-Authorization": token}
	for k, v := range AuthSignHeaders(appID, appKey) {
		headers[k] = v
	}
	for k, v := range ClientIdentityHeaders(version, platform) {
		headers[k] = v
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil // 非 200 不重试（与 Python 一致：直接返回 []）
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		var data any
		if json.Unmarshal(raw, &data) != nil {
			return nil
		}
		return ExtractModelIDs(data)
	}
	return nil
}
