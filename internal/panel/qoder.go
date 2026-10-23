// qoder.go 面板里 Qoder 平台的入口：添加账号（OAuth 设备授权 / PAT）、签到与福利、
// 余额与积分构成。
//
// Qoder 账号与 WorkBuddy 账号在同一个账号池、同一张账号表里：解冻 / 禁用 / 暂停 / 移除走
// 通用接口（按 uid），只有「签到 / 余额」这类上游调用按平台分派到这里。协议实现在
// internal/qoder，凭证落盘与签到留痕在 internal/qoderacct。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// qoderEnabled 未启用 Qoder 时统一回 501。
func (p *Panel) qoderEnabled(w http.ResponseWriter) bool {
	if p.cfg.Qoder == nil || p.cfg.Qoder.Client == nil {
		writeErr(w, http.StatusNotImplemented, "qoder 未启用（config.qoder.enabled）")
		return false
	}
	return true
}

func readJSONBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// ---------------------------------------------------------------------------
// 添加账号：OAuth 设备授权
// ---------------------------------------------------------------------------

// qoderLoginStart 生成一次设备授权会话，返回让用户在浏览器打开的授权链接。
// 会话（含一次性的 PKCE verifier）只保存在服务端内存，浏览器只拿到 state 与链接。
func (p *Panel) qoderLoginStart(w http.ResponseWriter, r *http.Request) {
	if !p.qoderEnabled(w) {
		return
	}
	var req struct {
		Realm string `json:"realm"`
	}
	_ = readJSONBody(r, &req)
	s, err := p.cfg.Qoder.Client.StartLogin(qoder.NormalizeRealm(req.Realm))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.qdLoginMu.Lock()
	now := time.Now()
	for k, v := range p.qdLogins {
		if now.After(v.ExpiresAt) {
			delete(p.qdLogins, k)
		}
	}
	p.qdLogins[s.State] = s
	p.qdLoginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "state": s.State, "url": s.AuthURL, "realm": s.Realm,
		"expires_in": int(time.Until(s.ExpiresAt).Seconds()),
	})
}

// qoderLoginPoll 轮询授权结果；拿到 token 即建档、入池、刷新一次余额。
func (p *Panel) qoderLoginPoll(w http.ResponseWriter, r *http.Request) {
	if !p.qoderEnabled(w) {
		return
	}
	state := r.URL.Query().Get("state")
	p.qdLoginMu.Lock()
	s := p.qdLogins[state]
	p.qdLoginMu.Unlock()
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "授权会话不存在或已过期，请重新获取链接"})
		return
	}
	res, err := p.cfg.Qoder.Client.PollLogin(r.Context(), s)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	switch res.Status {
	case "ok":
		p.qdLoginMu.Lock()
		delete(p.qdLogins, state)
		p.qdLoginMu.Unlock()
		a, aerr := p.cfg.Qoder.NewAccount(s.Realm, res.Tokens, res.User, "", "oauth")
		if aerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": "保存账号失败：" + aerr.Error()})
			return
		}
		p.qoderAddToPool(r.Context(), a)
		log.Printf("panel: qoder login ok acct=%s realm=%s", logfmt.Label(a.UID, a.Nickname), s.Realm)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": res.Message, "account": qoderAccountBrief(a)})
	case "expired", "error":
		p.qdLoginMu.Lock()
		delete(p.qdLogins, state)
		p.qdLoginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "message": res.Message})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": res.Message})
	}
}

// qoderLoginCancel 关闭弹窗时丢弃会话（幂等）。
func (p *Panel) qoderLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	_ = readJSONBody(r, &req)
	p.qdLoginMu.Lock()
	_, existed := p.qdLogins[req.State]
	delete(p.qdLogins, req.State)
	p.qdLoginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": existed})
}

