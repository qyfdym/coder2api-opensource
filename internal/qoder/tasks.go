package qoder

// 每日签到 / 一键领取 / Pro 福利包。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_tasks.py：run_checkin / run_pro_claim；qoder_accounts.py：Account.campaign_checkin /
// checkin / checkin_status / pro_eligibility / pro_claim。
//
// 三个入口与原实现面板的三个按钮一一对应：
//
//	DailyCheckin = 账号页「每日签到」：只领 Credits 类活动（run_checkin only_daily=True）；
//	ClaimAll     = 「一键签到 / 领取全部福利」：Credits + 兑换码 / 券类 + 已达标任务奖励
//	               （run_checkin only_daily=False，即原实现 /tasks/run）；
//	ClaimPro     = 「仅领 Pro 福利包」：一次性 +1800（原实现 /tasks/travel → run_pro_claim）。
//
// 顺序（与官方现状一致）：先走活动平台（当前每日 100 Credits 的真实入口）；活动平台没给出结论
// （列表查询失败、或领取全部报错）时，再兜底旧的 sash 每日签到接口。
//
// 本包不持久化任何账号状态：原实现落盘在账号文件里的 lastCheckin / campaignCodes /
// campaignBlockedUntil 都以返回值交还（CheckedIn / Codes / Blocked），由调用方写回；
// 「同人已领」冷却表通过 CheckinOptions.BlockedUntil 传入。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CampaignCode 本次领取到的兑换码（券类活动）。原实现按账号落盘在 campaignCodes[campaignId]，
// 活动只发一次，调用方必须持久化，否则重启后用户就找不回兑换码了。
type CampaignCode struct {
	CampaignID string
	Campaign   string // 活动显示名
	Code       string
}

// CampaignBlock 被服务端「同人已领」挡下的活动（SAME_PERSON_ALREADY_CLAIMED）。
// 调用方应持久化 CampaignID → Until（原实现 campaignBlockedUntil），下次通过
// CheckinOptions.BlockedUntil 传回，冷却期内不再重复 POST。
type CampaignBlock struct {
	CampaignID  string
	Campaign    string
	FailureCode string
	Until       time.Time // 冷却截止（新挡下为 now+6h；命中冷却时为传入的原截止）
	Cooldown    bool      // true = 本次因冷却未发起领取
}

// CheckinResult 签到 / 领取结论。
//
// OK 与原实现口径一致（流程给出了结论，不代表一定领到）；签到相关的「今日是否已完成」看
// CheckedIn。第二个返回值 *Error 非 nil 表示过程中遇到了需要调用方记账的上游错误（会话死亡 /
// 鉴权 / 限流 / 传输 / 服务端）——此时 OK 仍按原实现给出（例如活动平台 500、旧接口回「本区域
// 未开放」时 OK=true），但遇到会话死亡 / 鉴权失败一律 OK=false。
type CheckinResult struct {
	OK              bool
	Earned          float64 // 本次新增 Credits（已领 / 幂等重放不计）
	AlreadyClaimed  bool    // 今日已领（本次没有新增、但确认此前已领）
	Message         string  // 给用户看的结论（已到账 / 今天已领过 / 暂无可领 / 名额已发完 / 失败原因）
	Claimed         []string
	Codes           []CampaignCode
	NextAvailableAt time.Time // 下次可领（每天 10:00 UTC+8）；未知为零值
	Logs            []string

	// CheckedIn 今日签到已完成（本次领到、幂等重放或此前已领）：调用方据此记 lastCheckin。
	CheckedIn bool
	// Blocked 同人已领（含冷却命中）；Pending 名额已发完（次日 10:00 后重试）；
	// Locked 需先在官方桌面端完成任务。三者都是活动显示名。
	Blocked []CampaignBlock
	Pending []string
	Locked  []string
	// Unavailable 旧 sash 签到接口在本区域不存在（404/405/410/403）。
	Unavailable bool
	// Identity / MachineHeaders / IdentityError 本次活动平台请求的机器身份情况（见 CampaignList）。
	Identity       string
	MachineHeaders string
	IdentityError  string
	// Credits 领取后刷新的额度快照（失败或未刷新为 nil）；调用方可直接写回账本。
	Credits *Credits
	// State 仅 ClaimPro：claimed_now / already_claimed / not_available / error（原实现 run_pro_claim）。
	State string
}

// CheckinOptions 签到可选参数（零值即原实现缺省行为）。
type CheckinOptions struct {
	// Gap 相邻两次 claim 之间、以及领取后刷新额度前的等待（原实现 >= 1.0s 防风控）。
	// 0 = 缺省 1s；< 0 = 不等待（仅测试 / 调用方自行控速时使用）。
	Gap time.Duration
	// BlockedUntil campaignId → 冷却截止（调用方持久化的 campaignBlockedUntil）。
	BlockedUntil map[string]time.Time
	// SkipCreditsRefresh 领取后不刷新额度（缺省刷新，与原实现一致）。
	SkipCreditsRefresh bool
}

