package server

// kiro.go Kiro 平台在网关 handler 里的接入点（与 qoder.go 对位）。
//
// Kiro 账号与 WorkBuddy / Qoder 账号同在一个账号池：选号、冷却、熔断、粘性会话、在途上限
// 全部共用；这里只放 Kiro 真正不同的部分——错误分类到账号处置的映射、token 刷新、
// /v1/models 里的 Kiro 条目。协议本身在 internal/kiro，凭证运维在 internal/kiroacct。

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// kiroUpstreamError 把 Kiro 错误映射成网关统一错误信封，复用 handler 既有的
// 「fail-fast / 换号 / 末端透传」控制流；账号处置走 applyKiroPolicy。
// 返回 nil 表示按「传输层错误」处理（换号 + 连败计数分支）。
func kiroUpstreamError(ke *kiro.Error) *upstream.Error {
	if ke == nil || ke.Kind == kiro.KindTransport {
		return nil
	}
	kind := upstream.ErrServer
	switch ke.Kind {
	case kiro.KindContent:
		kind = upstream.ErrContentBlocked
	case kiro.KindBadRequest:
		kind = upstream.ErrBadParams
	case kiro.KindTooLarge:
		kind = upstream.ErrPromptTooLong
	case kiro.KindBusy:
		// 模型容量不足：换号也撞同一堵墙，不罚号、不轮转，503 透传（可跨平台回退）。
		kind = upstream.ErrUpstreamBusy
	case kiro.KindRateLimit:
		kind = upstream.ErrSoftRate
	case kiro.KindSessionDead:
		kind = upstream.ErrSessionDead
	case kiro.KindAuth, kiro.KindSuspended:
		kind = upstream.ErrAccountFault
	case kiro.KindQuota:
		kind = upstream.ErrHardCredit
	case kiro.KindModelUnavailable:
		kind = upstream.ErrModelBlocked
	}
	status := ke.Status
	if status < 400 {
		status = 502
	}
	msg := ke.Body
	if msg == "" {
		msg = ke.Msg
	} else if ke.Msg != "" && ke.Msg != ke.Body {
		msg = ke.Msg + " | " + ke.Body
	}
	return &upstream.Error{Kind: kind, Status: status, Msg: msg, RetryAfter: ke.RetryAfter}
}

// kiroKindOf 从网关错误信封反推 Kiro 类别（没有原始类别时用）。
func kiroKindOf(kind upstream.ErrKind) kiro.ErrKind {
	switch kind {
	case upstream.ErrSoftRate:
		return kiro.KindRateLimit
	case upstream.ErrSessionDead:
		return kiro.KindSessionDead
	case upstream.ErrAccountFault:
		return kiro.KindAuth
	case upstream.ErrHardCredit:
		return kiro.KindQuota
	case upstream.ErrModelBlocked:
		return kiro.KindModelUnavailable
	case upstream.ErrContentBlocked:
		return kiro.KindContent
	case upstream.ErrBadParams:
		return kiro.KindBadRequest
	case upstream.ErrPromptTooLong:
		return kiro.KindTooLarge
	case upstream.ErrUpstreamBusy:
		return kiro.KindBusy
	default:
		return kiro.KindServer
	}
}

// kiroAuthCooldown 401/403（非封禁）后的短冷却：同时把 token 标成「需刷新」，下次选中先刷新。
const kiroAuthCooldown = 60 * time.Second

// applyKiroPolicy Kiro 账号出错后的账号处置（与 applyQoderPolicy 对位，按 Kiro 语义）。
func (h *Handler) applyKiroPolicy(acct *auth.Auth, kind kiro.ErrKind, model string, retryAfter time.Duration) {
	uid := acct.UID
	switch kind {
	case kiro.KindRateLimit:
		var resetAt time.Time
		if retryAfter > 0 {
			resetAt = time.Now().Add(retryAfter)
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "kiro 429 throttled")
	case kiro.KindSessionDead:
		h.cfg.Pool.Disable(uid, "kiro 登录已失效（refresh token / OIDC 客户端过期）：需要重新登录")
	case kiro.KindSuspended:
		h.cfg.Pool.Disable(uid, "kiro 账号被临时封禁（TEMPORARILY_SUSPENDED）")
	case kiro.KindAuth:
		if kiroacct.IsAPIKey(acct) {
			// API Key 没有刷新这条路：被拒就是密钥失效 / 被撤销。
			h.cfg.Pool.Disable(uid, "kiro API Key 被拒（401/403）：密钥失效或已撤销")
			return
		}
		acct.SetTokens("", "", 0) // ExpiresAt=0 → 下次选中先刷新
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, kiroAuthCooldown, "kiro auth rejected (401/403)")
	case kiro.KindQuota:
		d := time.Until(kiroacct.NextReset(acct, time.Now()))
		if d < time.Minute {
			d = time.Minute
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolHard, d, "kiro 额度耗尽")
	case kiro.KindModelUnavailable:
		h.cfg.Pool.BlockModelBackoff(uid, model, "kiro model unavailable for this account")
	case kiro.KindServer:
		h.cfg.Pool.NoteError(uid)
	case kiro.KindTransport:
		h.cfg.Pool.NoteFailures(uid)
	default:
		// 内容拦截 / 参数错误 / 请求过大 / 容量不足 / 截断：请求或渠道的问题，不罚号。
	}
}

