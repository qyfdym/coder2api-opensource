// kiro.go 面板里 Kiro 平台的入口：添加账号（AWS Builder ID / IAM Identity Center 设备授权登录、
// 凭证导入）、余额与积分构成、「模型与档位」页的 Kiro 条目。
//
// Kiro 账号与其它平台账号在同一个账号池、同一张账号表里：解冻 / 禁用 / 暂停 / 移除走通用接口
// （按 uid），只有「余额」这类上游调用按平台分派到这里。协议实现在 internal/kiro，
// 凭证落盘在 internal/kiroacct。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// kiroEnabled 未启用 Kiro 时统一回 501。
func (p *Panel) kiroEnabled(w http.ResponseWriter) bool {
	if p.cfg.Kiro == nil || p.cfg.Kiro.Client == nil {
		writeErr(w, http.StatusNotImplemented, "kiro 未启用（config.kiro.enabled）")
		return false
	}
	return true
}

func kiroAccountBrief(a *auth.Auth) map[string]any {
	m := a.KiroMeta()
	return map[string]any{
		"uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(), "platform": a.Platform(),
		"provider": m.Provider, "auth_method": m.AuthMethod, "plan": m.Plan,
	}
}

// ---------------------------------------------------------------------------
// 添加账号：设备授权（Builder ID / IAM Identity Center）
// ---------------------------------------------------------------------------