func (o CheckinOptions) gap() time.Duration {
	switch {
	case o.Gap < 0:
		return 0
	case o.Gap == 0:
		return time.Second
	default:
		return o.Gap
	}
}

// proRewardCredit Pro 升级包面额兜底（上游 claim 目前不回传金额）。
const proRewardCredit = 1800

// samePersonCooldown 「同人已领」后的冷却（原实现 6 小时）。
const samePersonCooldown = 6 * time.Hour

// DailyCheckin 账号页「每日签到」：只领每日 Credits。
func (c *Client) DailyCheckin(ctx context.Context, cred Cred) (CheckinResult, *Error) {
	return c.RunCheckin(ctx, cred, true, CheckinOptions{})
}

// ClaimAll 「一键签到 / 领取全部福利」：每日 Credits + 兑换码类活动 + 已达标任务奖励。
// 与原实现 /tasks/run 一致，不含 Pro 福利包（需要时另调 ClaimPro）。
func (c *Client) ClaimAll(ctx context.Context, cred Cred) (CheckinResult, *Error) {
	return c.RunCheckin(ctx, cred, false, CheckinOptions{})
}

// ClaimPro 「仅领 Pro 福利包」（一次性 +1800）。
func (c *Client) ClaimPro(ctx context.Context, cred Cred) (CheckinResult, *Error) {
	return c.claimPro(ctx, cred, CheckinOptions{})
}