// refreshKiro Kiro 账号 token 将过期时先刷新；返回的 error 保留 Kiro 分类。
func (h *Handler) refreshKiro(ctx context.Context, acct *auth.Auth) error {
	if h.cfg.Kiro == nil {
		return errors.New("kiro not enabled")
	}
	if err := h.cfg.Kiro.EnsureFresh(ctx, acct, h.cfg.RefreshSkew); err != nil {
		return err
	}
	return nil
}

// kiroModelID 模型名在 Kiro 目录里的规范 id（找不到时按 Kiro 的版本号写法归一）。
func (h *Handler) kiroModelID(bare string) string {
	if h.cfg.Kiro != nil && h.cfg.Kiro.Client != nil {
		if m, ok := h.cfg.Kiro.Client.LookupModel(bare); ok {
			return m.ID
		}
		m, _ := kiro.ParseModel(bare, h.cfg.Kiro.Client.ThinkingSuffix())
		return m
	}
	m, _ := kiro.ParseModel(bare, "-thinking")
	return m
}

// kiroModelEntries /v1/models 里的 Kiro 条目（id = "kiro:" + 模型 id）。池里没有 Kiro 账号时不列。
// 思考：带 reasoning_effort（low … max）即开；模型名加 -thinking 后缀同样可用（max 档）。
func (h *Handler) kiroModelEntries(hidden map[string]struct{}) []map[string]any {
	if h.cfg.Kiro == nil || h.cfg.Kiro.Client == nil {
		return nil
	}
	if total, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(auth.RealmKiro); total == 0 {
		return nil
	}
	var cred *kiro.Cred
	if uids := h.cfg.Pool.AvailableUIDsForRealm(auth.RealmKiro); len(uids) > 0 {
		if a := h.cfg.Pool.AuthByUID(uids[0]); a != nil && a.IsKiro() && !kiroacct.NeedsRefresh(a, time.Minute) {
			c := kiroacct.Cred(a)
			cred = &c
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	models := h.cfg.Kiro.Client.Models(ctx, cred)
	cancel()
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if _, hide := hidden[auth.RealmKiro+":"+m.ID]; hide {
			continue
		}
		entry := map[string]any{
			"id":                          auth.RealmKiro + ":" + m.ID,
			"object":                      "model",
			"created":                     1753600000,
			"owned_by":                    "kiro",
			"supports_tool_call":          true,
			"supports_reasoning":          true,
			"reasoning_supported_efforts": kiro.SupportedEfforts,
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		if m.Description != "" {
			entry["description"] = m.Description
		}
		if m.MaxInput > 0 {
			entry["context_length"] = m.MaxInput
		}
		if m.MaxOutput > 0 {
			entry["max_output_tokens"] = m.MaxOutput
		}
		if m.Vision {
			entry["supports_images"] = true
		}
		if m.RateMultiplier > 0 {
			entry["price_factor"] = m.RateMultiplier
		}
		out = append(out, entry)
	}
	return out
}

// kiroEquivalent 其它平台模型 → Kiro 等价模型（同名优先，其次同厂商同档位）。
func (h *Handler) kiroEquivalent(bare string) (fallbackTarget, bool) {
	if h.cfg.Kiro == nil || h.cfg.Kiro.Client == nil || !h.realmHasAccounts(auth.RealmKiro) {
		return fallbackTarget{}, false
	}
	if m, ok := h.cfg.Kiro.Client.LookupModel(bare); ok {
		return fallbackTarget{realm: auth.RealmKiro, bare: m.ID, via: "same_name"}, true
	}
	var ids []string
	for _, m := range h.cfg.Kiro.Client.CachedModels() {
		ids = append(ids, m.ID)
	}
	// 其它平台的 Claude 写法常用横线版本号（claude-sonnet-4-5），先归一再按厂商档位匹配。
	norm, _ := kiro.ParseModel(bare, "")
	if name, ok := matchModelFamily(strings.ToLower(norm), ids); ok {
		return fallbackTarget{realm: auth.RealmKiro, bare: name, via: "family"}, true
	}
	return fallbackTarget{}, false
}