// kiroLoginStart 发起一次设备授权。请求体 {start_url?, region?}：start_url 为空 = AWS Builder ID。
// 会话（含 OIDC 客户端凭证与 deviceCode）只保存在服务端内存，浏览器只拿会话 ID、验证码与链接。
func (p *Panel) kiroLoginStart(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	var req struct {
		StartURL string `json:"start_url"`
		Region   string `json:"region"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s, kerr := p.cfg.Kiro.Client.StartDeviceLogin(ctx, req.StartURL, req.Region)
	if kerr != nil {
		status := http.StatusBadGateway
		if kerr.Kind == kiro.KindBadRequest {
			status = http.StatusBadRequest
		}
		writeErr(w, status, kerr.Error())
		return
	}
	p.krLoginMu.Lock()
	now := time.Now()
	for k, v := range p.krLogins {
		if now.After(v.ExpiresAt) {
			delete(p.krLogins, k)
		}
	}
	p.krLogins[s.ID] = s
	p.krLoginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "state": s.ID, "url": s.VerificationURI, "user_code": s.UserCode,
		"provider": s.Provider, "interval": s.Interval, "expires_in": int(time.Until(s.ExpiresAt).Seconds()),
	})
}

// kiroLoginPoll 轮询授权结果；拿到 token 即建档、入池、刷新一次余额。
func (p *Panel) kiroLoginPoll(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	state := r.URL.Query().Get("state")
	p.krLoginMu.Lock()
	s := p.krLogins[state]
	p.krLoginMu.Unlock()
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "授权会话不存在或已过期，请重新发起"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res := p.cfg.Kiro.Client.PollDeviceLogin(ctx, s)
	drop := func() {
		p.krLoginMu.Lock()
		delete(p.krLogins, state)
		p.krLoginMu.Unlock()
	}
	switch res.Status {
	case "ok":
		drop()
		a, err := p.cfg.Kiro.NewAccountFromLogin(ctx, s, res.Tokens)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": "保存账号失败：" + err.Error()})
			return
		}
		p.kiroAddToPool(ctx, a)
		log.Printf("panel: kiro login ok acct=%s provider=%s", logfmt.Label(a.UID, a.Nickname), s.Provider)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": res.Message, "account": kiroAccountBrief(a)})
	case "pending", "slow_down":
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": res.Message, "interval": s.Interval})
	default:
		drop()
		writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "message": res.Message})
	}
}

// kiroLoginCancel 关闭弹窗时丢弃会话（幂等）。
func (p *Panel) kiroLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	_ = readJSONBody(r, &req)
	p.krLoginMu.Lock()
	_, existed := p.krLogins[req.State]
	delete(p.krLogins, req.State)
	p.krLoginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": existed})
}

// ---------------------------------------------------------------------------
// 添加账号：凭证导入
// ---------------------------------------------------------------------------

// kiroImportMaxBody 导入文本上限（一次几十个账号的 JSON 也只有几十 KB）。
const kiroImportMaxBody = 1 << 20

// kiroImport POST {text}：JSON / 行格式 / 单个 refresh token / ksk_ API Key（见 kiro.ParseImport）。
// 逐条导入：OAuth 凭证先刷新一次确认可用；成功的立即入池并刷新一次余额。
func (p *Panel) kiroImport(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, kiroImportMaxBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(raw) > kiroImportMaxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "导入内容超过 1MB")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	items, skipped := kiro.ParseImport(req.Text)
	if len(items) == 0 {
		msg := "没有可导入的账号"
		if len(skipped) > 0 {
			msg += "：" + strings.Join(skipped, "；")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg, "skipped": skipped})
		return
	}
	if len(items) > 200 {
		writeErr(w, http.StatusBadRequest, "一次最多导入 200 个账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	results := p.cfg.Kiro.Import(ctx, items)
	var accounts []map[string]any
	var failed []string
	for _, res := range results {
		if !res.OK {
			label := res.Nickname
			if label == "" {
				label = "账号"
			}
			failed = append(failed, label+"："+res.Error)
			continue
		}
		p.kiroAddToPool(ctx, res.Account)
		accounts = append(accounts, kiroAccountBrief(res.Account))
	}
	log.Printf("panel: kiro import: %d ok, %d failed, %d skipped", len(accounts), len(failed), len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": len(accounts) > 0, "imported": len(accounts), "accounts": accounts,
		"failed": failed, "skipped": skipped,
	})
}

// kiroAddToPool 新账号入池（同 uid 已存在时替换凭证、保留池状态）并刷新一次余额。
func (p *Panel) kiroAddToPool(ctx context.Context, a *auth.Auth) {
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	if _, _, err := p.kiroRefreshCredits(ctx, a); err != nil {
		log.Printf("panel: kiro 新账号余额查询失败 acct=%s: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// ---------------------------------------------------------------------------
// 余额与积分构成
// ---------------------------------------------------------------------------

type kiroDisabledErr struct{}

func (kiroDisabledErr) Error() string { return "kiro 未启用" }

// kiroRefreshCredits 查询额度并写回账号池（有额度即解冻；用完则硬冷却到下次重置）。
func (p *Panel) kiroRefreshCredits(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	if p.cfg.Kiro == nil {
		return 0, 0, kiroDisabledErr{}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remain, total, _, kerr := p.cfg.Kiro.Balance(ctx, a)
	if kerr != nil {
		if kerr.Kind == kiro.KindSuspended || kerr.Kind == kiro.KindSessionDead {
			p.cfg.Pool.Disable(a.UID, "kiro: "+kerr.Error())
		}
		return 0, 0, kerr
	}
	p.applyKiroCredits(a, remain, total)
	return remain, total, nil
}

func (p *Panel) applyKiroCredits(a *auth.Auth, remain, total int64) {
	p.cfg.Pool.ReenableIfCredits(a.UID, remain, total)
	p.cfg.Pool.SetCreditsSnapshot(a.UID, pool.CreditsSnapshot{Remain: remain, Total: total})
	if remain <= 0 && total > 0 {
		d := time.Until(kiroacct.NextReset(a, time.Now()))
		if d < time.Minute {
			d = time.Hour
		}
		p.cfg.Pool.Cooldown(a.UID, pool.CoolHard, d, "kiro 额度耗尽（等待额度重置）")
	}
}

// kiroPackages Kiro 额度明细映射成「积分构成」视图的包结构：基础额度 / 试用额度 / 附加额度。
func (p *Panel) kiroPackages(ctx context.Context, a *auth.Auth) ([]upstream.CreditPackage, int64, int64, error) {
	if p.cfg.Kiro == nil {
		return nil, 0, 0, kiroDisabledErr{}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remain, total, u, kerr := p.cfg.Kiro.Balance(ctx, a)
	if kerr != nil {
		return nil, 0, 0, kerr
	}
	p.applyKiroCredits(a, remain, total)
	pkg := func(name string, cur, limit float64, expires int64) upstream.CreditPackage {
		cp := upstream.CreditPackage{
			Name:   name,
			Remain: int64(math.Floor(math.Max(0, limit-cur))),
			Used:   int64(math.Ceil(cur)),
			Size:   int64(math.Round(limit)),
		}
		if expires > 0 {
			cp.ExpiresAt = expires * 1000
			cp.EndTime = time.Unix(expires, 0).Format(time.RFC3339)
		}
		return cp
	}
	var out []upstream.CreditPackage
	if u.Limit > 0 {
		name := "基础额度"
		if u.PlanTitle != "" {
			name += "（" + u.PlanTitle + "）"
		}
		out = append(out, pkg(name, u.Current, u.Limit, u.NextResetAt))
	}
	if strings.EqualFold(u.TrialStatus, "ACTIVE") && u.TrialLimit > 0 {
		out = append(out, pkg("试用额度", u.TrialCurrent, u.TrialLimit, u.TrialExpiresAt))
	}
	now := time.Now()
	for _, b := range u.Bonuses {
		if b.Limit <= 0 || (b.Status != "" && !strings.EqualFold(b.Status, "ACTIVE")) ||
			(b.ExpiresAt > 0 && time.Unix(b.ExpiresAt, 0).Before(now)) {
			continue
		}
		name := b.Name
		if name == "" {
			name = "附加额度"
		}
		out = append(out, pkg(name, b.Current, b.Limit, b.ExpiresAt))
	}
	return out, remain, total, nil
}

// ---------------------------------------------------------------------------
// 「模型与档位」页
// ---------------------------------------------------------------------------

// kiroModelEntries Kiro 条目（字段与 panelModelEntry 同形，前端一张表渲染）。池里没有 Kiro 账号时不列。
func (p *Panel) kiroModelEntries(ctx context.Context, fetchErrs *[]string) []map[string]any {
	if p.cfg.Kiro == nil || p.cfg.Kiro.Client == nil {
		return nil
	}
	uids := p.cfg.Pool.AvailableUIDsForRealm(auth.RealmKiro)
	var models []kiro.Model
	if len(uids) > 0 {
		if a := p.cfg.Pool.AuthByUID(uids[0]); a != nil && a.IsKiro() {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if kerr := p.cfg.Kiro.EnsureFresh(cctx, a, 5*time.Minute); kerr != nil {
				*fetchErrs = append(*fetchErrs, "kiro: "+kerr.Error())
				models = p.cfg.Kiro.Client.CachedModels()
			} else if list, kerr := p.cfg.Kiro.Client.RefreshModels(cctx, kiroacct.Cred(a)); kerr != nil {
				*fetchErrs = append(*fetchErrs, "kiro: "+kerr.Error())
				models = p.cfg.Kiro.Client.CachedModels()
			} else {
				models = list
			}
			cancel()
		}
	} else if total, _, _, _, _ := p.cfg.Pool.CountsDetailedForRealm(auth.RealmKiro); total > 0 {
		models = p.cfg.Kiro.Client.CachedModels()
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		credits := ""
		if m.RateMultiplier > 0 {
			credits = "x" + strconv.FormatFloat(m.RateMultiplier, 'f', -1, 64)
		}
		e := map[string]any{
			"id":   auth.RealmKiro + ":" + m.ID,
			"name": m.Name,
			// Kiro 思考默认关闭：请求带 reasoning_effort / thinking 才注入思考提示。
			"default_effort":       "off",
			"supported_efforts":    kiro.SupportedEfforts,
			"can_disable_thinking": true,
			"supports_reasoning":   true,
			"supports_images":      m.Vision,
			"supports_tool_call":   true,
			"credits":              credits,
			"description":          m.Description,
			"vendor":               "Kiro",
			"platform":             auth.PlatformKiro,
			"enabled":              true,
		}
		if m.MaxInput > 0 {
			e["context_length"] = m.MaxInput
		}
		if m.MaxOutput > 0 {
			e["max_output_tokens"] = m.MaxOutput
		}
		out = append(out, e)
	}
	return out
}
