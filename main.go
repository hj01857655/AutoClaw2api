// AutoClaw2api-go 入口：装配配置、日志、凭据源与上游通道。
//
// 凭据源只有 oauth 自主授权一种——独立网关，不读也不写 AutoClaw 桌面端
// 的任何文件。
package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"autoclaw2api/internal/account"
	"autoclaw2api/internal/config"
	"autoclaw2api/internal/dump"
	"autoclaw2api/internal/handler"
	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/oauth"
	"autoclaw2api/internal/upstream"
)

func main() {
	cfg := config.Load()
	logger := handler.NewLogger(cfg.StateDir)
	dw := dump.New(cfg.DumpOn, cfg.DumpDir, cfg.DumpMaxFiles)

	oauthStore := oauth.NewStore(cfg.OAuthDir, cfg.AuthBase, cfg.CNAuthBase,
		cfg.AppVersion, cfg.Platform, config.AppID, cfg.AppKey, cfg.OAuthExtraHosts, cfg.Port, logger)
	acct := account.New(cfg.AccountBase, cfg.AppVersion, cfg.Platform,
		config.AppID, cfg.AppKey)
	// 账号面双域：任务/签到/钱包跟激活账号的域走（与 chat 分流同一 RegionFn）
	acct.CNBase = cfg.CNAuthBase
	acct.RegionFn = oauthStore.ActiveRegion
	acct.DumpDir = cfg.DumpDir
	if acct.DumpDir == "" {
		acct.DumpDir = "dumps"
	}

	tokenFn := func() (string, error) { return oauthStore.Get() }
	up := upstream.New(cfg.UpstreamBase, config.UpstreamPath, cfg.AppVersion,
		cfg.Platform, config.AppID, cfg.AppKey, tokenFn, oauthStore.Invalidate, logger)
	// 双域路由：国际账号 → .env 指定域；国内账号 → managed gateway 域（路径无 /v1）
	up.CNBase = strings.TrimRight(cfg.CNUpstreamBase, "/")
	up.CNPath = config.CNUpstreamPath
	up.RegionFn = oauthStore.ActiveRegion
	// 多账号轮转：429/持续 401 时把当前账号放进冷却表，Get() 下次自动换
	// 备选落盘账号；成功清冷却。
	up.AccountLimited = func() {
		oauthStore.MarkRateLimited(oauthStore.ActiveAccount(), cfg.RotateCooldown)
	}
	up.AccountOK = func() { oauthStore.ClearCooldown(oauthStore.ActiveAccount()) }

	fetchModels := func() []string {
		token, err := tokenFn()
		if err != nil {
			logger.Warn("模型目录拉取失败（无凭据）",
				"err", err.Error())
			return nil
		}
		return lanes.FetchModels(upstream.NewHTTPClient(), cfg.UpstreamBase, token,
			cfg.AppVersion, cfg.Platform, config.AppID, cfg.AppKey)
	}

	srv := handler.New(cfg, logger, dw, fetchModels)
	srv.Up = up
	srv.OAuth = oauthStore
	srv.Account = acct
	if cfg.AutoClaim {
		go srv.StartAutoClaim() // 每日自动签到（daily_signin，幂等；AUTOCLAW_AUTO_CLAIM=0 关）
	}

	schemeHost := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	fmt.Printf("AutoClaw2api 启动: http://%s\n", schemeHost)
	fmt.Printf("OpenAI Base URL:    http://%s/v1\n", schemeHost)
	fmt.Printf("Anthropic Base URL: http://%s\n", schemeHost)
	fmt.Printf("上游: %s\n", cfg.UpstreamBase)
	if cfg.APIKey != "" {
		fmt.Println("本地鉴权: 已开启（API_KEY）")
	} else {
		fmt.Println("本地鉴权: 未开启（仅回环）")
	}
	if token, err := tokenFn(); err == nil && token != "" {
		acc := "?"
		if cred, ok := oauthStore.Peek(); ok && cred.Account != "" {
			acc = cred.Account
		}
		if len(acc) > 12 {
			acc = acc[:12]
		}
		fmt.Printf("凭据: account=%s %s\n", acc, oauth.FmtExp(token))
	} else {
		fmt.Printf("凭据不可用: %s\n", err.Error())
		fmt.Printf("  → 打开 http://%s/admin 用 OAuth 授权登录\n", schemeHost)
	}
	fmt.Printf("模型目录: %s\n", joinOr(srv.ListModels()))

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatal(err)
	}
}

func joinOr(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
