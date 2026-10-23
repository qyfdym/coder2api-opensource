package server

// qoder.go Qoder 平台在网关 handler 里的接入点。
//
// Qoder 账号与 WorkBuddy 账号同在一个账号池：选号、冷却、熔断、粘性会话、在途上限全部
// 共用；这里只放两平台真正不同的部分——模型名到路由域的决定、错误分类到账号处置的映射、
// /v1/models 里的 Qoder 条目。协议本身在 internal/qoder，凭证运维在 internal/qoderacct。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// qoderDefaultRealm qoder: 前缀两区都有时的优先路由域。
func (h *Handler) qoderDefaultRealm() string {
	if h.cfg.QoderDefaultRealm == auth.RealmQoderIntl {
		return auth.RealmQoderIntl
	}
	return auth.RealmQoderCN
}

// resolveQoderAuto 为 "qoder:X" 决定路由域与是否允许两区回退。
//
// 规则（与方案一致）：X 只在一个区有 → 只用那个区；两区都有 → 默认区优先、该区选不到号时
// 回退另一区；目录里查不到（新模型 / 拼写）→ 默认区、不回退（由上游给出明确的模型错误）。
// 目录查询只读内存 / 磁盘缓存与内置快照，不打网络。
func (h *Handler) resolveQoderAuto(bare string) (realm string, cross bool) {
	def := h.qoderDefaultRealm()
	if h.cfg.Qoder == nil {
		return def, false
	}
	realms := h.cfg.Qoder.ModelRealms(bare)
	switch len(realms) {
	case 0:
		return def, false
	case 1:
		return realms[0], false
	default:
		return def, true
	}
}

// hiddenKey models.hidden 的判定键（"<路由域>:<模型>"）。
// Qoder 模型接受多种写法（官方名 / 内部 key / 小写别名），先归一成目录里的 id——
// 与 /v1/models 列出的 id 同口径，否则换个大小写就能绕过黑名单。
func (h *Handler) hiddenKey(realm, bare string) string {
	if auth.IsQoderRealm(realm) && h.cfg.Qoder != nil && h.cfg.Qoder.Client != nil {
		if m, ok := h.cfg.Qoder.Client.LookupModel(qoderacct.Region(realm), bare); ok {
			return realm + ":" + m.ID
		}
	}
	if auth.IsKiroRealm(realm) {
		// Kiro 模型同样接受多种写法（claude-sonnet-4-5 / claude-sonnet-4.5 / -thinking 后缀）。
		return realm + ":" + h.kiroModelID(bare)
	}
	return realm + ":" + bare
}

// modelRate 积分倍率（用量记账与积分保底用）：Qoder 域取 Qoder 目录的 price_factor，
// Kiro 域取 Kiro 目录的 rateMultiplier。
func (h *Handler) modelRate(realm, bare string) string {
	if auth.IsQoderRealm(realm) {
		if h.cfg.Qoder == nil {
			return ""
		}
		return h.cfg.Qoder.ModelRate(realm, bare)
	}
	if auth.IsKiroRealm(realm) {
		return h.cfg.Kiro.ModelRate(bare)
	}
	if h.cfg.Upstream == nil {
		return ""
	}
	return h.cfg.Upstream.ModelRate(realm, bare)
}

