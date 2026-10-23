package server

// fallback.go 跨平台回退：WorkBuddy 与 Qoder 在同一个账号池里，首选平台这次服务不了
// （没有可用号 / 该模型在所有号上冷却 / 轮转耗尽 / 渠道拦截 / 上游满载）时，把请求映射到
// 另一平台的等价模型再试一轮。
//
// 两个平台的模型名不一样（WorkBuddy 叫 deepseek-v4.1-flash，Qoder 叫 DeepSeek-Flash），
// 所以回退的核心是「找等价模型」，按优先级：
//  1. config models.fallback 显式映射（值为空串 = 该模型禁止回退）；
//  2. 同名（大小写不敏感；Qoder 侧走官方目录的别名解析）；
//  3. 同厂商 + 同档位（flash / pro / max / plus …）唯一匹配，多个时取版本号最高者。
// 找不到等价模型就不回退——宁可报错，也不悄悄换成一个能力 / 价格完全不同的模型。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fallbackTarget 回退目标：路由域 + 裸模型名 + 目标平台内是否允许两域互退。
type fallbackTarget struct {
	realm string
	bare  string
	cross bool
	// via 匹配方式（mapping / same_name / family），写进日志与请求记录说明。
	via string
}

// pendingChatErr 轮转循环内暂存的终止错误：canFallback=true 时先尝试跨平台回退，
// 回退不了（或回退也失败）才写回客户端。
type pendingChatErr struct {
	status      int
	code, msg   string
	hint        string
	canFallback bool
}

// joinHint 追加一段 gateway_hint（空段不加分隔符）。
func joinHint(hint, more string) string {
	switch {
	case strings.TrimSpace(more) == "":
		return hint
	case strings.TrimSpace(hint) == "":
		return more
	default:
		return hint + "; " + more
	}
}

// platformOfRealm 路由域所属平台（workbuddy / qoder / kiro）。
func platformOfRealm(realm string) string {
	switch {
	case auth.IsQoderRealm(realm) || realm == qoderAutoRealm:
		return auth.PlatformQoder
	case auth.IsKiroRealm(realm):
		return auth.PlatformKiro
	}
	return auth.PlatformWorkBuddy
}

// platformRealms 平台的全部路由域。
func platformRealms(platform string) []string {
	switch platform {
	case auth.PlatformQoder:
		return []string{auth.RealmQoderCN, auth.RealmQoderIntl}
	case auth.PlatformKiro:
		return []string{auth.RealmKiro}
	}
	return []string{"cn", "global"}
}

// fallbackPlatforms 跨平台回退时依次尝试的其它平台（顺序固定：WorkBuddy 优先其次 Qoder，
// Kiro 排最后——Kiro 是 Claude 系模型，与另外两个平台同名 / 同档位的模型最少）。
func fallbackPlatforms(realm string) []string {
	switch platformOfRealm(realm) {
	case auth.PlatformQoder:
		return []string{auth.PlatformWorkBuddy, auth.PlatformKiro}
	case auth.PlatformKiro:
		return []string{auth.PlatformWorkBuddy, auth.PlatformQoder}
	}
	return []string{auth.PlatformQoder, auth.PlatformKiro}
}

// platformUsable 平台此刻有没有可能接手（已装配 + 池里有该平台账号）。
func (h *Handler) platformUsable(platform string) bool {
	switch platform {
	case auth.PlatformQoder:
		if h.cfg.Qoder == nil {
			return false
		}
	case auth.PlatformKiro:
		if h.cfg.Kiro == nil {
			return false
		}
	}
	for _, r := range platformRealms(platform) {
		if h.realmHasAccounts(r) {
			return true
		}
	}
	return false
}

// usableOtherPlatformNames 首选平台失败时、池里有号的其它平台名（"Qoder" / "Qoder、Kiro"）；
// 没有返回空串。用于「其它平台有号但找不到等价模型」的提示。
func (h *Handler) usableOtherPlatformNames(realm string) string {
	var names []string
	for _, p := range fallbackPlatforms(realm) {
		if h.platformUsable(p) {
			names = append(names, platformName(platformRealms(p)[0]))
		}
	}
	return strings.Join(names, "、")
}

// chatFailNote 一句话说明某平台这一轮为什么没服务成（进请求记录的 note）。
func (h *Handler) chatFailNote(realm string, mb pool.ModelBlockStatus, lastErr error, pending *pendingChatErr) string {
	p := platformName(realm)
	switch {
	case mb.Blocked:
		s := fmt.Sprintf("%s：该模型在 %d 个号上冷却中（%s）", p, mb.Count, strings.Join(mb.Accounts, "、"))
		if !mb.Until.IsZero() {
			s += "，" + mb.Until.Local().Format("01-02 15:04") + " 解封"
		}
		return s
	case pending != nil:
		return p + "：" + pending.code
	case errors.Is(lastErr, errUpstreamTimeout):
		return p + "：上游超时"
	case lastErr != nil:
		var ue *upstream.Error
		if errors.As(lastErr, &ue) {
			return fmt.Sprintf("%s：%s（HTTP %d）", p, ue.Kind, ue.Status)
		}
		return p + "：" + logfmt.Truncate(lastErr.Error(), 120)
	case !h.realmHasAccounts(realm):
		return p + "：池里没有该域账号"
	default:
		return p + "：无可用账号（禁用 / 冷却 / 暂停 / 在途占满）"
	}
}

