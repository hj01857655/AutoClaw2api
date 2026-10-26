// AutoClaw2api-go 入口：装配配置、日志、凭据源与上游通道。
//
// desktop 凭据源永远构造（/health 与面板的 account 字段读它的缓存），
// oauth 源仅当 AUTOCLAW_CRED_SOURCE=oauth；两者可并存，互不干扰。
package main

import (
	"fmt"
	"log"
	"net/http"

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

	store := lanes.NewCredentialStore(cfg.ClawBase, cfg.AccountDir)
	var oauthStore *oauth.Store
	if cfg.CredSource == "oauth" {
		oauthStore = oauth.NewStore(cfg.OAuthDir, cfg.AuthBase, cfg.AppVersion,
			cfg.Platform, config.AppID, cfg.AppKey, cfg.OAuthExtraHosts, cfg.Port, logger)
	}
	acct := account.New(cfg.AccountBase, cfg.AppVersion, cfg.Platform,
		config.AppID, cfg.AppKey)

	tokenFn := func() (string, error) {
		if oauthStore != nil {
			return oauthStore.Get()
		}
		cred, err := store.Get()
		if err != nil {
			return "", err
		}
		return cred.AccessToken, nil
	}
	up := upstream.New(cfg.UpstreamBase, config.UpstreamPath, cfg.AppVersion,
		cfg.Platform, config.AppID, cfg.AppKey, tokenFn, store.Invalidate, logger)

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
	srv.Store = store
	srv.OAuth = oauthStore
	srv.Account = acct

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
	if cred, err := store.Get(); err == nil {
		acc := cred.Account
		if acc == "" {
			acc = "?"
		}
		if len(acc) > 12 {
			acc = acc[:12]
		}
		fmt.Printf("凭据: account=%s %s\n", acc, lanes.FmtExp(cred.AccessToken))
	} else {
		fmt.Printf("凭据不可用: %s\n", err.Error())
		fmt.Println("  → 确认 AutoClaw 桌面端已登录，且以同一 Windows 用户身份运行本服务")
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
