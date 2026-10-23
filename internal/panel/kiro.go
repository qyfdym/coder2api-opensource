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

// listAccounts 账号池状态列表；Kiro 账号补上当前实际使用的出口（脱敏）。出口由 Kiro 客户端
// 按账号 / 代理池 / 默认选出，账号池本身不知道，所以在面板这一层填。
func (p *Panel) listAccounts() []pool.Status {
	list := p.cfg.Pool.List()
	if p.cfg.Kiro == nil || p.cfg.Kiro.Client == nil {
		return list
	}
	for i := range list {
		if list[i].Platform != auth.PlatformKiro {
			continue
		}
		a := p.cfg.Pool.AuthByUID(list[i].UID)
		if a == nil || !a.IsKiro() {
			continue
		}
		info := p.cfg.Kiro.Client.ProxyInfo(kiroacct.Cred(a))
		list[i].ProxyKind, list[i].Proxy, list[i].ProxyDown = info.Kind, info.Label, info.Down
	}
	return list
}

// kiroProxySet POST {"proxy_url": ""|"direct"|"<地址>"}：设置账号自己的出站代理。
// 空 = 恢复自动（代理池 / 平台默认）；direct = 这个账号不走代理。
// Kiro 按出口 IP 的地区放出模型，换出口后该账号缓存的模型目录与「模型不可用」冷却都按旧出口算的，一并作废。
func (p *Panel) kiroProxySet(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	var req struct {
		ProxyURL *string `json:"proxy_url"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := readJSONBody(r, &req); err != nil || req.ProxyURL == nil {
		writeErr(w, http.StatusBadRequest, "请求体需要 {\"proxy_url\": \"...\"}（留空 = 恢复自动，direct = 不走代理）")
		return
	}
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || !a.IsKiro() {
		writeErr(w, http.StatusNotFound, "不是 Kiro 账号或账号不存在")
		return
	}
	v, err := kiro.NormalizeProxyURL(*req.ProxyURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.UpdateKiro(func(m *auth.KiroMeta) { m.ProxyURL = v })
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存账号失败："+err.Error())
		return
	}
	p.cfg.Kiro.Client.InvalidateCatalog(uid)
	p.cfg.Pool.ClearModelCooldowns(uid)
	info := p.cfg.Kiro.Client.ProxyInfo(kiroacct.Cred(a))
	log.Printf("panel: kiro proxy acct=%s -> %s %s", logfmt.Label(a.UID, a.Nickname), info.Kind, info.Label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_kind": info.Kind, "proxy": info.Label, "proxy_down": info.Down})
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
	portal := p.krPortals[req.State]
	delete(p.krPortals, req.State)
	p.krLoginMu.Unlock()
	if portal != nil {
		existed = true
		portal.wipe()
	}
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
		// Mode "" / "credentials" = 凭证文本；"sso_token" = 每行一个 x-amz-sso_authn。
		Mode   string `json:"mode"`
		Region string `json:"region"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	if strings.EqualFold(strings.TrimSpace(req.Mode), "sso_token") {
		var tokens []string
		for _, line := range strings.Split(req.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				tokens = append(tokens, line)
			}
		}
		if len(tokens) == 0 {
			writeErr(w, http.StatusBadRequest, "请粘贴 SSO token（x-amz-sso_authn 的值），每行一个")
			return
		}
		if len(tokens) > kiroSSOTokenMax {
			writeErr(w, http.StatusBadRequest, "SSO token 一次最多导入 "+strconv.Itoa(kiroSSOTokenMax)+" 个")
			return
		}
		p.kiroFinishImport(ctx, w, p.cfg.Kiro.ImportSSOTokens(ctx, tokens, req.Region), nil, "sso_token")
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
	p.kiroFinishImport(ctx, w, p.cfg.Kiro.Import(ctx, items), skipped, "credentials")
}

// kiroSSOTokenMax 一次最多导入的 SSO token 数（每个要走 7 步上游调用）。
const kiroSSOTokenMax = 20

// kiroFinishImport 导入结果统一收尾：成功的入池并刷新余额，失败的汇总原因。
func (p *Panel) kiroFinishImport(ctx context.Context, w http.ResponseWriter, results []kiroacct.ImportResult, skipped []string, kind string) {
	var accounts []map[string]any
	var failed, warnings []string
	for _, res := range results {
		label := res.Nickname
		if label == "" {
			label = "账号"
		}
		if !res.OK {
			failed = append(failed, label+"："+res.Error)
			continue
		}
		if res.Warning != "" {
			// 没能联网校验：直接入池，不再为它查余额（会再等一轮超时），余额由后台刷新补上。
			p.cfg.Pool.Add(res.Account)
			p.cfg.Pool.Revive(res.Account.UID)
			warnings = append(warnings, label+"："+res.Warning)
		} else {
			p.kiroAddToPool(ctx, res.Account)
		}
		accounts = append(accounts, kiroAccountBrief(res.Account))
	}
	log.Printf("panel: kiro import(%s): %d ok (%d unverified), %d failed, %d skipped", kind, len(accounts), len(warnings), len(failed), len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": len(accounts) > 0, "imported": len(accounts), "accounts": accounts,
		"failed": failed, "skipped": skipped, "warnings": warnings,
	})
}

