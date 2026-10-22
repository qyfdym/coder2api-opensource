package server

// effort.go 请求记录里的「思考强度」：客户端请求的档位 + 这次实际生效的档位。
//
// 两者经常不一样，这正是要记下来的原因：
//   - 模型不支持该档位 → 网关按模型档位表降级（max → high）；
//   - 客户端没指定 → DeepSeek 系由网关补默认档（WorkBuddy），Qoder 由上游按模型默认档运行；
//   - 客户端用了上游不认的写法（如 Responses 风格的 reasoning.effort 打到 WorkBuddy）。

import (
	"encoding/json"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// effortPeek 请求体里与思考档位相关的字段（随 chatCompletions 的 peek 一次解析）。
// 一律 RawMessage：客户端可能给出任意类型，类型不符时不能让整个 peek 解析失败。
type effortPeek struct {
	ReasoningEffort      json.RawMessage `json:"reasoning_effort"`
	ReasoningEffortCamel json.RawMessage `json:"reasoningEffort"`
	Reasoning            json.RawMessage `json:"reasoning"`
	Thinking             json.RawMessage `json:"thinking"`
}

// rawString 取 JSON 字符串值（小写、去空白）；数字 / 布尔按字面量；对象 / 数组 / null 返回空。
func rawString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s[0] == '{' || s[0] == '[' {
		return ""
	}
	if s[0] == '"' {
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		s = v
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// rawField 取 JSON 对象里某个字段的字符串值。
func rawField(raw json.RawMessage, key string) string {
	if s := strings.TrimSpace(string(raw)); s == "" || s[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return rawString(obj[key])
}

// effortInfo 请求侧解析结果。
type effortInfo struct {
	// requested 客户端请求的档位（展示用，任一写法都算）："off" = 显式关闭思考；空 = 没指定。
	requested string
	// wb WorkBuddy 上游实际认的字段（顶层 reasoning_effort / reasoningEffort + thinking.type）。
	wb upstream.EffortRequest
	// qd Qoder 侧取值（reasoning_effort → reasoning.effort → thinking.effort / level）。
	qd string
}

func (p effortPeek) info() effortInfo {
	var ei effortInfo
	top := rawString(p.ReasoningEffort)
	if top == "" {
		top = rawString(p.ReasoningEffortCamel)
	}
	nested := rawField(p.Reasoning, "effort")
	thEffort := rawField(p.Thinking, "effort")
	if thEffort == "" {
		thEffort = rawField(p.Thinking, "level")
	}
	thType := rawField(p.Thinking, "type")

	ei.wb = upstream.EffortRequest{Effort: top, ThinkingType: thType}
	ei.qd = firstNonEmpty(rawString(p.ReasoningEffort), nested, thEffort)
	ei.requested = firstNonEmpty(top, nested, thEffort)
	if ei.requested == "" && (thType == "disabled" || rawString(p.Thinking) == "false") {
		ei.requested = "off"
	}
	if effortOffWords[ei.requested] {
		ei.requested = "off"
	}
	return ei
}

// effortOffWords 关闭思考的各种写法（展示口径统一为 "off"）。
var effortOffWords = map[string]bool{"none": true, "off": true, "disabled": true, "disable": true, "false": true}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// effortOut 这次请求在 realm 上实际生效的档位（选中账号后、按真实服务域计算）。
// 关闭类统一写 "off"（WorkBuddy 用 off、Qoder 用 none，展示口径合一）。
func (h *Handler) effortOut(realm, bare string, ei effortInfo) string {
	var out string
	if auth.IsQoderRealm(realm) {
		if h.cfg.Qoder == nil || h.cfg.Qoder.Client == nil {
			return ""
		}
		out = h.cfg.Qoder.Client.OutboundEffort(qoderacct.Region(realm), bare, ei.qd)
	} else {
		if h.cfg.Upstream == nil {
			return ""
		}
		out = h.cfg.Upstream.OutboundEffort(realm, bare, ei.wb)
	}
	if effortOffWords[out] {
		out = "off"
	}
	return out
}
