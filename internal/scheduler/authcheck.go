package scheduler

// authcheck.go 后台任务（签到 / 余额）遇到 401 时的登录状态验证。
//
// 背景：WorkBuddy 的 access token 有效期 60 天，聊天和后台任务平时都不会主动刷新它。上游把
// 登录会话作废后（实例：refresh 返回 12153「invalid_grant: Session doesn't have required
// client」），access token 立即被网关层拒绝，返回的是 openresty / APISIX 的 HTML 401 页，没有
// 业务信封，Classify 只能归成 ErrClient。于是签到、余额每 5 分钟各报一次 401，账号却一直显示
// 正常：保活每天只跑一次，12153 要连续 3 次才禁用，至少拖三天。
//
// 这里在后台任务拿到 401 时立刻刷新一次 token 验证（每个账号限频）：
//   - 刷新成功：只是 access token 被单独作废，换到新 token 后任务重试一次，账号照常可用；
//   - 刷新返回 12153：access token 与 refresh token 都失效，登录确实已经死了，立即禁用并写明
//     「需要重新登录」，后台任务随之不再打这个号；
//   - 其它失败（网络等）：不处置，下个间隔再验证。
//
// 立即禁用而不走「连续 3 次 12153」：那条规则防的是 access token 仍然可用、只有刷新偶发报
// 12153 的误判；这里 access token 已经被拒，账号此刻无论如何都服务不了，留在池里只会被选中后失败。

import (
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// authRecheckInterval 同一账号两次刷新验证之间的最短间隔（余额刷新每 5 分钟一轮，不能每轮都去刷）。
const authRecheckInterval = 30 * time.Minute

// loginDeadReason 登录失效的禁用原因（面板账号行与 /status 的 disabled_reason）。
const loginDeadReason = "登录已失效：access token 被拒（401），refresh token 也已失效（12153）。需要在面板重新登录这个账号"

// recoverAuth err 是「access token 被拒」时刷新 token 验证登录状态。返回 true = 已换到新 token，
// 调用方重试一次；其余情况返回 false（禁用与否看 loginDeadNow）。
func (s *Scheduler) recoverAuth(task, uid string, a *auth.Auth, err error) bool {
	if a == nil || !a.IsWorkBuddy() || s.cfg.Upstream == nil || !upstream.IsAuthRejected(err) {
		return false
	}
	if a.RefreshTokenValue() == "" || !s.authCheckDue(uid) {
		return false
	}
	label := logfmt.Label(uid, a.Nickname)
	rerr := s.cfg.Upstream.RefreshToken(a)
	switch {
	case rerr == nil:
		s.cfg.Pool.ClearSessionDead(uid)
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("%s %s save: %v", task, label, serr)
		}
		log.Printf("%s %s: access token 被拒（401），已刷新 token，重试一次", task, label)
		return true
	case upstream.IsSessionDead(rerr):
		s.cfg.Pool.Disable(uid, loginDeadReason)
		log.Printf("%s %s: %s（刷新返回：%v）", task, label, loginDeadReason, rerr)
	default:
		log.Printf("%s %s: access token 被拒（401），刷新验证失败：%v（%s 后再验证）", task, label, rerr, authRecheckInterval)
	}
	return false
}

// authCheckDue 该账号是否到了可以再验证的时间；是则记下本次时刻。
func (s *Scheduler) authCheckDue(uid string) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := time.Now()
	if last, ok := s.authChecks[uid]; ok && now.Sub(last) < authRecheckInterval {
		return false
	}
	if s.authChecks == nil {
		s.authChecks = map[string]time.Time{}
	}
	s.authChecks[uid] = now
	return true
}

// loginDeadNow 账号是否已因登录失效被禁用（任务据此跳过同一轮里剩下的调用）。
func (s *Scheduler) loginDeadNow(uid string) bool {
	st, ok := s.cfg.Pool.Status(uid)
	return ok && st.Disabled && loginDead(st.DisabledReason)
}

// loginDead 禁用原因是不是「登录失效、要重新登录」（本文件、保活 12153、聊天路径刷新失败三处的写法）。
func loginDead(reason string) bool {
	return strings.Contains(reason, "登录已失效") || strings.Contains(reason, "12153") ||
		strings.Contains(reason, "session dead")
}
