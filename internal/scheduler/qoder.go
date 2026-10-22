package scheduler

// qoder.go 调度器里 Qoder 账号的三类保号任务（签到 / 保活 / 余额）。
//
// 调度器不依赖 Qoder 协议包：经 QoderOps 接口调用（main 注入 qoderacct.Service 的适配）。
// 排程与 WorkBuddy 共用同一套小时表（checkin_hours / keepalive_hours / 余额周期），
// 这样两个平台的账号在同一时刻一起签到、一起保活，面板与日志里看到的是同一轮。

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// QoderOps Qoder 账号运维（由 qoderacct 适配实现）。
type QoderOps interface {
	// Checkin 签到 + 领取已达标的活动奖励（一键签到口径）。
	// done = 今日签到已完成；sessionDead = 凭证已失效需重新登录。
	Checkin(a *auth.Auth) (done bool, msg string, sessionDead bool, err error)
	// Keepalive 刷新 token 并落盘。
	Keepalive(a *auth.Auth) (sessionDead bool, err error)
	// Balance 查询额度（池内整数口径）。
	Balance(a *auth.Auth) (remain, total int64, err error)
}

// qoderCheckin 单号签到 + 刷新余额（RunCheckinNow 的 Qoder 分支）。
func (s *Scheduler) qoderCheckin(uid string, a *auth.Auth) {
	if s.cfg.Qoder == nil {
		return
	}
	done, msg, dead, err := s.cfg.Qoder.Checkin(a)
	switch {
	case dead:
		s.cfg.Pool.Disable(uid, "qoder session expired (checkin): 需要重新登录")
		log.Printf("qoder checkin %s: 会话失效 — 禁用：%v", logfmt.Label(uid, a.Nickname), err)
		return
	case err != nil:
		log.Printf("qoder checkin %s: %v（%s）", logfmt.Label(uid, a.Nickname), err, msg)
	default:
		log.Printf("qoder checkin %s: %s", logfmt.Label(uid, a.Nickname), msg)
	}
	if done {
		s.cfg.Pool.NoteCheckinDone(uid)
	}
	s.qoderBalance(uid, a)
}

// qoderKeepalive 单号保活（RunKeepaliveNow 的 Qoder 分支；会话死亡连续计数语义与 WorkBuddy 一致）。
func (s *Scheduler) qoderKeepalive(uid string, a *auth.Auth, disabled bool) {
	if s.cfg.Qoder == nil {
		return
	}
	dead, err := s.cfg.Qoder.Keepalive(a)
	if err != nil {
		log.Printf("qoder keepalive %s: %v", logfmt.Label(uid, a.Nickname), err)
		if dead && !disabled && s.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("qoder keepalive %s: 连续 %d 次会话失效 — 禁用", logfmt.Label(uid, a.Nickname), pool.SessionDeadThreshold())
		}
		return
	}
	s.cfg.Pool.ClearSessionDead(uid)
}

// qoderBalance 单号余额刷新（含「有额度即解冻」）。
func (s *Scheduler) qoderBalance(uid string, a *auth.Auth) {
	if s.cfg.Qoder == nil {
		return
	}
	remain, total, err := s.cfg.Qoder.Balance(a)
	if err != nil {
		log.Printf("qoder balance %s: %v", logfmt.Label(uid, a.Nickname), err)
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain, total)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{Remain: remain, Total: total})
}
