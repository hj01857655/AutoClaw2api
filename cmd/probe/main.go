// probe 账号面探针：用 oauth 凭据查任务 / 余额 / 流水 / 过期积分（只读）。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"autoclaw2api/internal/account"
	"autoclaw2api/internal/config"
	"autoclaw2api/internal/oauth"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := oauth.NewStore(cfg.OAuthDir, cfg.AuthBase, cfg.CNAuthBase,
		cfg.AppVersion, cfg.Platform, config.AppID, cfg.AppKey, cfg.OAuthExtraHosts, cfg.Port, logger)
	cred, ok := store.Peek()
	if !ok {
		fmt.Println("尚未授权：先起网关（go run .），打开 /admin 用 OAuth 授权登录")
		os.Exit(1)
	}
	fmt.Printf("账号: %s…  token 长度: %d  过期: %s\n",
		first12(cred.Account), len(cred.AccessToken), oauth.FmtExp(cred.AccessToken))

	token := cred.AccessToken
	if len(token) > 7 && token[:7] == "Bearer " {
		// 已带前缀
	} else {
		token = "Bearer " + token
	}

	client := account.New(cfg.AccountBase, cfg.AppVersion, cfg.Platform, config.AppID, cfg.AppKey)

	fmt.Println("\n=== 任务列表 ===")
	tasks, err := client.ListTasks(token)
	if err != nil {
		fmt.Println("任务列表失败:", err)
	} else {
		for _, t := range tasks {
			fmt.Printf("  %-40s %8d 分  claimable=%v  title=%s\n",
				t.TaskID, t.Points, t.Claimable, trunc(t.Title, 30))
		}
	}

	fmt.Println("\n=== 积分余额 ===")
	bal, err := client.WalletBalance(token)
	if err != nil {
		fmt.Println("余额失败:", err)
	} else {
		for _, w := range bal.Wallets {
			fmt.Printf("  %-12s %12s\n", w.Label, formatNum(w.Balance))
		}
		fmt.Printf("  合计: %s\n", formatNum(bal.Total))
	}

	fmt.Println("\n=== 积分流水（最近 10 笔）===")
	ledgers, err := client.CreditLedgers(token, 1, 10)
	if err != nil {
		fmt.Println("流水失败:", err)
	} else {
		for _, l := range ledgers {
			fmt.Printf("  %12s  %-6s  %s\n", formatNum(l.Amount), l.Kind, trunc(l.Description, 40))
		}
	}

	fmt.Println("\n=== 即将过期积分 ===")
	exp, err := client.PointsExpiring(token)
	if err != nil {
		fmt.Println("过期积分失败:", err)
	} else if exp == nil {
		fmt.Println("  无即将过期的积分")
	} else {
		fmt.Printf("  %s 分（%s）\n", formatNum(exp.Expiring), exp.Text)
	}
}

func first12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func formatNum(n int64) string {
	return strconv.FormatInt(n, ',')
}
