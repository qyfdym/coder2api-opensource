package kiro

// 思考强度（reasoning_effort）→ Kiro 思考开关与预算。
//
// Kiro 没有结构化的 thinking 参数：思考靠系统提示词里的
// <thinking_mode>enabled</thinking_mode><max_thinking_length>N</max_thinking_length> 打开，
// 上游随后以 reasoningContentEvent（或正文里的 <thinking> 块）下发思考内容。
// 档位因此映射成 max_thinking_length 预算；客户端没要求思考时不开（Kiro 默认就是不思考）。
//
// 开启思考的写法（任一即可，显式关闭优先）：
//   - reasoning_effort / reasoningEffort / reasoning.effort / thinking.effort：low … max；
//   - thinking: {type: enabled|adaptive, budget_tokens: N}（Anthropic 写法）；
//   - 模型名带 -thinking 后缀（Kiro-Go 写法，按 max 档）。

import (
	"encoding/json"
	"strconv"
	"strings"
)

// EffortRequest 请求里与思考相关的字段（已小写）。
type EffortRequest struct {
	Effort       string // reasoning_effort 等档位写法
	ThinkingType string // thinking.type
	BudgetTokens int    // thinking.budget_tokens
}

// SupportedEfforts Kiro 模型对外公布的档位（/v1/models 的 reasoning_supported_efforts）。
var SupportedEfforts = []string{"low", "medium", "high", "xhigh", "max"}

var effortBudgets = map[string]int{
	"low":    4096,
	"medium": 16384,
	"high":   32768,
	"xhigh":  65536,
	"max":    200000,
}

const (
	minThinkingBudget = 1024
	maxThinkingBudget = 200000
	// defaultOnEffort 只说「要思考」没给档位（-thinking 后缀 / thinking.type=enabled 不带预算）
	// 时的档位：与 Kiro-Go 的固定提示词一致（max_thinking_length=200000）。
	defaultOnEffort = "max"
)

var effortOffWords = map[string]bool{"none": true, "off": true, "disabled": true, "disable": true, "false": true}

// effortAliases 其它平台的档位写法归一到 Kiro 的档位表。
var effortAliases = map[string]string{"minimal": "low", "min": "low", "mid": "medium", "extra_high": "xhigh", "maximum": "max"}

// resolveThinking 决定这次请求的 Kiro 模型 id、思考预算（0 = 不思考）与展示档位（"off" / low…max）。
func (c *Client) resolveThinking(model string, r EffortRequest) (kiroModel string, budget int, label string) {
	kiroModel, suffix := ParseModel(model, c.opts.ThinkingSuffix)
	eff := strings.ToLower(strings.TrimSpace(r.Effort))
	if a, ok := effortAliases[eff]; ok {
		eff = a
	}
	tt := strings.ToLower(strings.TrimSpace(r.ThinkingType))
	switch {
	case effortOffWords[eff] || tt == "disabled":
		return kiroModel, 0, "off"
	case effortBudgets[eff] > 0:
		return kiroModel, effortBudgets[eff], eff
	case r.BudgetTokens > 0:
		b := r.BudgetTokens
		if b < minThinkingBudget {
			b = minThinkingBudget
		}
		if b > maxThinkingBudget {
			b = maxThinkingBudget
		}
		return kiroModel, b, labelForBudget(b)
	case eff != "":
		// 认不出的档位写法（auto 等）：按 high 开思考，不悄悄丢掉客户端的思考意图。
		return kiroModel, effortBudgets["high"], "high"
	case tt == "enabled" || tt == "adaptive" || suffix:
		return kiroModel, effortBudgets[defaultOnEffort], defaultOnEffort
	}
	return kiroModel, 0, "off"
}

func labelForBudget(b int) string {
	for _, e := range SupportedEfforts {
		if b <= effortBudgets[e] {
			return e
		}
	}
	return "max"
}

// OutboundEffort 这次请求实际生效的思考档位（"off" = 不思考）。供请求记录的「思考强度」列。
func (c *Client) OutboundEffort(model string, r EffortRequest) string {
	_, _, label := c.resolveThinking(model, r)
	return label
}

// effortFromBody 从 OpenAI 请求体里取思考相关字段（字段类型不对时忽略，不让整个请求失败）。
func effortFromBody(body []byte) EffortRequest {
	var peek struct {
		ReasoningEffort      json.RawMessage `json:"reasoning_effort"`
		ReasoningEffortCamel json.RawMessage `json:"reasoningEffort"`
		Reasoning            json.RawMessage `json:"reasoning"`
		Thinking             json.RawMessage `json:"thinking"`
	}
	_ = json.Unmarshal(body, &peek)
	r := EffortRequest{
		Effort: firstNonEmptyStr(rawStr(peek.ReasoningEffort), rawStr(peek.ReasoningEffortCamel),
			rawObjStr(peek.Reasoning, "effort"), rawObjStr(peek.Thinking, "effort"), rawObjStr(peek.Thinking, "level")),
		ThinkingType: rawObjStr(peek.Thinking, "type"),
	}
	if rawStr(peek.Thinking) == "false" {
		r.ThinkingType = "disabled"
	}
	if s := rawObjStr(peek.Thinking, "budget_tokens"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			r.BudgetTokens = n
		}
	}
	return r
}

func rawStr(raw json.RawMessage) string {
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

func rawObjStr(raw json.RawMessage, key string) string {
	if s := strings.TrimSpace(string(raw)); s == "" || s[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return rawStr(obj[key])
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