// qoderUpstreamError 把 Qoder 错误映射成网关统一错误信封，复用 handler 既有的
// 「fail-fast / 换号 / 末端透传」控制流。账号处置不走 applyErrorPolicy（那是 WorkBuddy
// 业务码口径），而是 applyQoderPolicy。
//
// 返回 nil 表示应按「传输层错误」处理（非 *upstream.Error，走换号 + 连败计数分支）。
func qoderUpstreamError(qe *qoder.Error) *upstream.Error {
	if qe == nil || qe.Kind == qoder.KindTransport {
		return nil
	}
	kind := upstream.ErrServer
	switch qe.Kind {
	case qoder.KindContent:
		kind = upstream.ErrContentBlocked
	case qoder.KindBadRequest:
		kind = upstream.ErrBadParams
	case qoder.KindTooLarge:
		kind = upstream.ErrPromptTooLong
	case qoder.KindQueued, qoder.KindVersion:
		// 排队（包内已按 retryAfterSeconds 等过）/ 客户端版本过低：换号也撞同一堵墙，
		// 不罚号、不轮转，503 透传给客户端。
		kind = upstream.ErrUpstreamBusy
	case qoder.KindRateLimit:
		kind = upstream.ErrSoftRate
	case qoder.KindSessionDead:
		kind = upstream.ErrSessionDead
	case qoder.KindAuth:
		kind = upstream.ErrAccountFault
	case qoder.KindQuota:
		kind = upstream.ErrHardCredit
	case qoder.KindModelUnavailable:
		kind = upstream.ErrModelBlocked
	}
	status := qe.Status
	if status < 400 {
		status = 502
	}
	msg := qe.Body
	if msg == "" {
		msg = qe.Msg
	} else if qe.Msg != "" && qe.Msg != qe.Body {
		msg = qe.Msg + " | " + qe.Body
	}
	return &upstream.Error{Kind: kind, Status: status, Msg: msg, RetryAfter: qe.RetryAfter}
}

// qoderKindOf 从网关错误信封反推 Qoder 类别（流内 error 帧 / 包装后的错误统一入口）。
func qoderKindOf(kind upstream.ErrKind) qoder.ErrKind {
	switch kind {
	case upstream.ErrSoftRate:
		return qoder.KindRateLimit
	case upstream.ErrSessionDead:
		return qoder.KindSessionDead
	case upstream.ErrAccountFault:
		return qoder.KindAuth
	case upstream.ErrHardCredit:
		return qoder.KindQuota
	case upstream.ErrModelBlocked:
		return qoder.KindModelUnavailable
	case upstream.ErrContentBlocked:
		return qoder.KindContent
	case upstream.ErrBadParams:
		return qoder.KindBadRequest
	case upstream.ErrPromptTooLong:
		return qoder.KindTooLarge
	case upstream.ErrUpstreamBusy:
		return qoder.KindQueued
	default:
		return qoder.KindServer
	}
}

// qoderAuthCooldown 401/403（非会话死亡）后的短冷却：同时把 token 标成「需刷新」，
// 下一次选中它时先刷新再用（原实现同样是先刷新、仍失败才判死）。
const qoderAuthCooldown = 60 * time.Second

// applyQoderPolicy Qoder 账号出错后的账号处置（与 applyErrorPolicy 对位，但按 Qoder 语义）。
func (h *Handler) applyQoderPolicy(acct *auth.Auth, kind qoder.ErrKind, model string, retryAfter time.Duration) {
	uid := acct.UID
	switch kind {
	case qoder.KindRateLimit:
		// 账号级 429：原实现 10 分钟起翻倍、封顶 2 小时——与网关 soft_rate 退避同构。
		var resetAt time.Time
		if retryAfter > 0 {
			resetAt = time.Now().Add(retryAfter)
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "qoder 429 rate limit")
	case qoder.KindSessionDead:
		h.cfg.Pool.Disable(uid, "qoder session expired (TOKEN_EXPIRE / 12153): 需要重新登录")
	case qoder.KindAuth:
		acct.SetTokens("", "", 0) // ExpiresAt=0 → NeedsRefresh，下次选中先刷新
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, qoderAuthCooldown, "qoder auth rejected (401/403)")
	case qoder.KindQuota:
		// 额度耗尽：停放到下一个签到窗口（每天 10:00 UTC+8 刷新，原实现 402 停放口径）。
		d := time.Until(qoder.NextCheckinWindow(time.Now()))
		if d < time.Minute {
			d = time.Minute
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolHard, d, "qoder 额度耗尽")
	case qoder.KindModelUnavailable:
		h.cfg.Pool.BlockModelBackoff(uid, model, "qoder model unavailable for this account")
	case qoder.KindServer:
		h.cfg.Pool.NoteError(uid)
	case qoder.KindTransport:
		h.cfg.Pool.NoteFailures(uid)
	default:
		// 内容审核 / 参数错误 / 请求过大 / 排队 / 版本过低：请求或渠道的问题，不罚号。
	}
}

