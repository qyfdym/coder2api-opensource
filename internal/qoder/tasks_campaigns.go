package qoder

// 官方活动平台：活动列表（只读）与单个活动领取。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：Account._campaigns_get / campaigns / claim_campaign / campaign_label /
// _machine_headers_hint；qoder_tasks.py：campaign_title / campaign_desc / campaign_link /
// _campaign_state_cn / CAMPAIGN_NAME_FALLBACK。
//
// 官方把「每日领取 100 Credits」等限时活动搬到了 campaign 平台。取列表要两层都对：
// ① 桌面端请求头；② 服务端认可的真实机器身份。任一不对都表现为 HTTP 200 + 列表里少活动
// （不报错）——这正是「领不到」最常见的根因，所以列表结果里如实带出身份与机器头状态。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Campaign 活动平台上的一个活动（字段对齐原实现 campaigns() 的 item 与 /tasks 视图需要）。
type Campaign struct {
	ID    string // campaignId
	Key   string // campaignKey
	Title string // 显示名：官方中文标题 > 内置兜底表 > 奖励类型 > key（原实现 campaign_title）
	Label string // 结论文案用名：官方中文标题 > key > id（原实现 campaign_label，签到消息用它）

	TitleZh     string
	TitleEn     string
	Description string // 官方中文说明（placements[].content.zh.description）
	DetailURL   string // 官方详情页（content.zh/en.detailUrl）
	ButtonText  string
	JumpURL     string // 跳转：DetailURL > placement.campaignUrl > 列表 campaignUrl > 官网 /activities

	ActionType        string // CLAIM_BENEFIT / VIEW_DETAILS / ""
	ClaimStatus       string // CLAIMABLE / CLAIMED / NOT_ELIGIBLE / ACHIEVEMENT_NOT_COMPLETED / ...
	UnavailableReason string // REDEMPTION_CODE_OUT_OF_STOCK / ACHIEVEMENT_NOT_COMPLETED / RISK_BLOCKED / ...
	// State 归一分类（与官方前端状态机一致）：claimable / claimed / out_of_stock / inactive /
	// risk_blocked / task_required / ineligible。StateLabel 为对应中文。
	State      string
	StateLabel string

	StartAt int64 // Unix 秒
	EndAt   int64

	BenefitKind   string // CREDITS / REDEMPTION_CODE / REDEMPTION_COUPON / COUPON / ""
	BenefitAmount int64
	RewardCredit  int64  // Credits 类奖励额（非 Credits 类为 0）
	RewardText    string // 非 Credits 类奖励的文字（如「兑换码 ×1」）

	RequiredAchievementKey string // 需先完成的成就（如 sites_first_use）
	AchievementCompleted   bool

	// Current / Target 进度：活动平台不下发数值进度，按原实现任务行口径——
	// 可领 / 已领为 1/1，其余 0/1。
	Current int
	Target  int
}

// CampaignList 活动列表全貌（Campaigns 只返回其中的 Campaigns）。
type CampaignList struct {
	ShowCampaign bool   // 上游 showCampaign：false 通常意味着身份未被认可或账号不在定向内
	Claimable    bool   // 上游 claimable
	CampaignURL  string // 上游 campaignUrl
	Campaigns    []Campaign
	// Available 活动平台在本区域是否存在（查询失败且 HTTP 404/405/410 时为 false）。
	Available bool
	// Identity 本次身份来源（runtime-info / derived）；MachineHeaders 本次是否发了机器头（native / omitted）。
	Identity       string
	MachineHeaders string
	IdentityError  string // 取不到真实身份的原因（Identity=derived 时非空）
	Hint           string // 国际版 + 未发机器头时的已知限制提示（国内版为空）
	Healed         bool   // 本次因 showCampaign=false 触发了「换身份自愈」且重试成功
}

