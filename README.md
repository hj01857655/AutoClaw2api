# AutoClaw2api (Go)

![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white) ![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux-lightgrey) ![Type](https://img.shields.io/badge/type-API%20Gateway-blue)

AutoClaw（智谱 AutoGLM 桌面端）→ OpenAI / Anthropic 兼容 API 的 Go 网关，纯 stdlib 零第三方依赖。

## 构建 / 运行

```bash
go build -o autoclaw2api .
./autoclaw2api          # 默认 127.0.0.1:8378
```

```bash
go run ./cmd/probe      # 账号面探针：任务列表 / 积分 / 余额（只读，验证凭据）
```

## 凭据来源

网关是独立服务：凭据只有 **登录自主获取** 一种来源，打开
`http://127.0.0.1:8378/admin` 操作（或直接调 `/admin/api/*`），DPAPI 加密落盘
`<状态目录>/.cred/`。不依赖、不读取、不回写 AutoClaw 桌面端的任何文件。
两种登录通道，对应两套账号体系：

- **国际版（Google / z.ai 账号）**：网页 OAuth 授权（device flow + 本地回调）。
- **国内版（+86 手机号）**：手机验证码登录，两步 API，无浏览器环节——发码 → 收到短信后提交验证码，成功即落盘并激活：

```bash
curl -X POST http://127.0.0.1:8378/admin/api/oauth/sms/send  -d '{"phone":"13800138000"}'
curl -X POST http://127.0.0.1:8378/admin/api/oauth/sms/verify -d '{"phone":"13800138000","code":"123456"}'
```

> 验证码为全数字；`agent-login` 端点按数字类型校验，字符串直接 400001（`code:0` 也会被当空值拒收）。

**多账号落盘**：每个账号一份 `<状态目录>/.cred/<sha256(account)>.bin`，面板上换个号再授权即并存且**自动激活新号**（同号重授权=覆盖），`/admin/api/accounts` 列表 / `select` 切换（网关立刻用新号，无需重启）/ `delete` 删除。旧版单文件 `oauth-credential.bin` 自动兼容读取，下次落盘即迁移到分账号布局。

token 过期自动用 refreshToken 续期；上游 401 时网关自动重读凭据并重试一次。

**多账号自动轮转**：上游 429（限流）或连续 401（账号被拒）时，当前账号自动进入冷却表（默认 10 分钟，`AUTOCLAW_COOLDOWN_MIN` 可调），网关立即换下一个未冷却的落盘账号重试，请求不失败；成功即清冷却。冷却表在内存里，不动激活账号的落盘状态，重启清零。面板「报表」页签可看冷却中的账号。

**请求报表**：`GET /admin/api/stats`（面板「报表」页签）按模型聚合请求数 / 成功率 / token 消耗 / 平均耗时，内存态聚合（重启清零）。

## 接口

| 端点 | 说明 |
|---|---|
| `GET /health` | 健康检查（含凭据源、账号、到期时间） |
| `GET /v1/models` | 模型列表（`zai_auto` / `zai_auto-fast` / `zaicoding_glm-5.3` / `zai_glm-5.3-flash`） |
| `POST /v1/chat/completions` | OpenAI Chat Completions（流式 + 工具调用） |
| `POST /v1/messages` | Anthropic Messages（Claude Code / Cline 直连） |
| `POST /v1/responses` | OpenAI Responses |
| `GET /`、`/admin` | Web 管理面板（内置，开箱即用）：OAuth / 短信登录、账号切换、余额 / 积分明细、任务领取、自检与模型探测。状态目录放一份自定义 `panel.html` 可覆盖内置版 |

`/admin/api/*` 面板后端：`overview` / `credits` / `ledgers` / `claim` / `tasks` / `oauth/begin|status|cancel` / `oauth/sms/send|verify`（国内版验证码登录）/ `accounts` / `accounts/select` / `accounts/delete` / `accounts/import` / `refresh-token` / `stats`（请求报表 + 账号冷却）/ `self-check` / `probe-models`。

## 配置（环境变量 > `.env` > 默认值）

| 变量 | 默认 | 说明 |
|---|---|---|
| `API_KEY` | 空 | 非空 = 入口鉴权（Bearer / x-api-key），chat 三协议与 `/admin/api/*` 共用 |
| `AUTOCLAW_ADMIN_KEY` | 空 | 非空 = `/admin/api/*` 改用这把独立钥匙——发出去的 chat Key 进不了后台，公网部署时两把钥匙必须不同；未设回退 `API_KEY` 保持旧行为 |
| `HOST` / `PORT` | `127.0.0.1` / `8378` | 监听地址。凭据绑死本机 DPAPI，默认仅回环 |
| `AUTOCLAW2API_STATE_DIR` | exe 所在目录 | panel.html / request.log / dumps/ / .cred/ 落点 |
| `AUTOCLAW_APP_VERSION` / `AUTOCLAW_PLATFORM` / `AUTOCLAW_APP_KEY` | `2.1.0` / `win` / 内置值 | 客户端身份参数（对齐桌面端 2.x） |
| `AUTOCLAW_UPSTREAM_BASE` / `AUTOCLAW_ACCOUNT_BASE` / `AUTOCLAW_AUTH_BASE` | 内置值（国内域 zhipuai.cn） | 国际域三件套：上游 / 账号面 / OAuth 端点 base。**国际版账号必须覆盖为 `https://autoglm-api.autoglm.ai`**（默认域打 chat 401），仓库根 `.env` 已预置 |
| `AUTOCLAW_CN_UPSTREAM_BASE` | `https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw` | 国内版 chat 域（managed gateway）。**双域按激活账号的 JWT `source_id` 自动路由，无需手动切换** |
| `AUTOCLAW_CN_AUTH_BASE` | `https://autoglm-api.zhipuai.cn` | 国内版账号面 base——手机验证码登录与国内号的任务/签到/钱包端点只挂国内域，独立于国际域三件套 |
| `AUTOCLAW_DEFAULT_MODEL` | `zai_auto` | 请求未带 model 时的兜底模型 |
| `AUTOCLAW_AUTO_CLAIM` | 开 | 每日自动签到（`daily_signin`，幂等）：启动与日期变化时自动领取，设 `0`/`off`/`false` 关闭 |
| `AUTOCLAW_COOLDOWN_MIN` | `10` | 多账号轮转的冷却时长（分钟）：429/持续 401 后该账号暂停使用的时长 |
| `AUTOCLAW_OAUTH_DIR` / `AUTOCLAW_OAUTH_EXTRA_HOSTS` | `<状态目录>/.cred` | oauth 凭据目录 / OAuth 回调白名单附加 host |
| `DUMP` / `DUMP_DIR` / `DUMP_MAX_FILES` | 开 / `dumps` / 20 | 上游请求 / 响应落盘（排障用） |

## 国内版与国际版

AutoClaw 有两套互相独立的账号体系，登录方式、域名、端点都不同。网关两条登录通道都支持，凭据落同一套多账号库（`.cred/<账号>.bin`），两版账号可并存、可在面板/`accounts/select` 间切换：

| | 国际版 | 国内版 |
|---|---|---|
| 账号 | Google / z.ai 账号 | +86 手机号 |
| 登录方式 | 网页 OAuth：`/userapi/overseasv1/{p}-oauth-url` → `-oauth-login`（浏览器授权 + 本地回调） | 手机验证码：`/userapi/v1/agent-send-code` → `/userapi/v1/agent-login/`（两步 API，无浏览器环节） |
| 登录/账号面域 | `autoglm-api.autoglm.ai` | `autoglm-api.zhipuai.cn`（`AUTOCLAW_CN_AUTH_BASE`，独立于国际域三件套） |
| chat 域 | `autoglm-api.autoglm.ai`（`AUTOCLAW_UPSTREAM_BASE`） | `autoglm-acceleration-api.zhipuai.cn`（默认值即是） |
| chat 路径 | `/autoclaw-proxy/proxy/autoclaw[/v1]/chat/completions`（`/v1/` 可省） | 同左且不带 `/v1/`——与网关默认拼接一致 |
| 身份头参考值 | X-Channel=AutoClaw4、X-Version=1.18.1 | X-Channel=AutoClaw26、X-Version=2.1.0、X-Agent-Id=main |
| refresh | `/userapi/v1/refresh`（code 400002 回落 `/userapi/v1/agent-refresh`） | 同左，仅 host 优先序不同（国内号先打国内域） |

账号体系识别：refresh token 的 JWT `source_id` claim 以 `agent` 开头 = 国内（短信）账号，否则为国际 OAuth 账号。

**国内版验证状态（2026-10-27 实测，已全链路闭环）**：

- 发码端点 `agent-send-code` **实测打通**：真实手机号请求返回 code:0，短信正常下发；
- 验证码校验 `agent-login/` **实测到达业务层**：错误验证码返回 `630202 验证码错误`——签名或请求形状不对会是 400002 / 400001，走到验证码比对说明前置全对；注意 `code:0` 会被上游当空值回 400001，全数字验证码按数值发送；
- 真实手机号验证码登录 → token 落盘激活 → **国内域 chat 200**（正常推理回复），全链路闭环；
- 两套号的 token 不混域：国际 token 打国内 chat 域 401，反之亦然——网关已按激活账号的 JWT `source_id` 自动分流 chat 与账号面（任务/签到/钱包），无需手动切域。

网关实测两版对 X-Channel 均不做硬校验（发 `official` 两域皆 200），签到/任务端点两版同路径同形状，仅 base 域不同。

## 实现要点（实证结论）

- 上游头形态：签名三件套（Authorization + X-Auth-Appid/TimeStamp/Sign）+ 客户端身份头为必要条件；**User-Agent 必须显式设置**——上游 WAF 对 Go 默认 UA 或缺失 UA 直接 405
- 请求体经 `SanitizeBody`：剥 `zai_` 模型前缀、注入 2.x 身份 banner；`max_tokens` 族字段禁发（上游 400）
- 非流式请求由网关聚合上游 SSE；三协议共享同一上游与凭据
- Windows DPAPI 加解密在 `internal/winapi`；`LocalFree` 在 kernel32 不在 crypt32

## 协议文档

- `docs/dual-domain-protocol.md` —— 双域协议（国际 OAuth / 国内 SMS / 签名头 / 刷新轮换 / chat 约束）

## Docker 部署

```bash
cp .env.example .env   # 按需填写
docker compose up -d
```

镜像同时发布到 GHCR：`ghcr.io/hj01857655/autoclaw2api`（跟随 v* 标签发布，含 latest，linux/amd64 + linux/arm64）。

> 注：DPAPI 绑死 Windows 用户，容器内没有桌面端可读——默认 oauth 源正好是唯一通道；挂载持久卷保存 `.cred/`，重新部署不用再授权。

二进制发版：推送 `v*` 标签后 Actions 自动构建 linux / windows / darwin 的 amd64 + arm64 六平台产物并发布 Release。