// chatResultNote 请求最终失败时的说明：首选平台原因 + 回退经过（或未回退的原因）。
func (h *Handler) chatResultNote(fallbackFrom, primaryNote, realm string, mb pool.ModelBlockStatus,
	lastErr error, pending *pendingChatErr, noEquivalent string) string {
	cur := h.chatFailNote(realm, mb, lastErr, pending)
	if fallbackFrom != "" {
		return primaryNote + "；回退后 " + cur
	}
	if noEquivalent != "" {
		return cur + "；" + noEquivalent + " 没有等价模型，未回退（可在 models.fallback 配映射）"
	}
	return cur
}

// platformName 路由域 → 平台展示名。
func platformName(realm string) string {
	switch platformOfRealm(realm) {
	case auth.PlatformQoder:
		return "Qoder"
	case auth.PlatformKiro:
		return "Kiro"
	}
	return "WorkBuddy"
}

// realmHasAccounts 该路由域在池里有没有账号（不论健康与否）。
func (h *Handler) realmHasAccounts(realm string) bool {
	total, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
	return total > 0
}

// lookupFallbackMapping 在 models.fallback 里找映射。依次尝试：客户端原样写法、
// 「路由域:模型」、平台通用写法（WorkBuddy 裸名 / qoder:模型）。大小写不敏感。
// found=true 且 value 为空串 = 显式禁止回退。
func lookupFallbackMapping(m map[string]string, reqModel, realm, bare string) (string, bool) {
	if len(m) == 0 {
		return "", false
	}
	keys := []string{reqModel, realm + ":" + bare}
	switch platformOfRealm(realm) {
	case auth.PlatformQoder:
		keys = append(keys, "qoder:"+bare)
	case auth.PlatformKiro:
		// 「kiro:模型」已经在上面（路由域就叫 kiro），不再匹配裸名：裸名键属于 WorkBuddy。
	default:
		keys = append(keys, bare)
	}
	for _, k := range keys {
		if v, ok := m[strings.ToLower(strings.TrimSpace(k))]; ok {
			return v, true
		}
	}
	return "", false
}

// crossPlatformTarget 为「首选平台服务不了」的请求找另一平台的等价模型。
func (h *Handler) crossPlatformTarget(reqModel, realm, bare string) (fallbackTarget, bool) {
	if v, ok := lookupFallbackMapping(h.loadLive().ModelFallback, reqModel, realm, bare); ok {
		if strings.TrimSpace(v) == "" {
			return fallbackTarget{}, false
		}
		tr, tb, explicit := resolveModelDetail(strings.TrimSpace(v))
		t := fallbackTarget{realm: tr, bare: tb, via: "mapping"}
		switch {
		case tr == qoderAutoRealm:
			if h.cfg.Qoder == nil {
				return fallbackTarget{}, false
			}
			t.realm, t.cross = h.resolveQoderAuto(tb)
		case auth.IsKiroRealm(tr):
			// Kiro 单一路由域，不做平台内互退。
		case !explicit:
			t.cross = h.cfg.Pool.CrossDomainFallback()
		}
		if (auth.IsQoderRealm(t.realm) && h.cfg.Qoder == nil) || (auth.IsKiroRealm(t.realm) && h.cfg.Kiro == nil) {
			return fallbackTarget{}, false
		}
		return t, h.realmHasAccounts(t.realm) || (t.cross && h.realmHasAccounts(otherRealm(t.realm)))
	}
	for _, p := range fallbackPlatforms(realm) {
		if !h.platformUsable(p) {
			continue
		}
		var (
			t  fallbackTarget
			ok bool
		)
		switch p {
		case auth.PlatformWorkBuddy:
			t, ok = h.workbuddyEquivalent(bare)
		case auth.PlatformQoder:
			t, ok = h.qoderEquivalent(bare)
		case auth.PlatformKiro:
			t, ok = h.kiroEquivalent(bare)
		}
		if ok {
			return t, true
		}
	}
	return fallbackTarget{}, false
}

