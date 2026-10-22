package qoder

// 签到闭环（活动平台 → 旧 sash 接口兜底 → 刷新额度）与 Pro 福利包领取。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_tasks.py：run_checkin / run_pro_claim；qoder_accounts.py：Account.checkin /
// checkin_status / pro_eligibility / pro_claim。
//
// Logs 的每一行与原实现 run_checkin / run_pro_claim 的日志逐字一致（单元测试用原实现输出对照），
// 只有一处有意偏离：领取请求报错时原实现打印「活动平台查询失败：None」（取了不存在的 error 键），
// 这里打印真实的失败原因。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RunCheckin 签到闭环。onlyDaily=true 只领 Credits 类活动（DailyCheckin），false 领全部（ClaimAll）。
func (c *Client) RunCheckin(ctx context.Context, cred Cred, onlyDaily bool, opt CheckinOptions) (CheckinResult, *Error) {
	gap := opt.gap()
	realm := NormalizeRealm(cred.Realm)
	name := credName(cred)
	res := CheckinResult{}
	logf := func(format string, args ...any) { res.Logs = append(res.Logs, fmt.Sprintf(format, args...)) }
	logf("开始为账号 [%s] 执行每日签到...", name)

	camp := c.campaignCheckin(ctx, cred, onlyDaily, gap, opt.BlockedUntil)
	res.Identity, res.MachineHeaders, res.IdentityError = camp.list.Identity, camp.list.MachineHeaders, camp.list.IdentityError
	if camp.list.MachineHeaders == MachineHeadersOmitted {
		logf("⚠ [%s] 未取得官方机器身份（%s），本次未发送机器头", name, camp.list.IdentityError)
	}
	logCodes := func() {
		for _, it := range camp.codes {
			logf("🎟 [%s] %s 兑换码：%s（官方活动页可扫码兑换）", name, it.Campaign, it.Code)
		}
	}
	switch {
	case len(camp.claimed) > 0:
		logf("✓ [%s] 活动领取成功 +%d Credits（%s）", name, camp.earned, joinLabels(camp.claimed, "、"))
		logCodes()
	case len(camp.blocked) > 0:
		fcs := make([]string, 0, len(camp.blocked))
		for _, b := range camp.blocked {
			fcs = append(fcs, oaFirst(b.FailureCode, "?"))
		}
		logf("⚠ [%s] 同人已领取：同一设备/身份下其他账号本轮已领（服务端按人去重，%s），本号本轮不再发放", name, strings.Join(fcs, ", "))
	case len(camp.already) > 0:
		logf("✓ [%s] 今日活动奖励已领取（%s）", name, joinLabels(camp.already, "、"))
		logCodes()
	case camp.ok():
		logf("— [%s] %s", name, camp.message)
		logCodes()
	case camp.queryErr != nil:
		logf("! [%s] 活动平台查询失败：%s", name, camp.queryErr.Msg)
	default:
		logf("! [%s] %s", name, camp.message)
	}

	if len(camp.claimed) > 0 || len(camp.already) > 0 || camp.ok() {
		for _, it := range camp.pending {
			logf("⏳ [%s] %s：今日名额已发完，次日 10:00 后自动重试", name, it.Label)
		}
		for _, it := range camp.locked {
			logf("🔒 [%s] %s：需先在官方桌面端完成新人任务（成就 %s）", name, it.Label, oaFirst(it.RequiredAchievementKey, "?"))
		}
		res.OK = true
		res.Earned = float64(camp.earned)
		res.AlreadyClaimed = len(camp.claimed) == 0 && len(camp.already) > 0
		res.CheckedIn = camp.checkedIn
		res.Message = camp.message + identitySuffix(realm, camp.list, camp.checkedIn)
		res.NextAvailableAt = camp.next
		res.Codes = camp.codes
		res.Blocked = camp.blocked
		for _, cp := range camp.claimed {
			res.Claimed = append(res.Claimed, cp.Label)
		}
		for _, cp := range camp.pending {
			res.Pending = append(res.Pending, cp.Label)
		}
		for _, cp := range camp.locked {
			res.Locked = append(res.Locked, cp.Label)
		}
		c.refreshAfterClaim(ctx, cred, opt, gap, &res, logf, false)
		return res, camp.claimErr
	}

	// --- 活动平台没给出结论：旧 sash 签到接口兜底 ---
	campFail := camp.message
	campErr := camp.claimErr
	if camp.queryErr != nil {
		campFail = "活动平台查询失败：" + camp.queryErr.Msg
		campErr = camp.queryErr
	}
	campFail += identitySuffix(realm, camp.list, false)
	lg := c.legacyCheckin(ctx, cred)
	if !lg.ok {
		logf("! [%s] 签到失败: %s", name, lg.errText)
		res.Message = campFail + "；旧签到接口签到失败：" + lg.errText
		if lg.err != nil {
			return res, lg.err
		}
		return res, campErr
	}
	if lg.unavailable || lg.disabled {
		logf("— [%s] %s，本次跳过", name, lg.msg)
		res.Unavailable = lg.unavailable
		res.Message = campFail + "；" + lg.msg + "，本次跳过"
		// 会话死亡 / 鉴权失败不能被「旧接口不存在」掩盖成成功。
		res.OK = campErr == nil || (campErr.Kind != KindSessionDead && campErr.Kind != KindAuth)
		return res, campErr
	}
	res.OK, res.CheckedIn = true, true
	if lg.already {
		logf("✓ [%s] %s", name, lg.msg)
		res.AlreadyClaimed = true
	} else {
		res.Earned = float64(lg.reward)
		res.Claimed = []string{"每日签到（旧接口）"}
		logf("✓ [%s] 签到成功 +%d 积分（连续 - 天）", name, lg.reward)
	}
	res.Message = lg.msg
	c.refreshAfterClaim(ctx, cred, opt, gap, &res, logf, true)
	return res, nil
}

