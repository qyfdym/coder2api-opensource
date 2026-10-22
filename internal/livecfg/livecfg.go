// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string              // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration       // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool                // 出站请求体指纹脱敏
	RecordClientInfo     bool                // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	// ModelFallback 跨平台回退的显式模型映射（键已小写归一，值原样；空值 = 禁止回退）；nil = 只用自动匹配。
	ModelFallback map[string]string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为「<路由域>:<模型>」精确键集合。
//
//   - "cn:X" / "global:X" / "qoder-cn:X" / "qoder-intl:X" → 只隐藏该域；
//   - "qoder:X" → Qoder 两区（qoder-cn / qoder-intl）都隐藏；
//   - 裸名 "X" → WorkBuddy 两域（cn / global）都隐藏（不波及 Qoder，两平台模型名互不相干）。
//
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":"，前段恰为
// 已知前缀才剥离；未知前缀整串按裸名处理）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		prefix, bare, ok := strings.Cut(m, ":")
		if ok && bare != "" {
			switch prefix {
			case "cn", "global", "qoder-cn", "qoder-intl":
				out[prefix+":"+bare] = struct{}{}
				continue
			case "qoder":
				out["qoder-cn:"+bare] = struct{}{}
				out["qoder-intl:"+bare] = struct{}{}
				continue
			}
		}
		out["cn:"+m] = struct{}{}
		out["global:"+m] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeModelFallback 归一化 models.fallback：键去空白并转小写（查表大小写不敏感），
// 值去空白后原样保留（目标模型名大小写对上游有意义）。空键丢弃；空值保留（= 禁止回退）。
// 返回的 map 只读共享，调用方不得修改。
func NormalizeModelFallback(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
