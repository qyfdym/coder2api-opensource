package scheduler

// kiro.go 调度器里 Kiro 账号的保号任务（保活 / 余额）。Kiro 没有签到体系。
//
// 调度器不依赖 Kiro 协议包：经 KiroOps 接口调用（main 注入 kiroacct.Service 的适配）。
// 排程与其它平台共用 keepalive_hours / 余额周期；签到时点顺带刷新一次余额。

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// KiroOps Kiro 账号运维（由 kiroacct 适配实现）。
type KiroOps interface {
	// Keepalive 刷新 token 并落盘（API Key 账号是空操作）。sessionDead = 需要重新登录。
	Keepalive(a *auth.Auth) (sessionDead bool, err error)
	// Balance 查询额度（池内整数口径）；resetAt = 额度下次重置时刻（未知为零值）。
	Balance(a *auth.Auth) (remain, total int64, resetAt time.Time, err error)
}

// kiroKeepalive 单号保活（会话死亡连续计数语义与其它平台一致）。
func (s *Scheduler) kiroKeepalive(uid string, a *auth.Auth, disabled bool) {
	if s.cfg.Kiro == nil {
		return
	}
	dead, err := s.cfg.Kiro.Keepalive(a)
	if err != nil {
		log.Printf("kiro keepalive %s: %v", logfmt.Label(uid, a.Nickname), err)
		if dead && !disabled && s.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("kiro keepalive %s: 连续 %d 次登录失效 — 禁用", logfmt.Label(uid, a.Nickname), pool.SessionDeadThreshold())
		}
		return
	}
	s.cfg.Pool.ClearSessionDead(uid)
}

// kiroBalance 单号余额刷新：有额度即解冻；额度用完则硬冷却到下次重置（不再白撞 402）。
func (s *Scheduler) kiroBalance(uid string, a *auth.Auth) {
	if s.cfg.Kiro == nil {
		return
	}
	remain, total, resetAt, err := s.cfg.Kiro.Balance(a)
	if err != nil {
		log.Printf("kiro balance %s: %v", logfmt.Label(uid, a.Nickname), err)
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain, total)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{Remain: remain, Total: total})
	if remain <= 0 && total > 0 {
		d := time.Until(resetAt)
		if resetAt.IsZero() || d < time.Minute {
			d = time.Hour
		}
		s.cfg.Pool.Cooldown(uid, pool.CoolHard, d, "kiro 额度耗尽（等待额度重置）")
	}
}