// 活动领取失败码 → 中文（与官方 growth-page / activity-iframe 前端一致）。
var campaignFailureCN = map[string]string{
	"REDEMPTION_CODE_OUT_OF_STOCK": "今日名额已发完（每日 10:00 刷新，次日再来）",
	"ACHIEVEMENT_NOT_COMPLETED":    "需先完成新人任务（成就未完成）",
	"CAMPAIGN_NOT_ACTIVE":          "活动已结束/未开始",
	"RISK_BLOCKED":                 "风控拦截（当前设备/账号不可领取）",
	"RISK_DEPENDENCY_UNAVAILABLE":  "风控服务不可用，稍后重试",
}

// campaignNameFallback 活动中文名兜底（服务端没带 content.zh 时用；先精确再按顺序前缀匹配）。
var campaignNameFallback = []struct{ key, name string }{
	{"act-20260928-620", "新人任务：发布 Qoder 站点领奶茶免单卡"},
	{"act-20260928", "新人任务：发布 Qoder 站点领奶茶免单卡"},
	{"act-20260930", "每天领 100 Credits"},
	{"act-20260923", "每天领 100 Credits"},
	{"act-20260901-922", "限时福利：专业版/高级版首月 Credits 翻倍"},
	{"act-20260901", "全新 Qoder 上线福利"},
}

var campaignStateLabel = map[string]string{
	"claimable": "可领", "claimed": "已领", "out_of_stock": "名额发完", "task_required": "需先完成任务",
	"risk_blocked": "风控拦截", "inactive": "活动已结束", "ineligible": "暂不可领",
}

// intlIdentityHint 国际版 + 未发送机器头时的已知限制提示（原实现 _machine_headers_hint 原文）。
const intlIdentityHint = "国际版服务端要求真实的 UMID 机器身份（由官方客户端组件生成、每 50 分钟刷新），" +
	"本机没有该组件、本次未发送机器头：活动列表可能不可见、领取可能失败。这是已知限制，不等于今天没有活动。"

// Campaigns 活动列表（只读）。完整的列表元信息（showCampaign、身份状态等）见 CampaignsView。
func (c *Client) Campaigns(ctx context.Context, cred Cred) ([]Campaign, *Error) {
	l, err := c.CampaignsView(ctx, cred)
	return l.Campaigns, err
}

// CampaignsView 活动列表与元信息（只读；原实现 Account.campaigns(force=True)）。
//
// 身份自愈：本次带着真实身份却拿到 showCampaign=false，通常意味着服务端已不认这份身份——
// 清掉组件种子强制换一份**新**身份并重试一次（同种子下组件只会返回同一个被拒的身份）。
// 原实现 20 秒列表缓存属于面板层优化，这里不做（调用方需要时自行缓存，领取路径必须绕过缓存）。
func (c *Client) CampaignsView(ctx context.Context, cred Cred) (CampaignList, *Error) {
	realm := NormalizeRealm(cred.Realm)
	q, use, err := c.campaignsGet(ctx, cred)
	healed := false
	if err == nil && !oaTruthy(q["showCampaign"]) && use.source == IdentitySourceNative {
		c.logf("活动列表 showCampaign=false（%s），清种子换新机器身份后重试一次", oaTail(cred.UID, 8))
		_, _ = c.machineIdentity(ctx, true)
		if q2, use2, err2 := c.campaignsGet(ctx, cred); err2 == nil && oaTruthy(q2["showCampaign"]) {
			q, use, healed = q2, use2, true
		}
	}
	list := CampaignList{Identity: use.source, MachineHeaders: use.headers, Available: true, Healed: healed}
	if use.err != nil {
		list.IdentityError = use.err.Error()
	}
	if realm == RealmIntl && use.headers == MachineHeadersOmitted {
		list.Hint = intlIdentityHint
	}
	if err != nil {
		switch err.Status {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone:
			list.Available = false
		}
		if list.Hint != "" {
			e := *err
			e.Msg += "（" + list.Hint + "）"
			err = &e
		}
		return list, err
	}
	list.ShowCampaign = oaTruthy(q["showCampaign"])
	list.Claimable = oaTruthy(q["claimable"])
	list.CampaignURL = oaStr(q["campaignUrl"])
	website := ConfigFor(realm).Website
	for _, raw := range oaList(q["campaigns"]) {
		m := oaMap(raw)
		if m == nil {
			continue
		}
		list.Campaigns = append(list.Campaigns, parseCampaign(m, list.CampaignURL, website))
	}
	return list, nil
}