// refreshQoder Qoder 账号 token 将过期时先刷新（与 WorkBuddy 的 RefreshToken 分支对位）。
// 返回的 error 保留 Qoder 分类，调用方据此停用或计错后换号。
func (h *Handler) refreshQoder(ctx context.Context, acct *auth.Auth) error {
	if h.cfg.Qoder == nil {
		return errors.New("qoder not enabled")
	}
	if err := h.cfg.Qoder.EnsureFresh(ctx, acct, h.cfg.RefreshSkew); err != nil {
		return err
	}
	return nil
}

// qoderModelEntries /v1/models 里的 Qoder 条目（id = "qoder:" + 官方模型名）。
//
// 只列池里**有 Qoder 账号**的区域：没号的区域列出来客户端也调不通。同名模型两区都有时只列
// 一次（qoder: 前缀会按默认区优先、选不到回退另一区）；显式 qoder-cn: / qoder-intl: 照常可调。
// 目录优先用该区一个可用账号拉动态目录（qoder 包内存缓存 5 分钟），失败退回磁盘缓存 / 内置快照。
// models.hidden：写 "qoder:X" 隐藏两区，写 "qoder-cn:X" 只藏该区；所有可用区都被隐藏才不列。
func (h *Handler) qoderModelEntries(hidden map[string]struct{}) []map[string]any {
	if h.cfg.Qoder == nil {
		return nil
	}
	type seenModel struct {
		m      qoder.Model
		realms []string
	}
	byID := map[string]*seenModel{}
	var order []string
	for _, rr := range []string{h.qoderDefaultRealm(), otherRealm(h.qoderDefaultRealm())} {
		total, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(rr)
		if total == 0 {
			continue
		}
		var cred *qoder.Cred
		if uids := h.cfg.Pool.AvailableUIDsForRealm(rr); len(uids) > 0 {
			if a := h.cfg.Pool.AuthByUID(uids[0]); a != nil && a.IsQoder() {
				c := qoderacct.Cred(a)
				cred = &c
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, _ := h.cfg.Qoder.Client.Models(ctx, qoderacct.Region(rr), cred)
		cancel()
		for _, m := range models {
			if !m.Enabled {
				continue
			}
			if _, hide := hidden[rr+":"+m.ID]; hide {
				continue
			}
			if s, ok := byID[m.ID]; ok {
				s.realms = append(s.realms, rr)
				continue
			}
			byID[m.ID] = &seenModel{m: m, realms: []string{rr}}
			order = append(order, m.ID)
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, id := range order {
		s := byID[id]
		m := s.m
		sort.Strings(s.realms)
		entry := map[string]any{
			"id":       "qoder:" + id,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "qoder",
			"realms":   s.realms,
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		if m.Description != "" {
			entry["description"] = m.Description
		}
		if m.ContextLength > 0 {
			entry["context_length"] = m.ContextLength
		}
		if m.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = m.MaxOutputTokens
		}
		if len(m.Efforts) > 0 {
			entry["reasoning_supported_efforts"] = m.Efforts
			if m.DefaultEffort != "" {
				entry["reasoning_default_effort"] = m.DefaultEffort
			}
		}
		if m.Vision {
			entry["supports_images"] = true
		}
		if m.ToolCalls {
			entry["supports_tool_call"] = true
		}
		if m.Reasoning {
			entry["supports_reasoning"] = true
		}
		if m.PriceFactor > 0 {
			entry["price_factor"] = m.PriceFactor
		}
		out = append(out, entry)
	}
	return out
}

// firstSSEErrorFrame 在一段 SSE 字节里找第一个 error 帧，返回其 data 内容（没有返回空串）。
func firstSSEErrorFrame(buf []byte) string {
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" || !strings.Contains(data, `"error"`) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) == nil {
			if _, ok := obj["error"]; ok {
				return data
			}
		}
	}
	return ""
}

// errorFrameCodeMsg 取 error 帧里的 code / message（缺失时用给定缺省值）。
func errorFrameCodeMsg(frame, defCode, defMsg string) (string, string) {
	var obj struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(frame), &obj) != nil {
		return defCode, defMsg
	}
	code, msg := defCode, defMsg
	if obj.Error.Type != "" {
		code = obj.Error.Type
	}
	if obj.Error.Message != "" {
		msg = obj.Error.Message
	}
	return code, msg
}