// kiroImportLocal POST {}：导入网关所在机器上的 Kiro IDE 登录缓存（~/.aws/sso/cache）。
// 只读固定的两类文件；网关在别的机器 / 容器里时读不到，会如实说明。
func (p *Panel) kiroImportLocal(w http.ResponseWriter, r *http.Request) {
	if !p.kiroEnabled(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	results, skipped := p.cfg.Kiro.ImportLocalCache(ctx)
	if len(results) == 0 {
		msg := "没有可导入的本机 Kiro IDE 登录缓存"
		if len(skipped) > 0 {
			msg += "：" + strings.Join(skipped, "；")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg, "skipped": skipped})
		return
	}
	p.kiroFinishImport(ctx, w, results, skipped, "local_cache")
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
	if kiroacct.ShouldCoolOnExhausted(a, remain, total) {
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
	// 目录按账号授权返回：模型页展示的是可用账号目录的并集；打开页面即强制重拉（最多 4 个账号）。
	var accts []*auth.Auth
	for _, uid := range p.cfg.Pool.AvailableUIDsForRealm(auth.RealmKiro) {
		if a := p.cfg.Pool.AuthByUID(uid); a != nil && a.IsKiro() {
			accts = append(accts, a)
		}
	}
	var models []kiro.Model
	if len(accts) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		for _, e := range p.cfg.Kiro.RefreshCatalogs(cctx, accts, kiroacct.CatalogOpts{Force: true, EnsureFresh: true}) {
			*fetchErrs = append(*fetchErrs, "kiro: "+e)
		}
		cancel()
		models = p.cfg.Kiro.Client.CachedModels()
	} else if total, _, _, _, _ := p.cfg.Pool.CountsDetailedForRealm(auth.RealmKiro); total > 0 {
		models = p.cfg.Kiro.Client.CachedModels()
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		credits := ""
		if m.RateMultiplier > 0 {
			credits = "x" + strconv.FormatFloat(m.RateMultiplier, 'f', -1, 64)
		}
		// 没有原生档位的模型：思考默认关闭，请求带 reasoning_effort / thinking 才注入思考提示；
		// 有原生档位的模型：档位与默认档取目录里的声明（见 kiro.Model.EffortInfo）。
		efforts, defEffort, canDisable := m.EffortInfo()
		e := map[string]any{
			"id":                   auth.RealmKiro + ":" + m.ID,
			"name":                 m.Name,
			"default_effort":       defEffort,
			"supported_efforts":    efforts,
			"can_disable_thinking": canDisable,
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
