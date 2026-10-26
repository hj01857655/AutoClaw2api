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

## 凭据来源（`AUTOCLAW_CRED_SOURCE`）

| 值 | 说明 |
|---|---|
| `desktop`（默认） | 复用 AutoClaw 桌面端登录态（`%APPDATA%/AutoClaw-official` 下的 DPAPI 加密凭据）。要求桌面端已登录且同一 Windows 用户运行本服务 |
| `oauth` | 独立 OAuth 授权：打开 `http://127.0.0.1:8378/admin` 点登录走浏览器授权（device flow + 本地回调），凭据同样 DPAPI 加密落盘 `<状态目录>/.cred/`，与桌面端互不干扰 |

**oauth 源支持多账号落盘**：每个账号一份 `<状态目录>/.cred/<sha256(account)>.bin`，面板上换个号再授权即并存（同号重授权=覆盖），`/admin/api/accounts` 列表 / `select` 切换（网关立刻用新号，无需重启）/ `delete` 删除。旧版单文件 `oauth-credential.bin` 自动兼容读取，下次落盘即迁移到分账号布局。desktop 源为只读复用（绝不回写 AutoClaw 目录）。

token 过期自动用 refreshToken 续期；上游 401 时网关自动重读凭据并重试一次。

## 接口

| 端点 | 说明 |
|---|---|
| `GET /health` | 健康检查（含凭据源、账号、到期时间） |
| `GET /v1/models` | 模型列表（`zai_auto` / `zai_auto-fast` / `zaicoding_glm-5.3` / `zai_glm-5.3-flash`） |
| `POST /v1/chat/completions` | OpenAI Chat Completions（流式 + 工具调用） |
| `POST /v1/messages` | Anthropic Messages（Claude Code / Cline 直连） |
| `POST /v1/responses` | OpenAI Responses |
| `GET /`、`/admin` | Web 管理面板：OAuth 登录、余额 / 积分明细、任务领取 |

`/admin/api/*` 面板后端：`overview` / `credits` / `ledgers` / `claim` / `tasks` / `oauth/begin|status|cancel` / `accounts` / `accounts/select` / `accounts/delete` / `refresh-token` / `self-check` / `probe-models`。

## 配置（环境变量 > `.env` > 默认值）

| 变量 | 默认 | 说明 |
|---|---|---|
| `API_KEY` | 空 | 非空 = 入口鉴权（Bearer / x-api-key） |
| `HOST` / `PORT` | `127.0.0.1` / `8378` | 监听地址。凭据绑死本机 DPAPI，默认仅回环 |
| `AUTOCLAW2API_STATE_DIR` | exe 所在目录 | panel.html / request.log / dumps/ / .cred/ 落点 |
| `AUTOCLAW_CRED_SOURCE` | `desktop` | 凭据源（见上表） |
| `AUTOCLAW_BASE` | `%APPDATA%/AutoClaw-official` | desktop 源的用户数据根 |
| `AUTOCLAW_ACCOUNT` | 自动选 mtime 最新 | 多账号时指定账号目录名（sha256(accountId)） |
| `AUTOCLAW_APP_VERSION` / `AUTOCLAW_PLATFORM` / `AUTOCLAW_APP_KEY` | `2.1.0` / `win` / 内置值 | 客户端身份参数（对齐桌面端 2.x） |
| `AUTOCLAW_UPSTREAM_BASE` / `AUTOCLAW_ACCOUNT_BASE` / `AUTOCLAW_AUTH_BASE` | 内置值 | 上游 / 账号面 / OAuth 端点 base |
| `AUTOCLAW_DEFAULT_MODEL` | `zai_auto` | 请求未带 model 时的兜底模型 |
| `AUTOCLAW_OAUTH_DIR` / `AUTOCLAW_OAUTH_EXTRA_HOSTS` | `<状态目录>/.cred` | oauth 凭据目录 / OAuth 回调白名单附加 host |
| `DUMP` / `DUMP_DIR` / `DUMP_MAX_FILES` | 开 / `dumps` / 20 | 上游请求 / 响应落盘（排障用） |

## 实现要点（实证结论）

- 上游头形态：签名三件套（Authorization + X-Auth-Appid/TimeStamp/Sign）+ 客户端身份头为必要条件；**User-Agent 必须显式设置**——上游 WAF 对 Go 默认 UA 或缺失 UA 直接 405
- 请求体经 `SanitizeBody`：剥 `zai_` 模型前缀、注入 2.x 身份 banner；`max_tokens` 族字段禁发（上游 400）
- 非流式请求由网关聚合上游 SSE；三协议共享同一上游与凭据
- Windows DPAPI 加解密在 `internal/winapi`；`LocalFree` 在 kernel32 不在 crypt32

## Docker 部署

```bash
cp .env.example .env   # 按需填写
docker compose up -d
```

镜像同时发布到 GHCR：`ghcr.io/hj01857655/hj01857655/AutoClaw2api`（跟随 v* 标签发布，含 latest，linux/amd64 + linux/arm64）。

> 注：DPAPI 凭据绑死 Windows 用户，容器内 `desktop` 源不可用——Docker 部署请用 `AUTOCLAW_CRED_SOURCE=oauth` 并挂载持久卷。

二进制发版：推送 `v*` 标签后 Actions 自动构建 linux / windows / darwin 的 amd64 + arm64 六平台产物并发布 Release。