// refreshAfterClaim 领取后刷新额度快照（发放有秒级延迟，先等 gap；失败不影响签到结论）。
// 活动平台路径打印 remain，旧接口路径打印有效余额（与原实现两条路径的日志口径一致）。
func (c *Client) refreshAfterClaim(ctx context.Context, cred Cred, opt CheckinOptions, gap time.Duration,
	res *CheckinResult, logf func(string, ...any), effective bool) {
	if opt.SkipCreditsRefresh || !oaSleep(ctx, gap) {
		return
	}
	cr, err := c.FetchCredits(ctx, cred)
	if err != nil {
		return
	}
	res.Credits = &cr
	v := cr.Remain
	if effective {
		v = cr.RemainEffective
	}
	logf("  当前额度余额: %s", oaFmtNum(v))
}

// legacyOutcome 旧 sash 每日签到的结论（原实现 Account.checkin 的返回 dict）。
type legacyOutcome struct {
	ok, unavailable, disabled, already bool
	msg, errText                       string
	reward                             int64
	err                                *Error
}

// legacyStatus GET daily-check-in/status 的摘要（原实现 checkin_status）。
type legacyStatus struct {
	ok          bool
	unavailable bool
	http        int
	errText     string
	err         *Error
	status      string
	active      bool
	today       bool
	streak      int64
	reward      int64
}

func (c *Client) legacyCheckinStatus(ctx context.Context, cred Cred) legacyStatus {
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: oaURL(cred.Realm, oaPathCheckinStatus),
		header: oaPlainHeaders(cred), attempts: 2, timeout: 15 * time.Second})
	if err != nil {
		if resp == nil {
			return legacyStatus{errText: err.Msg, err: err}
		}
		switch resp.status {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone:
			return legacyStatus{unavailable: true, http: resp.status, errText: err.Msg}
		case http.StatusForbidden:
			// issue #40：403 多为出口 / WAF 拦截（国际版该接口本应 404）；会话死亡标记则照常上报。
			if err.Kind == KindSessionDead {
				return legacyStatus{errText: err.Msg, err: err}
			}
			return legacyStatus{unavailable: true, http: resp.status, errText: err.Msg}
		}
		return legacyStatus{errText: err.Msg, err: err}
	}
	q, _ := oaObject(resp.body)
	st := legacyStatus{ok: true, status: oaStr(q["status"]), streak: oaInt(q["currentStreakDays"]),
		reward: oaInt(q["rewardCredits"])}
	st.active = st.status == "CLAIMABLE" || st.status == "CLAIMED"
	if last := normalizeEpoch(q["lastClaimedAt"]); last > 0 {
		// 原实现按本机时区比较「今天」（time.localtime），这里保持一致。
		st.today = st.status == "CLAIMED" && time.Unix(last, 0).Format("2006-01-02") == time.Now().Format("2006-01-02")
	}
	return st
}