// campaignsGet 一次活动列表请求：桌面端头；被拒（401/403）回退普通头，至少保留「能不能看到」的信息。
func (c *Client) campaignsGet(ctx context.Context, cred Cred) (map[string]any, identityUse, *Error) {
	u := oaURL(cred.Realm, oaPathCampaigns)
	h, use := c.oaDesktopHeaders(ctx, cred)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: u, header: h, attempts: 1, timeout: 15 * time.Second})
	if err != nil && (err.Status == http.StatusUnauthorized || err.Status == http.StatusForbidden) {
		resp2, err2 := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: u, header: oaPlainHeaders(cred),
			attempts: 1, timeout: 15 * time.Second})
		if err2 == nil {
			resp, err = resp2, nil
		} else {
			out := *err2
			if err2.Kind == KindTransport {
				out.Msg = fmt.Sprintf("HTTP %d (desktop) / %s (plain)", err.Status, err2.Msg)
			} else {
				out.Msg = fmt.Sprintf("HTTP %d (desktop) / HTTP %d (plain)", err.Status, err2.Status)
			}
			if err.Kind == KindSessionDead && out.Kind != KindSessionDead {
				out.Kind, out.Code = KindSessionDead, err.Code
			}
			return nil, use, &out
		}
	}
	if err != nil {
		return nil, use, err
	}
	q, ok := oaObject(resp.body)
	if !ok {
		return nil, use, &Error{Kind: KindServer, Status: resp.status, Msg: fmt.Sprintf("HTTP %d", resp.status),
			Body: truncate(string(resp.body), oaBodyKeep)}
	}
	return q, use, nil
}

// parseCampaign 单个活动的字段映射（纯函数）。
func parseCampaign(m map[string]any, listURL, website string) Campaign {
	benefit := oaMap(m["benefit"])
	placements := oaList(m["placements"])
	var titleZh, titleEn, descZh, detail, button, plURL string
	for i, pl := range placements {
		plm := oaMap(pl)
		if i == 0 {
			plURL = oaStr(plm["campaignUrl"])
		}
		cont := oaMap(plm["content"])
		if cont == nil {
			continue
		}
		zh, en := oaMap(cont["zh"]), oaMap(cont["en"])
		if titleZh == "" {
			titleZh = oaStr(zh["title"])
		}
		if titleEn == "" {
			titleEn = oaStr(en["title"])
		}
		if descZh == "" {
			descZh = oaStr(zh["description"])
		}
		if detail == "" {
			detail = oaFirst(oaStr(zh["detailUrl"]), oaStr(en["detailUrl"]))
		}
		if button == "" {
			button = oaFirst(oaStr(zh["buttonText"]), oaStr(en["buttonText"]))
		}
		if titleZh != "" && descZh != "" && detail != "" {
			break
		}
	}
	cp := Campaign{
		ID:                     oaFirst(oaStr(m["campaignId"]), oaStr(m["campaign_id"])),
		Key:                    oaFirst(oaStr(m["campaignKey"]), oaStr(m["campaign_key"])),
		TitleZh:                titleZh,
		TitleEn:                titleEn,
		Description:            strings.TrimSpace(descZh),
		DetailURL:              strings.TrimSpace(detail),
		ButtonText:             button,
		ActionType:             oaFirst(oaStr(m["actionType"]), oaStr(m["action_type"])),
		ClaimStatus:            oaFirst(oaStr(m["claimStatus"]), oaStr(m["claim_status"])),
		UnavailableReason:      oaStr(m["unavailableReason"]),
		StartAt:                normalizeEpoch(oaFirstTruthy(m["startAt"], m["start_at"])),
		EndAt:                  normalizeEpoch(oaFirstTruthy(m["endAt"], m["end_at"])),
		BenefitKind:            oaStr(benefit["kind"]),
		BenefitAmount:          oaInt(benefit["amount"]),
		RequiredAchievementKey: oaStr(m["requiredAchievementKey"]),
		AchievementCompleted:   oaTruthy(m["achievementCompleted"]),
		Target:                 1,
	}
	cp.Label = campaignLabel(cp)
	cp.Title = campaignTitle(cp)
	cp.JumpURL = oaFirst(cp.DetailURL, plURL, listURL, website+"/activities")
	cp.State = campaignState(cp.ClaimStatus, cp.UnavailableReason, cp.AchievementCompleted)
	cp.StateLabel = campaignStateLabel[cp.State]
	if cp.State == "claimable" || cp.State == "claimed" {
		cp.Current = 1
	}
	kind := strings.ToUpper(cp.BenefitKind)
	if kind == "" || kind == "CREDITS" {
		cp.RewardCredit = cp.BenefitAmount
	} else {
		amt := cp.BenefitAmount
		if amt == 0 {
			amt = 1
		}
		cp.RewardText = fmt.Sprintf("%s ×%d", campaignKindLabel(kind), amt)
	}
	return cp
}