// credName 日志里的账号名（原实现 nickname or uid[:8]）。
func credName(cred Cred) string {
	if cred.Nickname != "" {
		return cred.Nickname
	}
	r := []rune(cred.UID)
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// campaignRun 活动平台签到的中间结论（原实现 campaign_checkin 的返回 dict）。
type campaignRun struct {
	list                                     CampaignList
	queryErr                                 *Error
	claimed, already, pending, locked, views []Campaign
	blocked                                  []CampaignBlock
	codes                                    []CampaignCode
	errors                                   []string
	claimErr                                 *Error
	earned                                   int64
	checkedIn                                bool
	message                                  string
	next                                     time.Time
}

func (r *campaignRun) ok() bool { return r.queryErr == nil && len(r.errors) == 0 }

// campaignCheckin 活动平台签到：列出活动（绕过任何缓存）→ 对 CLAIMABLE 的逐个领取。
//
// 归类先于 action_type 过滤（#25 遗留）：官方在领取后会把 actionType 降级成 VIEW_DETAILS，
// 先按 action_type 过滤会丢掉「已领」事实。已领判据：有奖励（amount>0）或 action_type 仍是可领类；
// 真·详情类（无奖励）才算 views。
func (c *Client) campaignCheckin(ctx context.Context, cred Cred, onlyDaily bool, gap time.Duration, blockedUntil map[string]time.Time) *campaignRun {
	run := &campaignRun{}
	list, err := c.CampaignsView(ctx, cred)
	run.list = list
	run.next = NextCheckinWindow(time.Now())
	if err != nil {
		run.queryErr = err
		return run
	}
	for _, cp := range list.Campaigns {
		if onlyDaily {
			if k := strings.ToUpper(cp.BenefitKind); k != "" && k != "CREDITS" {
				continue // 只要「每日签到领积分」时跳过券类 / 其它奖励
			}
		}
		cid, cname := cp.ID, oaFirst(cp.Key, cp.ID)
		reason := strings.ToUpper(cp.UnavailableReason)
		claimable := cp.ActionType == "" || cp.ActionType == "CLAIM_BENEFIT"
		if cp.ClaimStatus == "CLAIMED" {
			if cp.BenefitAmount > 0 || claimable {
				run.already = append(run.already, cp)
			} else {
				run.views = append(run.views, cp)
			}
			continue
		}
		if !claimable {
			continue // VIEW_DETAILS 类活动无需（也不能）领取
		}
		if cp.ClaimStatus != "CLAIMABLE" {
			// 不可领但并非与我们无关：名额发完 / 成就未完成要如实报出来。
			// 注意原实现的判据：achievementCompleted 缺键即视为 false → 归入 locked。
			if reason == "REDEMPTION_CODE_OUT_OF_STOCK" {
				run.pending = append(run.pending, cp)
			} else if reason == "ACHIEVEMENT_NOT_COMPLETED" || !cp.AchievementCompleted {
				run.locked = append(run.locked, cp)
			}
			continue
		}
		if until, hit := blockedUntil[cid]; hit && until.After(time.Now()) {
			run.blocked = append(run.blocked, CampaignBlock{CampaignID: cid, Campaign: cname,
				FailureCode: "SAME_PERSON_ALREADY_CLAIMED", Until: until, Cooldown: true})
			continue // 同人已领（冷却中）：不再重复 POST
		}
		res := c.claimCampaign(ctx, cred, cid)
		switch {
		case res.ok:
			amount := res.amount
			if amount == 0 {
				amount = cp.BenefitAmount
			}
			run.checkedIn = true
			if res.replayed {
				run.already = append(run.already, cp)
			} else {
				run.claimed = append(run.claimed, cp)
				run.earned += amount
			}
			if res.code != "" {
				run.codes = append(run.codes, CampaignCode{CampaignID: cid, Campaign: cp.Label, Code: res.code})
			}
		case res.blocked:
			run.blocked = append(run.blocked, CampaignBlock{CampaignID: cid, Campaign: cp.Label,
				FailureCode: res.failureCode, Until: time.Now().Add(samePersonCooldown)})
		case campaignFailureCN[res.failureCode] != "":
			if res.failureCode == "REDEMPTION_CODE_OUT_OF_STOCK" {
				run.pending = append(run.pending, cp)
			} else {
				run.locked = append(run.locked, cp)
			}
		default:
			run.errors = append(run.errors, cname+": "+res.errText)
			if run.claimErr == nil && res.err != nil {
				run.claimErr = res.err
			}
		}
		// 每个 claim 之后统一等一次：成功 / 被挡 / 失败都刚打过上游（防风控）。
		if !oaSleep(ctx, gap) {
			break
		}
	}
	if len(run.already) > 0 {
		run.checkedIn = true
	}
	run.message = campaignMessage(run)
	return run
}

// campaignMessage 结论文案（与原实现逐字一致，单元测试用原实现输出对照）。
func campaignMessage(r *campaignRun) string {
	var msg string
	switch {
	case len(r.claimed) > 0:
		msg = fmt.Sprintf("活动领取成功 +%d Credits（%s）", r.earned, joinLabels(r.claimed, "、"))
	case len(r.blocked) > 0:
		msg = "同人已领取：同一设备/身份下的其他账号本轮已领过（服务端按人去重，failureCode=" + r.blocked[0].FailureCode + "）"
	case len(r.already) > 0:
		msg = "今日活动奖励已领取（" + joinLabels(r.already, "、") + "）"
	case len(r.pending) > 0:
		reqs, keys := make([]string, 0, len(r.pending)), make([]string, 0, len(r.pending))
		for _, c := range r.pending {
			reqs = append(reqs, oaFirst(c.RequiredAchievementKey, "无门槛任务"))
			keys = append(keys, oaFirst(c.Key, c.ID))
		}
		msg = "名额已发完，次日 10:00 后可再领：" + strings.Join(reqs, "、") + "（活动 " + strings.Join(keys, ", ") + "）"
	case len(r.locked) > 0:
		msg = "需先在官方桌面端完成新人任务后可领：" + joinReqs(r.locked) + "（活动 " + joinKeys(r.locked) + "）"
	case len(r.errors) > 0:
		msg = "活动领取失败：" + oaCut(strings.Join(r.errors, "; "), 200)
	default:
		msg = "当前账号暂无可领取的官方活动"
	}
	extra := ""
	if len(r.pending) > 0 {
		extra = "；另有活动今日名额已发完、次日 10:00 后重试：" + joinLabels(r.pending, "、")
	} else if len(r.locked) > 0 {
		extra = "；另有活动需先完成新人任务（" + joinReqs(r.locked) + "）：" + joinLabels(r.locked, "、")
	}
	if extra != "" && !strings.Contains(msg, "名额已发完") && !strings.Contains(msg, "新人任务") {
		msg += extra
	}
	return msg
}

func joinLabels(cs []Campaign, sep string) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Label)
	}
	return strings.Join(out, sep)
}

func joinReqs(cs []Campaign) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, oaFirst(c.RequiredAchievementKey, "?"))
	}
	return strings.Join(out, ", ")
}

func joinKeys(cs []Campaign) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, oaFirst(c.Key, c.ID))
	}
	return strings.Join(out, ", ")
}

// identitySuffix 没取到真实机器身份时追加到结论里的原因说明（不静默降级）。
// 国际版一律提示（服务端强校验机器身份）；国内版只在「什么都没领到」时提示（领到了说明不影响）。
func identitySuffix(realm string, list CampaignList, gotSomething bool) string {
	if list.MachineHeaders != MachineHeadersOmitted {
		return ""
	}
	if realm == RealmIntl {
		return "（" + intlIdentityHint + "原因：" + list.IdentityError + "）"
	}
	if gotSomething {
		return ""
	}
	return "（本机未取得官方 UMID 机器身份：" + list.IdentityError + "；本次未发送机器头，服务端可能过滤部分活动）"
}