// qoderEquivalent WorkBuddy 模型 → Qoder 等价模型（只看池里有 Qoder 账号的区）。
func (h *Handler) qoderEquivalent(bare string) (fallbackTarget, bool) {
	if h.cfg.Qoder == nil || h.cfg.Qoder.Client == nil {
		return fallbackTarget{}, false
	}
	var names []string
	seen := map[string]bool{}
	for _, rr := range []string{h.qoderDefaultRealm(), otherRealm(h.qoderDefaultRealm())} {
		if !h.realmHasAccounts(rr) {
			continue
		}
		region := qoderacct.Region(rr)
		// 同名：官方目录的别名解析（官方名 / key / 小写别名）。
		if m, ok := h.cfg.Qoder.Client.LookupModel(region, bare); ok && m.Enabled {
			realm, cross := h.resolveQoderAuto(m.ID)
			return fallbackTarget{realm: realm, bare: m.ID, cross: cross, via: "same_name"}, true
		}
		// 只读缓存 / 内置快照（cred=nil 不打网络）。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		models, _ := h.cfg.Qoder.Client.Models(ctx, region, nil)
		cancel()
		for _, m := range models {
			if m.Enabled && !seen[m.ID] {
				seen[m.ID] = true
				names = append(names, m.ID)
			}
		}
	}
	name, ok := matchModelFamily(bare, names)
	if !ok {
		return fallbackTarget{}, false
	}
	realm, cross := h.resolveQoderAuto(name)
	return fallbackTarget{realm: realm, bare: name, cross: cross, via: "family"}, true
}

// workbuddyEquivalent Qoder 模型 → WorkBuddy 等价模型（cn 优先，其次 global）。
// 目录只读缓存：cn 走 /v1/models 同一份 10 分钟缓存，global 走 upstream 侧探测缓存。
func (h *Handler) workbuddyEquivalent(bare string) (fallbackTarget, bool) {
	type realmModels struct {
		realm string
		ids   []string
	}
	var lists []realmModels
	if h.realmHasAccounts("cn") && h.cfg.Upstream != nil {
		var ids []string
		for _, mi := range h.fetchDynamicModels() {
			ids = append(ids, mi.ID)
		}
		lists = append(lists, realmModels{"cn", ids})
	}
	if h.cfg.GlobalEnabled && h.realmHasAccounts("global") && h.cfg.Upstream != nil {
		ids, _ := h.fetchGlobalModels()
		lists = append(lists, realmModels{"global", ids})
	}
	cross := h.cfg.Pool.CrossDomainFallback()
	for _, l := range lists {
		for _, id := range l.ids {
			if strings.EqualFold(id, bare) {
				return fallbackTarget{realm: l.realm, bare: id, cross: cross, via: "same_name"}, true
			}
		}
	}
	for _, l := range lists {
		if name, ok := matchModelFamily(bare, l.ids); ok {
			return fallbackTarget{realm: l.realm, bare: name, cross: cross, via: "family"}, true
		}
	}
	return fallbackTarget{}, false
}

// ---------------------------------------------------------------------------
// 同厂商 + 同档位匹配
// ---------------------------------------------------------------------------

// modelVendors 厂商前缀（按长度倒序匹配，避免 "hy" 抢走 "hunyuan"）。
var modelVendors = []string{"deepseek", "minimax", "hunyuan", "gemini", "claude", "qwen", "kimi", "glm", "gpt", "hy"}

// modelTiers 档位词：同厂商下 flash 与 pro 的能力、价格差很多，档位不同就不算等价。
// Claude 的 opus / sonnet / haiku 同理（价格差 5–15 倍），各自成档。
var modelTiers = map[string]bool{
	"flash": true, "pro": true, "max": true, "plus": true, "mini": true, "lite": true,
	"turbo": true, "air": true, "ultra": true, "nano": true,
	"opus": true, "sonnet": true, "haiku": true,
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)*`)

type modelFamily struct {
	vendor  string
	tier    string
	version []int
}

func familyOf(name string) modelFamily {
	n := strings.ToLower(strings.TrimSpace(name))
	var f modelFamily
	for _, v := range modelVendors {
		if strings.HasPrefix(n, v) {
			f.vendor = v
			break
		}
	}
	if f.vendor == "hy" {
		f.vendor = "hunyuan"
	}
	for _, tok := range strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '/' }) {
		if modelTiers[tok] {
			f.tier = tok
		}
	}
	if m := versionRe.FindString(strings.TrimPrefix(n, f.vendor)); m != "" {
		for _, p := range strings.Split(m, ".") {
			x, _ := strconv.Atoi(p)
			f.version = append(f.version, x)
		}
	}
	return f
}

func versionLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// matchModelFamily 在候选里找与 src 同厂商、同档位的模型：唯一则取之；多个取版本号最高、
// 再取名字最短（deepseek-v4.1-flash 优先于 deepseek-v4.1-flash-sg）。厂商不认识 → 不匹配。
func matchModelFamily(src string, cands []string) (string, bool) {
	sf := familyOf(src)
	if sf.vendor == "" {
		return "", false
	}
	var hits []string
	fams := map[string]modelFamily{}
	for _, c := range cands {
		cf := familyOf(c)
		if cf.vendor == sf.vendor && cf.tier == sf.tier {
			hits = append(hits, c)
			fams[c] = cf
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	sort.SliceStable(hits, func(i, j int) bool {
		vi, vj := fams[hits[i]].version, fams[hits[j]].version
		if versionLess(vi, vj) != versionLess(vj, vi) {
			return versionLess(vj, vi) // 版本高的在前
		}
		if len(hits[i]) != len(hits[j]) {
			return len(hits[i]) < len(hits[j])
		}
		return hits[i] < hits[j]
	})
	return hits[0], true
}
