# AutoClaw 双域协议

> 2026-10-26 实测锁定。

## 双供应商 / 双域

| | autoclaw（国际） | autoclaw-cn（国内） |
|---|---|---|
| 登录方式 | Google / z.ai 网页 OAuth | **手机验证码（+86），无 OAuth 授权链接** |
| 登录/账号面 host | https://autoglm-api.autoglm.ai | https://autoglm-api.zhipuai.cn |
| chat 域 | https://autoglm-api.autoglm.ai | **https://autoglm-acceleration-api.zhipuai.cn**（MANAGED_HOST，桌面端管理网关） |
| chat 路径 | /autoclaw-proxy/proxy/autoclaw/**v1**/chat/completions | /autoclaw-proxy/proxy/autoclaw/**chat/completions**（无 /v1） |
| X-Channel | AutoClaw4 | AutoClaw26 |
| X-Version / UA 版本 | 1.18.1 | 2.1.0 |
| token 刷新优先 host | GLOBAL_HOST | CN_HOST（SMS 账号） |

**桌面端管理域** autoglm-acceleration-api.zhipuai.cn（MANAGED_HOST）是第三个 host，
属于桌面端账号平面，与插件双域不同 —— 打错 host 即 401/不存在。

## 常量

```
APP_ID  = 100003
APP_KEY = 38d2391985e2369a5fb8227d8e6cd5e5
PRODUCT = autoclaw
CHANNEL = AutoClaw4（国际）/ AutoClaw26（国内）
CLIENT_VERSION = 1.18.1（国际）/ 2.1.0（国内）
X-Tm = linux-x64 | mac | win（裸 "linux" 被海外网关 403 {"message":"forbidden"}）
```

## 签名头（bizHeaders，账号面与 chat 共用）

```
X-Auth-Appid: 100003
X-Auth-TimeStamp: <秒级时间戳>
X-Auth-Sign: md5("100003&<ts>&38d2391985e2369a5fb8227d8e6cd5e5")
X-Version: <版本>            X-Product: autoclaw
X-Tm: <platformTm>           X-Channel: <渠道>
X-Trace-Id: <16 hex 随机>    X-Lang: zh-CN
Origin/Referer: http://localhost:18432
Authorization: Bearer <token>（带凭据时）
```

## 国内版 SMS 登录

```
POST {CN_HOST}/userapi/v1/agent-send-code
POST {CN_HOST}/userapi/v1/agent-login/        （注意尾斜杠）
```

## token 刷新（轮换 + host 匹配）

```
body: {source_id:"autoclaw", device_id:<JWT device_id claim>, refresh_token:<旧 refresh>}
第一跳 POST {host}/userapi/v1/refresh
  code==400002 → 第二跳 /userapi/v1/agent-refresh
host 顺序：JWT payload.source_id 以 "agent" 开头（SMS 账号）→ [CN_HOST, GLOBAL_HOST]
           否则（OAuth 账号）→ [GLOBAL_HOST, CN_HOST]
refreshToken 非一次性：旧值与新值都能再刷；响应 refresh 字段是 bool 不是 token
响应 data 内 access_token / refresh_token（snake_case）
banned 账号：拒绝但保留登录态（signIn:"kept"）
401/403 或 msg 含 invalid_grant/revoked/expired/登录已失效/token 失效 → 登录态已死
```

## 国际版 OAuth（仅 GLOBAL_HOST 提供）

```
POST {GLOBAL_HOST}/userapi/overseasv1/{vendor}-oauth-url
body: {source_id:"autoclaw", device_id, navigate_uri}
navigate_uri 白名单形态：http://localhost:<官方端口池端口>/auth/callback-<vendor>
  （随机端口 / 自定路径 → {"detail":"Redirect URI not registered for this client"}）
响应 data.oauth_url | url | login_url | auth_url | verify_url → authorize_url
authorize_url 必须带 state；回调原样带回，缺失/不符拒收（防回调劫持）
ali_captcha_verify_param（阿里验证码回执）可作为 body 附加项
```

## 上游约束（chat）

- 请求体禁 max_tokens / max_completion_tokens / max_output_tokens（406 空响应）
- system 必须含 2.x banner（`You are AutoClaw. Answer the user directly and concisely.`）
- X-Request-Model 必填且必须是上游模型 id
- 不发 X-Harness-Type（2.x 上 406）
- 外来身份句中和：`You are a Claude agent, built on Anthropic's Claude Agent SDK.` 等
  → "You are a coding assistant." / `the agent framework`（CN 网关对 "Claude Agent SDK" 直接 406）
- 上游模型（4 个）：zai_auto / zai_auto-fast / zaicoding_glm-5.3 / zai_glm-5.3-flash
  （zai_glm-5-turbo 与豆包 tdpsk_deepseek-* 已下线，调用 400 非法模型）
- reasoning variants：auto / auto-fast / glm-5.3 系均支持 low / high / max
- 内置服务端工具：apply_patch / image_generation / code_interpreter / local_shell / shell /
  web_search（externalWebAccess/filters.allowedDomains/searchContextSize/userLocation）/ mcp / tool_search

## 账号面（桌面端积分/任务）

```
GET  /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw      → 钱包余额
GET  /agent-assetmgr/api/v1/points/expiring                   → 即将过期积分
POST /userapi/v1/user-profile                                 → user_name/user_phone/user_id
```
