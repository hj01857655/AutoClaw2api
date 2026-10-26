// 自动签到：每天首次尝试领 daily_signin，成功或已签后当天不再重复。
//
// 设计要点：
//   - 签到端点本身幂等（重复调只是 Already），所以循环不怕重，怕的是漏；
//   - 失败（网络错/上游错）不置 lastClaimDay，下个 tick 重试；
//   - 无凭据时静默返回——启动早期还没登录是常态，不值得刷日志；
//   - 只在 loop goroutine 里读写 lastClaimDay，无并发。
package handler

import (
	"log/slog"
	"time"

	"autoclaw2api/internal/account"
)

// autoClaimTick 检查间隔。日期一变就会在下个 tick 触发当天的签到，
// 所以真正的「每天一次」粒度是这个 tick，不是定时器精度。
const autoClaimTick = 30 * time.Minute

// StartAutoClaim 起后台签到循环（main 里 go srv.StartAutoClaim()）。
// 关闭条件由 config.AutoClaim 决定，main 里判断。
func (s *Server) StartAutoClaim() {
	s.autoClaimOnce()
	t := time.NewTicker(autoClaimTick)
	defer t.Stop()
	for range t.C {
		s.autoClaimOnce()
	}
}

func (s *Server) autoClaimOnce() {
	today := time.Now().Format("2006-01-02")
	if s.lastClaimDay == today {
		return
	}
	token, err := s.accountToken()
	if err != nil {
		return // 无凭据：静默，等下个 tick
	}
	result, err := s.Account.ClaimTask(token, account.TaskDailySignin)
	if err != nil {
		s.Log.Warn("自动签到失败（稍后重试）", slog.String("err", err.Error()))
		return
	}
	// Rejected 也置位：被拒说明这个账号今天不该在这领，重试没有意义
	s.lastClaimDay = today
	switch {
	case result.Completed:
		s.Log.Info("自动签到完成", slog.Int64("points", result.Points))
	case result.Already:
		s.Log.Info("自动签到：今日已签（幂等跳过）")
	default:
		s.Log.Info("自动签到未得分", slog.String("message", result.Message))
	}
}
