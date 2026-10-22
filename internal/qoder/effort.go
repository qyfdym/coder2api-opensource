package qoder

// 思考档位归一化。移植自 qoder2api-hub（MIT）qoder_proxy.py 的
// EFFORT_RANK / supported_efforts / default_effort / normalize_reasoning_effort /
// requested_effort。
//
// 为什么必须归一化：上游对不支持的档位**不报错、直接忽略**（回落模型默认档）。实测
// dfmodel 只支持 low/high/max（默认 max），传 medium/xhigh 时输出与默认档一致——客户端
// 以为「思考档位没生效」；cn 的 qmodel 只有开/关，传任何档位都被忽略（只有 none 能关）。

import (
	"fmt"
	"sort"
	"strings"
)

// effortRank 官方档位词表顺序（none < minimal < low < medium < high < xhigh < max）。
var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

var effortRankOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// effortOff 关闭类取值统一映射为 "none"（官方通用关闭值）。
var effortOff = map[string]bool{
	"none": true, "off": true, "disabled": true, "disable": true, "false": true, "0": true, "no": true,
}

func thinkingEfforts(meta *jval) *jval {
	tc := meta.get("thinking_config")
	if !tc.isObj() {
		return nil
	}
	en := tc.get("enabled")
	if !en.isObj() {
		return nil
	}
	ef := en.get("efforts")
	if !ef.isObj() {
		return nil
	}
	return ef
}

// supportedEfforts 该模型官方支持的思考档位（按 effortRank 顺序）；空 = 只有开/关。
func supportedEfforts(meta *jval) []string {
	ef := thinkingEfforts(meta)
	var out []string
	for _, e := range effortRankOrder {
		if ef.has(e) {
			out = append(out, e)
		}
	}
	return out
}

// defaultEffort is_default 标注项（按目录原始键序找第一个），无则取支持表中间档。
func defaultEffort(meta *jval) string {
	if ef := thinkingEfforts(meta); ef != nil {
		for i, k := range ef.keys {
			if cfg := ef.vals[i]; cfg.isObj() && cfg.get("is_default").truthy() {
				return k
			}
		}
	}
	sup := supportedEfforts(meta)
	if len(sup) == 0 {
		return ""
	}
	return sup[len(sup)/2]
}

// normalizeReasoningEffort 把请求档位归一到该模型官方支持的集合。
//
// 返回 (value, note, send)：send=false 表示不下发 reasoning_effort。规则：
//   - 关闭类取值 → "none"；
//   - 模型没有 thinking_config（如路由器 auto）→ 原样透传，不猜；
//   - 有 thinking_config 但无档位表（仅开/关）→ 除 none 外不下发；
//   - 命中档位表 → 原样；未知词 → 默认档（无则首档）；
//   - 其余取「最近的合法档位」，同距时偏向模型默认档（稳定排序，与原实现 sorted 一致）。
func normalizeReasoningEffort(effort string, meta *jval) (string, string, bool) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return "", "", false
	}
	if effortOff[e] {
		return "none", "", true
	}
	if !meta.get("thinking_config").isObj() {
		return e, "", true
	}
	sup := supportedEfforts(meta)
	if len(sup) == 0 {
		return "", "dropped (model has no effort levels; use none to disable)", false
	}
	for _, s := range sup {
		if s == e {
			return e, "", true
		}
	}
	want, known := effortRank[e]
	if !known {
		d := defaultEffort(meta)
		if d == "" {
			d = sup[0]
		}
		return d, "unknown level -> " + d, true
	}
	d := defaultEffort(meta)
	dRank, ok := effortRank[d]
	if !ok {
		dRank = want
	}
	cand := append([]string(nil), sup...)
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	sort.SliceStable(cand, func(i, j int) bool {
		ai, aj := abs(effortRank[cand[i]]-want), abs(effortRank[cand[j]]-want)
		if ai != aj {
			return ai < aj
		}
		return abs(effortRank[cand[i]]-dRank) < abs(effortRank[cand[j]]-dRank)
	})
	best := cand[0]
	return best, fmt.Sprintf("unsupported level %s -> %s", e, best), true
}

// requestedEffort 请求侧原始档位：reasoning_effort → reasoning.effort →
// thinking.effort / thinking.level（与原实现同源，不做归一化）。
func requestedEffort(payload *jval) string {
	if !payload.isObj() {
		return ""
	}
	pick := func(v *jval) string {
		if v == nil || !v.truthy() {
			return ""
		}
		if v.k == jStr {
			return v.s
		}
		return v.dumpPy()
	}
	if s := pick(payload.get("reasoning_effort")); s != "" {
		return s
	}
	if r := payload.get("reasoning"); r.isObj() {
		if s := pick(r.get("effort")); s != "" {
			return s
		}
	}
	if t := payload.get("thinking"); t.isObj() {
		if s := pick(t.get("effort")); s != "" {
			return s
		}
		if s := pick(t.get("level")); s != "" {
			return s
		}
	}
	return ""
}

// OutboundEffort 预测这次请求在上游实际生效的思考档位（与 buildQoderBody 同一套归一规则，
// 只读模型目录缓存 / 快照，不打网络）。供网关请求记录展示「请求的档位 → 实际档位」。
//
// requested 为请求侧原始档位（同 requestedEffort 的取值口径）：
//   - 非空 → 归一后的档位；"none" 表示关闭思考；被丢弃（模型只有开/关）返回空串；
//   - 空 → 客户端没指定，上游按模型默认档运行：返回目录里的默认档（目录也没有则空串）。
func (c *Client) OutboundEffort(realm, model, requested string) string {
	r := NormalizeRealm(realm)
	meta := c.catalogMetaFor(r, c.resolveUpstreamKey(model, r))
	if strings.TrimSpace(requested) == "" {
		return defaultEffort(meta)
	}
	norm, _, send := normalizeReasoningEffort(requested, meta)
	if !send {
		return ""
	}
	return norm
}