// qoderImportPAT 用 pt- 个人访问令牌导入账号（PAT 同时保存为刷新失败时的兜底）。
func (p *Panel) qoderImportPAT(w http.ResponseWriter, r *http.Request) {
	if !p.qoderEnabled(w) {
		return
	}
	var req struct {
		PAT   string `json:"pat"`
		Realm string `json:"realm"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	region := qoder.NormalizeRealm(req.Realm)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ts, qerr := p.cfg.Qoder.Client.ExchangePAT(ctx, region, req.PAT)
	if qerr != nil {
		writeErr(w, http.StatusBadRequest, "PAT 交换失败："+qerr.Error())
		return
	}
	u, uerr := p.cfg.Qoder.Client.UserInfo(ctx, qoder.Cred{Realm: region, AccessToken: ts.AccessToken})
	if uerr != nil {
		writeErr(w, http.StatusBadGateway, "拉取账号信息失败："+uerr.Error())
		return
	}
	if u.Nickname == "" && len(u.UID) >= 8 {
		u.Nickname = "u" + u.UID[len(u.UID)-8:]
	}
	a, err := p.cfg.Qoder.NewAccount(region, ts, u, req.PAT, "pat")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存账号失败："+err.Error())
		return
	}
	p.qoderAddToPool(ctx, a)
	log.Printf("panel: qoder PAT import ok acct=%s realm=%s", logfmt.Label(a.UID, a.Nickname), region)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": qoderAccountBrief(a)})
}

// qoderAddToPool 新账号入池（同 uid 已存在时替换凭证、保留池状态）并刷新一次余额。
func (p *Panel) qoderAddToPool(ctx context.Context, a *auth.Auth) {
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	if _, _, err := p.qoderRefreshCredits(ctx, a); err != nil {
		log.Printf("panel: qoder 新账号余额查询失败 acct=%s: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

func qoderAccountBrief(a *auth.Auth) map[string]any {
	return map[string]any{"uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(), "platform": a.Platform()}
}

// ---------------------------------------------------------------------------
// 余额与积分构成
// ---------------------------------------------------------------------------

// qoderRefreshCredits 查询额度并写回账号池（含「有额度即解冻」语义）。
func (p *Panel) qoderRefreshCredits(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	if p.cfg.Qoder == nil {
		return 0, 0, errQoderDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remain, total, _, qerr := p.cfg.Qoder.Balance(ctx, a)
	if qerr != nil {
		return 0, 0, qerr
	}
	p.cfg.Pool.ReenableIfCredits(a.UID, remain, total)
	p.cfg.Pool.SetCreditsSnapshot(a.UID, pool.CreditsSnapshot{Remain: remain, Total: total})
	return remain, total, nil
}

type qoderDisabledErr struct{}

func (qoderDisabledErr) Error() string { return "qoder 未启用" }

var errQoderDisabled error = qoderDisabledErr{}

// qoderPackages Qoder 额度明细映射成「积分构成」视图的包结构。
func (p *Panel) qoderPackages(ctx context.Context, a *auth.Auth) ([]upstream.CreditPackage, int64, int64, error) {
	if p.cfg.Qoder == nil {
		return nil, 0, 0, errQoderDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remain, total, cr, qerr := p.cfg.Qoder.Balance(ctx, a)
	if qerr != nil {
		return nil, 0, 0, qerr
	}
	out := make([]upstream.CreditPackage, 0, len(cr.Packages))
	for _, pk := range cr.Packages {
		cp := upstream.CreditPackage{
			Name:   pk.Name,
			Remain: int64(math.Floor(pk.Remain)),
			Used:   int64(math.Floor(pk.Used)),
			Size:   int64(math.Floor(pk.Size)),
		}
		if pk.ExpiresAt > 0 && pk.ExpiresAt < 253402214400 {
			cp.ExpiresAt = pk.ExpiresAt * 1000
			cp.EndTime = time.Unix(pk.ExpiresAt, 0).Format(time.RFC3339)
		}
		out = append(out, cp)
	}
	return out, remain, total, nil
}

// ---------------------------------------------------------------------------
// 签到与福利（任务中心的 Qoder tab）
// ---------------------------------------------------------------------------

func parseCheckinMode(s string) qoderacct.CheckinMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "daily":
		return qoderacct.CheckinDaily
	case "pro":
		return qoderacct.CheckinPro
	default:
		return qoderacct.CheckinAll
	}
}

// qoderCheckinView 签到结论的面板形态。
func qoderCheckinView(a *auth.Auth, res qoder.CheckinResult, qerr *qoder.Error) map[string]any {
	out := map[string]any{
		"uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(),
		"ok": res.OK && qerr == nil, "checked_in": res.CheckedIn, "earned": res.Earned,
		"already": res.AlreadyClaimed, "message": res.Message, "claimed": res.Claimed,
		"logs": res.Logs, "identity": res.Identity, "identity_error": res.IdentityError,
	}
	if res.State != "" {
		out["state"] = res.State
	}
	if !res.NextAvailableAt.IsZero() {
		out["next_available_at"] = res.NextAvailableAt.Unix()
		out["next_available_note"] = qoder.CheckinWindowNote(res.NextAvailableAt)
	}
	if len(res.Codes) > 0 {
		codes := make([]map[string]string, 0, len(res.Codes))
		for _, c := range res.Codes {
			codes = append(codes, map[string]string{"campaign": c.Campaign, "code": c.Code})
		}
		out["codes"] = codes
	}
	if qerr != nil {
		out["error"] = qerr.Error()
	}
	return out
}

// qoderCheckinOne 单号签到 / 领取，并顺带刷新余额（与 WorkBuddy 单号签到同语义）。
func (p *Panel) qoderCheckinOne(w http.ResponseWriter, r *http.Request, a *auth.Auth, mode qoderacct.CheckinMode) {
	if !p.qoderEnabled(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, qerr := p.cfg.Qoder.Checkin(ctx, a, mode)
	p.qoderAfterCheckin(a, res, qerr)
	view := qoderCheckinView(a, res, qerr)
	if remain, total, err := p.qoderRefreshCredits(ctx, a); err == nil {
		view["credits"], view["credits_total"] = remain, total
	} else {
		view["balance_error"] = err.Error()
	}
	view["checkin_done"] = res.CheckedIn
	view["checkin_message"] = res.Message
	writeJSON(w, http.StatusOK, view)
}

// qoderAfterCheckin 签到结论回写账号池：今日已签 → 按钮显示「已签」；会话死亡 → 停用。
func (p *Panel) qoderAfterCheckin(a *auth.Auth, res qoder.CheckinResult, qerr *qoder.Error) {
	if res.CheckedIn {
		p.cfg.Pool.NoteCheckinDone(a.UID)
	}
	if qerr != nil && qerr.Kind == qoder.KindSessionDead {
		p.cfg.Pool.Disable(a.UID, "qoder session expired (checkin): 需要重新登录")
	}
	log.Printf("panel: qoder checkin acct=%s ok=%v earned=%v msg=%q", logfmt.Label(a.UID, a.Nickname), res.OK, res.Earned, res.Message)
}

// qoderAccountCheckin POST /panel/api/qoder/accounts/{uid}/checkin {mode: daily|all|pro}
func (p *Panel) qoderAccountCheckin(w http.ResponseWriter, r *http.Request) {
	a := p.cfg.Pool.AuthByUID(r.PathValue("uid"))
	if a == nil || !a.IsQoder() {
		writeErr(w, http.StatusNotFound, "qoder account not found")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	_ = readJSONBody(r, &req)
	p.qoderCheckinOne(w, r, a, parseCheckinMode(req.Mode))
}

// qoderAccounts 池内全部 Qoder 账号（禁用号默认不含）。
func (p *Panel) qoderAccounts(includeDisabled bool) []*auth.Auth {
	var out []*auth.Auth
	for _, st := range p.cfg.Pool.List() {
		if st.Platform != auth.PlatformQoder || (st.Disabled && !includeDisabled) {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.IsQoder() {
			out = append(out, a)
		}
	}
	return out
}

// qoderCheckinAll POST /panel/api/qoder/checkin_all {mode: all|daily|pro}
// 逐号执行（账号间留 1 秒间隔，与原实现防风控节奏一致），同步返回每号结论。
func (p *Panel) qoderCheckinAll(w http.ResponseWriter, r *http.Request) {
	if !p.qoderEnabled(w) {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	_ = readJSONBody(r, &req)
	mode := parseCheckinMode(req.Mode)
	accts := p.qoderAccounts(false)
	results := make([]map[string]any, 0, len(accts))
	var earned float64
	for i, a := range accts {
		if i > 0 {
			select {
			case <-r.Context().Done():
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "results": results, "error": "cancelled"})
				return
			case <-time.After(time.Second):
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		res, qerr := p.cfg.Qoder.Checkin(ctx, a, mode)
		p.qoderAfterCheckin(a, res, qerr)
		_, _, _ = p.qoderRefreshCredits(ctx, a)
		cancel()
		earned += res.Earned
		results = append(results, qoderCheckinView(a, res, qerr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": string(mode), "earned": earned, "results": results})
}

// qoderCampaigns GET /panel/api/qoder/campaigns 各账号的活动列表与签到留痕（只读）。
func (p *Panel) qoderCampaigns(w http.ResponseWriter, r *http.Request) {
	if !p.qoderEnabled(w) {
		return
	}
	accts := p.qoderAccounts(false)
	out := make([]map[string]any, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m := a.QoderMeta()
			row := map[string]any{
				"uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(), "plan": m.Plan,
				"last_checkin": m.LastCheckin, "last_message": m.CheckinNote,
				"checked_today": qoderacct.CheckedInToday(a, time.Now()),
			}
			if len(m.CampaignCode) > 0 {
				codes := make([]map[string]any, 0, len(m.CampaignCode))
				for id, c := range m.CampaignCode {
					codes = append(codes, map[string]any{"campaign_id": id, "campaign": c.Campaign, "code": c.Code, "at": c.At})
				}
				sort.Slice(codes, func(i, j int) bool { return codes[i]["at"].(int64) > codes[j]["at"].(int64) })
				row["codes"] = codes
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			list, qerr := p.cfg.Qoder.Client.CampaignsView(ctx, qoderacct.Cred(a))
			if qerr != nil {
				row["error"] = qerr.Error()
				out[i] = row
				return
			}
			items := make([]map[string]any, 0, len(list.Campaigns))
			for _, c := range list.Campaigns {
				items = append(items, map[string]any{
					"id": c.ID, "title": c.Title, "description": c.Description, "jump_url": c.JumpURL,
					"state": c.State, "state_label": c.StateLabel, "current": c.Current, "target": c.Target,
					"reward_credit": c.RewardCredit, "reward_text": c.RewardText, "end_at": c.EndAt,
					"required": c.RequiredAchievementKey,
				})
			}
			row["campaigns"] = items
			row["show_campaign"] = list.ShowCampaign
			row["identity"] = list.Identity
			row["identity_error"] = list.IdentityError
			row["hint"] = list.Hint
			out[i] = row
		}(i, a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// wbOnly 过滤出 WorkBuddy 账号（成长任务 / 开学季 / 补偿等 WorkBuddy 专属体系用）。
func wbOnly(list []pool.Status) []pool.Status {
	out := list[:0:0]
	for _, st := range list {
		if st.Platform != auth.PlatformQoder && st.Platform != auth.PlatformKiro {
			out = append(out, st)
		}
	}
	return out
}

// qoderModelEntries 「模型与档位」页的 Qoder 条目（字段与 panelModelEntry 同形，前端一张表渲染）。
func (p *Panel) qoderModelEntries(ctx context.Context, fetchErrs *[]string) []map[string]any {
	if p.cfg.Qoder == nil {
		return nil
	}
	var out []map[string]any
	for _, rr := range []string{auth.RealmQoderCN, auth.RealmQoderIntl} {
		uids := p.cfg.Pool.AvailableUIDsForRealm(rr)
		if len(uids) == 0 {
			continue
		}
		a := p.cfg.Pool.AuthByUID(uids[0])
		if a == nil || !a.IsQoder() {
			continue
		}
		cred := qoderacct.Cred(a)
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err := p.cfg.Qoder.Client.Models(cctx, cred.Realm, &cred)
		cancel()
		if err != nil && len(models) == 0 {
			*fetchErrs = append(*fetchErrs, rr+": "+err.Error())
			continue
		}
		for _, m := range models {
			credits := ""
			if m.PriceFactor > 0 {
				credits = "x" + strings.TrimRight(strings.TrimRight(strconvF(m.PriceFactor), "0"), ".")
			}
			e := map[string]any{
				"id":                 rr + ":" + m.ID,
				"name":               m.Name,
				"default_effort":     m.DefaultEffort,
				"supported_efforts":  m.Efforts,
				"supports_reasoning": m.Reasoning,
				"supports_images":    m.Vision,
				"supports_tool_call": m.ToolCalls,
				"credits":            credits,
				"description":        m.Description,
				"vendor":             "Qoder",
				"platform":           auth.PlatformQoder,
				"enabled":            m.Enabled,
				"disabled_reason":    m.DisabledReason,
			}
			if m.ContextLength > 0 {
				e["context_length"] = m.ContextLength
			}
			if m.MaxOutputTokens > 0 {
				e["max_output_tokens"] = m.MaxOutputTokens
			}
			if m.PriceFactorValley != nil {
				e["price_factor_valley"] = *m.PriceFactorValley
			}
			out = append(out, e)
		}
	}
	return out
}

func strconvF(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }
