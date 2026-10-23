package panel

// kiro_portal.go Kiro 网页登录（Google / GitHub / Microsoft 企业 SSO）与超额开关的面板接口。
//
// 网页登录不需要网关监听 3128：用户在浏览器登录后，把「无法访问 localhost」页面的地址栏 URL 粘回来，
// 服务端从里面取授权码 / 企业身份源描述符（协议见 internal/kiro/portal.go）。PKCE verifier、state、
// 以及企业 SSO 多 profile 时暂存的临时凭证只留在服务端内存，完成 / 取消 / 过期即清除。

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

const (
	krPortalMax          = 64
	krPortalSelectionTTL = 10 * time.Minute
)

// krPortal 一个进行中的网页登录；pending / profiles 非空 = 登录已完成，正在等用户挑 profile。
type krPortal struct {
	sess     *kiro.PortalSession
	expires  time.Time
	pending  *kiro.PortalResult
	profiles []kiro.Profile
}

// wipe 清除会话与暂存凭证里的敏感材料（取消 / 过期 / 完成时调用）。
func (g *krPortal) wipe() {
	if g == nil {
		return
	}
	if g.sess != nil {
		g.sess.Retire()
	}
	if g.pending != nil {
		g.pending.Tokens = kiro.TokenSet{}
		g.pending = nil
	}
	g.profiles = nil
}

func (p *Panel) krPortalGet(state string) *krPortal {
	p.krLoginMu.Lock()
	defer p.krLoginMu.Unlock()
	return p.krPortals[state]
}

func (p *Panel) krPortalDrop(state string) {
	p.krLoginMu.Lock()
	g := p.krPortals[state]
	delete(p.krPortals, state)
	p.krLoginMu.Unlock()
	g.wipe()
}

// kiroPortalStart 发起网页登录。返回 Kiro 登录页地址，会话 ID 即后续接口的 state。
func (p *Panel) kiroPortalStart(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	sess, loginURL := p.cfg.Kiro.Client.StartPortalLogin()
	now := time.Now()
	p.krLoginMu.Lock()
	var expired []*krPortal
	for k, v := range p.krPortals {
		if now.After(v.expires) {
			expired = append(expired, v)
			delete(p.krPortals, k)
		}
	}
	if len(p.krPortals) >= krPortalMax {
		p.krLoginMu.Unlock()
		for _, v := range expired {
			v.wipe()
		}
		sess.Retire()
		writeErr(w, http.StatusTooManyRequests, "进行中的网页登录太多，请先取消或等待过期")
		return
	}
	p.krPortals[sess.ID] = &krPortal{sess: sess, expires: sess.ExpiresAt}
	p.krLoginMu.Unlock()
	for _, v := range expired {
		v.wipe()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "state": sess.ID, "url": loginURL, "stage": sess.Stage(),
		"expires_in": int(time.Until(sess.ExpiresAt).Seconds()),
	})
}

func profileBrief(ps []kiro.Profile) []map[string]any {
	out := make([]map[string]any, 0, len(ps))
	for _, pr := range ps {
		out = append(out, map[string]any{"arn": pr.ARN, "name": pr.Name, "region": pr.Region})
	}
	return out
}

// kiroPortalComplete 处理用户粘回的一个回调地址：
//
//	status=next            → 企业 SSO：返回第二段（Microsoft）登录地址，继续粘贴回调；
//	status=select_profile  → 企业 SSO 登录完成，账号有多个 profile，需要用户挑一个；
//	status=ok              → 账号已建档入池；
//	status=error           → 失败；retry=true 表示会话还在，可以改正后重新粘贴。
func (p *Panel) kiroPortalComplete(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	var req struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.State) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		writeErr(w, http.StatusBadRequest, "state 与 callback_url 不能为空")
		return
	}
	g := p.krPortalGet(req.State)
	if g == nil || g.sess == nil || g.pending != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "retry": false, "message": "登录会话不存在或已过期，请重新发起"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	prog, kerr := p.cfg.Kiro.Client.ContinuePortalLogin(ctx, g.sess, req.CallbackURL)
	if kerr != nil {
		retry := !g.sess.Expired()
		if !retry {
			p.krPortalDrop(req.State)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "retry": retry, "message": kerr.Error()})
		return
	}
	if prog.AuthorizationURL != "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "next", "stage": g.sess.Stage(), "url": prog.AuthorizationURL})
		return
	}
	res := prog.Result
	if res == nil {
		p.krPortalDrop(req.State)
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "retry": false, "message": "登录没有返回凭证，请重新发起"})
		return
	}

	// 企业 SSO：列出 profile；多于一个先让用户挑，再建档。
	var profileArn, warning string
	if res.Kind == kiro.AuthMethodExternalIDP {
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		profiles, perr := p.cfg.Kiro.PortalProfiles(pctx, res)
		pcancel()
		switch {
		case perr != nil:
			warning = "账号已添加，但暂时没能确认 Kiro profile（稍后会自动补全）"
		case len(profiles) > 1:
			p.krLoginMu.Lock()
			g.pending, g.profiles, g.expires = res, profiles, time.Now().Add(krPortalSelectionTTL)
			p.krLoginMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "select_profile", "message": "该账号有多个 Kiro profile，请选择要使用的一个",
				"profiles": profileBrief(profiles), "expires_in": int(krPortalSelectionTTL.Seconds()),
			})
			return
		case len(profiles) == 1:
			profileArn = profiles[0].ARN
		}
	}
	p.krPortalFinish(ctx, w, req.State, res, profileArn, warning)
}

// krPortalFinish 建档入池并回包；无论成败都销毁该登录会话。
func (p *Panel) krPortalFinish(ctx context.Context, w http.ResponseWriter, state string, res *kiro.PortalResult, profileArn, warning string) {
	a, err := p.cfg.Kiro.NewAccountFromPortal(ctx, res, profileArn)
	p.krPortalDrop(state)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "retry": false, "message": "保存账号失败：" + err.Error()})
		return
	}
	p.kiroAddToPool(ctx, a)
	log.Printf("panel: kiro portal login ok acct=%s kind=%s", logfmt.Label(a.UID, a.Nickname), res.Kind)
	out := map[string]any{"status": "ok", "account": kiroAccountBrief(a)}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

// kiroPortalSelect 企业 SSO 多 profile：用户挑一个后建档。只接受登录时列出的那几个。
func (p *Panel) kiroPortalSelect(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	var req struct {
		State      string `json:"state"`
		ProfileArn string `json:"profile_arn"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	g := p.krPortalGet(req.State)
	if g == nil || g.pending == nil || time.Now().After(g.expires) {
		p.krPortalDrop(req.State)
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "retry": false, "message": "选择已过期，请重新登录"})
		return
	}
	arn := strings.TrimSpace(req.ProfileArn)
	offered := false
	for _, pr := range g.profiles {
		offered = offered || pr.ARN == arn
	}
	if !offered {
		writeErr(w, http.StatusBadRequest, "所选 profile 不在登录时列出的范围内")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	p.krPortalFinish(ctx, w, req.State, g.pending, arn, "")
}

// ---------------------------------------------------------------------------
// 超额开关
// ---------------------------------------------------------------------------

func overageJSON(info kiroacct.OverageInfo) map[string]any {
	return map[string]any{
		"status": info.Status, "enabled": strings.EqualFold(info.Status, "ENABLED"),
		"capable": info.Capable, "capability": info.Capability,
		"cap_usd": info.CapUSD, "rate_usd": info.RateUSD, "current_usd": info.CurrentUSD,
		"subscription": info.SubscriptionTit,
	}
}

// kiroOverageGet 读取账号当前的超额开关状态与费用上限。
func (p *Panel) kiroOverageGet(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	a := p.cfg.Pool.AuthByUID(r.PathValue("uid"))
	if a == nil || !a.IsKiro() {
		writeErr(w, http.StatusNotFound, "不是 Kiro 账号或账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	info, kerr := p.cfg.Kiro.Overage(ctx, a)
	if kerr != nil {
		writeErr(w, http.StatusBadGateway, "kiro overage: "+kerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, overageJSON(info))
}

// kiroOverageSet 打开 / 关闭超额：请求体必须显式带 {"enabled": true|false}（打开意味着可能产生费用）。
// 打开后因「额度用完」被冷却的账号立即解冻；关闭后下一次额度刷新会重新按额度判断。
func (p *Panel) kiroOverageSet(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := readJSONBody(r, &req); err != nil || req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "请求体需要 {\"enabled\": true|false}")
		return
	}
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || !a.IsKiro() {
		writeErr(w, http.StatusNotFound, "不是 Kiro 账号或账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	info, kerr := p.cfg.Kiro.SetOverage(ctx, a, *req.Enabled)
	if kerr != nil {
		writeErr(w, http.StatusBadGateway, "kiro overage: "+kerr.Error())
		return
	}
	log.Printf("panel: kiro overage acct=%s enabled=%v", logfmt.Label(a.UID, a.Nickname), *req.Enabled)
	if *req.Enabled {
		if st, ok := p.cfg.Pool.Status(uid); ok && st.Cooling && st.CoolKind == "hard_credit" {
			p.cfg.Pool.Revive(uid)
		}
	}
	// 刷新一次额度：关闭超额且额度已用完时，这里会重新进入冷却。
	if _, _, err := p.kiroRefreshCredits(ctx, a); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("panel: kiro overage: 刷新额度失败 acct=%s: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
	writeJSON(w, http.StatusOK, overageJSON(info))
}