// campaignLabel 原实现 qoder_accounts.campaign_label：官方中文标题优先，其次 key，再次 id。
func campaignLabel(c Campaign) string {
	if t := strings.TrimSpace(c.TitleZh); t != "" {
		return t
	}
	return oaFirst(c.Key, c.ID)
}

// campaignTitle 原实现 qoder_tasks.campaign_title：官方中文名 > 内置兜底 > 奖励类型 > key。
func campaignTitle(c Campaign) string {
	if t := strings.TrimSpace(c.TitleZh); t != "" {
		return t
	}
	key := oaFirst(c.Key, c.ID)
	for _, f := range campaignNameFallback {
		if key == f.key {
			return f.name
		}
	}
	for _, f := range campaignNameFallback {
		if strings.HasPrefix(key, f.key) {
			return f.name
		}
	}
	switch strings.ToUpper(c.BenefitKind) {
	case "REDEMPTION_CODE":
		return "限时活动（兑换码）"
	case "REDEMPTION_COUPON":
		return "限时活动（兑换券）"
	case "COUPON":
		return "限时活动（优惠券）"
	case "CREDITS":
		return "限时活动（Credits）"
	}
	if key != "" {
		return key
	}
	return "限时活动"
}

func campaignKindLabel(kind string) string {
	switch kind {
	case "REDEMPTION_CODE":
		return "兑换码"
	case "REDEMPTION_COUPON":
		return "兑换券"
	case "COUPON":
		return "优惠券"
	case "":
		return "奖励"
	}
	return kind
}

// campaignState 原实现 _campaign_state_cn：把活动状态归一成分类。
func campaignState(status, reason string, achievementOK bool) string {
	st, rs := strings.ToUpper(status), strings.ToUpper(reason)
	switch {
	case st == "CLAIMABLE":
		return "claimable"
	case st == "CLAIMED":
		return "claimed"
	case rs == "REDEMPTION_CODE_OUT_OF_STOCK":
		return "out_of_stock"
	case rs == "CAMPAIGN_NOT_ACTIVE":
		return "inactive"
	case rs == "RISK_BLOCKED":
		return "risk_blocked"
	case rs == "ACHIEVEMENT_NOT_COMPLETED" || !achievementOK:
		return "task_required"
	}
	return "ineligible"
}

// claimOutcome 单个活动领取结论（原实现 claim_campaign 的返回 dict）。
type claimOutcome struct {
	ok, replayed, blocked bool
	status, failureCode   string
	grantID, code         string
	amount                int64
	message               string
	errText               string // 失败原因（日志 / 结论口径）
	err                   *Error // 上游错误（HTTP / 传输）；业务层拒绝时为 nil
}