// legacyCheckin 旧 sash 每日签到：先查状态，未签且活动开放才领取。
// 接口不存在（国际版 404）按「本区域无此接口」给出明确原因，而不是静默什么都不做。
func (c *Client) legacyCheckin(ctx context.Context, cred Cred) legacyOutcome {
	st := c.legacyCheckinStatus(ctx, cred)
	if !st.ok {
		if st.unavailable {
			return legacyOutcome{ok: true, unavailable: true,
				msg: fmt.Sprintf("本区域未开放 /sash/api/v1/me/daily-check-in 接口（HTTP %d）：每日领取活动改由官方客户端承接", st.http)}
		}
		return legacyOutcome{errText: st.errText, err: st.err}
	}
	if st.today {
		return legacyOutcome{ok: true, already: true, msg: "今日已签到"}
	}
	if !st.active {
		return legacyOutcome{ok: true, disabled: true, msg: fmt.Sprintf("官方签到活动未开放 (status=%s)", oaFirst(st.status, "?"))}
	}
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodPost, url: oaURL(cred.Realm, oaPathCheckinClaim),
		body: []byte("{}"), header: oaPlainHeaders(cred), attempts: 1, timeout: 15 * time.Second})
	if err != nil {
		if resp != nil && strings.Contains(string(resp.body), "ALREADY_CLAIMED") {
			return legacyOutcome{ok: true, already: true, msg: "今日已签到"} // 并发 / 重复领取的 409
		}
		return legacyOutcome{errText: err.Msg, err: err}
	}
	r, _ := oaObject(resp.body)
	succ, isBool := r["success"].(bool)
	failed := isBool && !succ
	if oaStr(r["result"]) == "ALREADY_CLAIMED" || (failed && strings.Contains(oaStr(r["error"]), "ALREADY")) {
		return legacyOutcome{ok: true, already: true, msg: "今日已签到"}
	}
	if failed {
		if st2 := c.legacyCheckinStatus(ctx, cred); st2.ok && st2.today {
			return legacyOutcome{ok: true, already: true, msg: "今日已签到（复查确认）"}
		}
		return legacyOutcome{errText: oaCut(oaFirst(oaStr(r["error"]), string(resp.body)), 160)}
	}
	reward := oaInt(r["rewardCredits"])
	return legacyOutcome{ok: true, reward: reward, msg: fmt.Sprintf("签到成功 +%d 积分", reward)}
}

// claimPro Pro 升级包：eligibility → claim。
//
// 幂等语义：上游把「已领取」归一成成功（409 / ALREADY），必须区分「本轮真实新增」与「本就已领」，
// 否则批量汇总的新增积分每次都会虚增 +1800（原实现修过的历史缺陷）。
func (c *Client) claimPro(ctx context.Context, cred Cred, opt CheckinOptions) (CheckinResult, *Error) {
	name := credName(cred)
	res := CheckinResult{}
	logf := func(format string, args ...any) { res.Logs = append(res.Logs, fmt.Sprintf(format, args...)) }

	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: oaURL(cred.Realm, oaPathProEligibility),
		header: oaPlainHeaders(cred), attempts: 1, timeout: 15 * time.Second})
	eligible := false
	if err != nil {
		if resp != nil && (resp.status == http.StatusNotFound || resp.status == http.StatusForbidden || resp.status == http.StatusGone) &&
			err.Kind != KindSessionDead {
			eligible = false // 端点不存在 / 活动已下线：查询成功，只是不可领取
		} else {
			why := err.Msg
			if resp != nil {
				why = fmt.Sprintf("HTTP %d", resp.status)
			}
			logf("! [%s] Pro 升级包资格查询失败: %s", name, why)
			res.State, res.Message = "error", "资格查询失败: "+why
			return res, err
		}
	} else {
		m, _ := oaObject(resp.body)
		eligible = oaTruthy(m["eligible"])
	}
	if !eligible {
		logf("— [%s] Pro 升级包不可领取（已领或活动未开放）", name)
		res.OK, res.State, res.Message = true, "not_available", "不可领取（已领或活动未开放）"
		return res, nil
	}

	resp, err = c.oaDo(ctx, oaRequest{method: http.MethodPost, url: oaURL(cred.Realm, oaPathProClaim),
		body: []byte("{}"), header: oaPlainHeaders(cred), attempts: 1, timeout: 15 * time.Second})
	if err != nil {
		if resp != nil && (resp.status == http.StatusConflict || strings.Contains(string(resp.body), "ALREADY")) {
			logf("✓ [%s] Pro 升级包已领取过（不重复发放，本次不计新增）", name)
			res.OK, res.AlreadyClaimed, res.State = true, true, "already_claimed"
			res.Message = "已领取（不重复发放，本次不计新增）"
			return res, nil
		}
		logf("! [%s] Pro 升级包领取失败: %s", name, err.Msg)
		res.State, res.Message = "error", "领取失败: "+err.Msg
		return res, err
	}
	m, _ := oaObject(resp.body)
	if succ, isBool := m["success"].(bool); isBool && !succ {
		why := oaCut(oaFirst(oaStr(m["message"]), string(resp.body)), 160)
		logf("! [%s] Pro 升级包领取失败: %s", name, why)
		res.State, res.Message = "error", "领取失败: "+why
		return res, nil
	}
	logf("✓ [%s] Pro 升级包领取成功", name)
	earned := oaInt(m["amount"])
	if earned <= 0 {
		earned = proRewardCredit
	}
	res.OK, res.State, res.Earned = true, "claimed_now", float64(earned)
	res.Claimed = []string{"Pro 升级包"}
	res.Message = fmt.Sprintf("领取成功 +%d 积分", earned)
	c.refreshAfterClaim(ctx, cred, opt, opt.gap(), &res, logf, false)
	return res, nil
}
