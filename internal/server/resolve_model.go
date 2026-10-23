package server

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// qoderAutoRealm resolveModelDetail 对 "qoder:" 前缀返回的占位域：具体落在 qoder-cn 还是
// qoder-intl，由 handler 按模型目录决定（只在一区有 → 那一区；两区都有 → 默认区优先、
// 选不到号回退另一区）。它从不作为选号域使用。
const qoderAutoRealm = "qoder"

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为以下之一才剥离，否则视为裸名（realm=cn、bare=原串）：
//
//	cn / global              → WorkBuddy 国内 / 国际
//	qoder-cn / qoder-intl    → Qoder 国内 / 国际（强制该区，永不回退）
//	qoder                    → Qoder 自动选区（返回 qoderAutoRealm，由 handler 决定）
//	kiro                     → Kiro（单一路由域）
//
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 第三个返回值 realmExplicit 供跨域回退（issue #135）判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式前缀永不被回退改写（qoder: 的两区回退由 handler 单独处理）。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	switch prefix {
	case "cn", "global", auth.RealmQoderCN, auth.RealmQoderIntl, qoderAutoRealm, auth.RealmKiro:
	default:
		return "cn", model, false
	}
	bare = model[idx+1:]
	if auth.IsQoderRealm(prefix) || prefix == qoderAutoRealm || auth.IsKiroRealm(prefix) {
		// Qoder / Kiro 模型名对前后空白容忍（"qoder: Qwen3.8-Max"），与 /v1/models 展示 id 对齐。
		bare = strings.TrimSpace(bare)
		if bare == "" {
			// "qoder:" 后面什么都没有：不是有效模型名，按裸名处理（最终由本地报模型不存在）。
			return "cn", model, false
		}
	}
	return prefix, bare, true
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// otherRealm 返回同一平台内的另一个选号域（"cn" ↔ "global"，"qoder-cn" ↔ "qoder-intl"），
// 供跨域回退使用。跨平台永不回退：两个平台的模型目录、积分与账号体系都不相通。
// Kiro 只有一个路由域，返回自身（调用方据此不会做平台内互退）。
func otherRealm(realm string) string {
	switch realm {
	case "global":
		return "cn"
	case auth.RealmQoderCN:
		return auth.RealmQoderIntl
	case auth.RealmQoderIntl:
		return auth.RealmQoderCN
	case auth.RealmKiro:
		return auth.RealmKiro
	default:
		return "global"
	}
}