// claimCampaign POST …/campaigns/{id}/claim（官方幂等语义）。
//
// 服务端按「人」去重（同一机器指纹下的多账号合并为一人）：
//   - 已领取 → status=CLAIMED + replayed=true（不重复发放）；HTTP 409 / ALREADY_CLAIMED 同义；
//   - 同人已领 → status=BLOCKED + failureCode=SAME_PERSON_ALREADY_CLAIMED（同机多号每轮共享一次）；
//   - 兑换码类：CLAIMED 且 redemptionCode 非空才算拿到码；只有 CLAIMED 无码 = 发放确认中。
func (c *Client) claimCampaign(ctx context.Context, cred Cred, id string) claimOutcome {
	h, _ := c.oaDesktopHeaders(ctx, cred)
	u := oaURL(cred.Realm, fmt.Sprintf(oaPathCampaignClaimFmt, url.PathEscape(id)))
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodPost, url: u, body: []byte("{}"), header: h,
		attempts: 1, timeout: 20 * time.Second})
	if err != nil {
		if resp == nil {
			return claimOutcome{errText: err.Msg, err: err}
		}
		ec := ""
		if m, ok := oaObject(resp.body); ok {
			ec = strings.ToUpper(oaStr(m["errorCode"]))
		}
		if resp.status == http.StatusConflict || ec == "ALREADY_CLAIMED" || ec == "REPLAYED" {
			return claimOutcome{ok: true, status: "CLAIMED", replayed: true, message: "今日已领取（上游幂等确认）"}
		}
		return claimOutcome{errText: oaHTTPText(resp.status, resp.body), err: err}
	}
	r, ok := oaObject(resp.body)
	if !ok {
		return claimOutcome{errText: "unexpected claim body: " + oaCut(string(resp.body), 160),
			err: &Error{Kind: KindServer, Status: resp.status, Msg: "unexpected claim body", Body: truncate(string(resp.body), oaBodyKeep)}}
	}
	status := strings.ToUpper(oaStr(r["status"]))
	failure := strings.ToUpper(oaStr(r["failureCode"]))
	benefitAmt := oaInt(oaMap(r["benefit"])["amount"])
	if failure == "SAME_PERSON_ALREADY_CLAIMED" || status == "BLOCKED" {
		return claimOutcome{blocked: true, status: oaFirst(status, "BLOCKED"), failureCode: oaFirst(failure, "BLOCKED"),
			amount: benefitAmt, message: "同人已领取（同一设备/身份下其他账号本轮已领，服务端按人去重）"}
	}
	if succ, isBool := r["success"].(bool); status == "" && isBool && !succ {
		return claimOutcome{errText: oaCut(oaFirst(oaStr(r["error"]), string(resp.body)), 160)}
	}
	code := strings.TrimSpace(oaStr(r["redemptionCode"]))
	if msg, known := campaignFailureCN[failure]; known {
		return claimOutcome{status: oaFirst(status, "NOT_ELIGIBLE"), failureCode: failure, code: code, message: msg}
	}
	out := claimOutcome{
		ok:          status == "CLAIMED" || status == "GRANTED" || status == "SUCCESS",
		status:      status,
		replayed:    oaTruthy(r["replayed"]),
		failureCode: failure,
		grantID:     oaStr(r["grantId"]),
		code:        code,
		amount:      benefitAmt,
	}
	if out.amount == 0 {
		out.amount = oaInt(r["amount"])
	}
	out.message = "领取成功"
	if out.replayed {
		out.message = "已领取"
	}
	if code != "" {
		out.message += "，兑换码：" + code
	} else if status == "CLAIMED" {
		out.message += "，兑换码发放确认中"
	}
	if !out.ok {
		out.errText = "status=" + oaFirst(status, "?")
	}
	return out
}

func oaFirst(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func oaFirstTruthy(vs ...any) any {
	for _, v := range vs {
		if oaTruthy(v) {
			return v
		}
	}
	return nil
}
