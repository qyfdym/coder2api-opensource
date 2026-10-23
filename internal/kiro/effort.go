package kiro

// 思考强度（reasoning_effort）→ Kiro 思考开关与预算。
//
// Kiro 没有结构化的 thinking 参数：思考靠系统提示词里的
// <thinking_mode>enabled</thinking_mode><max_thinking_length>N</max_thinking_length> 打开，
// 上游随后以 reasoningContentEvent（或正文里的 <thinking> 块）下发思考内容。
// 档位因此映射成 max_thinking_length 预算；客户端没要求思考时不开（Kiro 默认就是不思考）。
// 例外：目录里声明了原生 reasoning.effort 档位的模型（gpt-5.6-*、kimi-k3 等）走请求体字段，见下文「原生档位」。
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

// OutboundEffort 这次请求实际生效的思考档位（"off" = 不思考；空 = 原生档位模型且客户端没指定，
// 不带档位、上游按模型默认处理）。供请求记录的「思考强度」列。
func (c *Client) OutboundEffort(model string, r EffortRequest) string {
	return c.planEffort(model, r).label
}

// ---------------------------------------------------------------------------
// 原生档位
// ---------------------------------------------------------------------------
//
// 目录（ListAvailableModels）里有些模型带 additionalModelRequestFieldsSchema，声明了
// reasoning.effort 的可选档位（真机：gpt-5.6-* 是 none/low/medium/high/xhigh/max，kimi-k3 是 low/high/max）。
// 这类模型的思考由请求体顶层的 additionalModelRequestFields: {"reasoning":{"effort":X}} 控制，
// 提示词里的 <thinking_mode> 标签对它们不起作用；档位不在 enum 里上游会返回 400，
// 所以发送前先落到模型支持的档位上。没有 schema 的模型照旧走提示词开关。

// effortRank 档位强弱（用于把请求档位落到模型支持的档位上）。
var effortRank = map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// effortPlan 一次请求的思考决策。
type effortPlan struct {
	kiroModel string
	// budget 提示词方式的思考预算（0 = 不注入）；原生档位模型恒为 0。
	budget int
	// native 该模型走原生 reasoning.effort；nativeEffort 为空 = 不带这个字段（客户端没指定）。
	native       bool
	nativeEffort string
	// label 展示用的实际档位（"off" / low…max；原生且没指定时为空）。
	label string
}

// thinking 回包里是否保留思考内容。
func (p effortPlan) thinking() bool {
	if p.native {
		return p.nativeEffort != "" && p.nativeEffort != "none"
	}
	return p.budget > 0
}

// planEffort 决定这次请求的模型 id、思考方式与档位。
func (c *Client) planEffort(model string, r EffortRequest) effortPlan {
	kiroModel, budget, label := c.resolveThinking(model, r)
	plan := effortPlan{kiroModel: kiroModel, budget: budget, label: label}
	m, ok := c.LookupModel(kiroModel)
	if !ok || len(m.Efforts) == 0 {
		return plan
	}
	plan.native, plan.budget, plan.label = true, 0, ""
	eff := strings.ToLower(strings.TrimSpace(r.Effort))
	explicitOff := effortOffWords[eff] || strings.EqualFold(strings.TrimSpace(r.ThinkingType), "disabled")
	if label == "off" && !explicitOff {
		return plan // 没指定：不带字段，上游按模型默认档运行
	}
	want := label
	if explicitOff {
		want = "none"
	}
	plan.nativeEffort = pickEffort(m.Efforts, want)
	plan.label = plan.nativeEffort
	if plan.label == "none" {
		plan.label = "off"
	}
	return plan
}

// pickEffort 把想要的档位落到模型支持的档位上：取不高于它的最高档（不花超出请求的思考量）；
// 支持的档位都比它高时取最低档（例如 kimi-k3 没有 none，「关闭」落到 low）。
func pickEffort(supported []string, want string) string {
	w := effortRank[want]
	best, bestRank := "", -1
	lowest, lowestRank := "", 1<<30
	for _, s := range supported {
		r, ok := effortRank[s]
		if !ok {
			continue
		}
		if r <= w && r > bestRank {
			best, bestRank = s, r
		}
		if r < lowestRank {
			lowest, lowestRank = s, r
		}
	}
	if best != "" {
		return best
	}
	return lowest
}

// nativeEffortSchema 解析目录里的 additionalModelRequestFieldsSchema，取出 reasoning.effort 的
// 可选档位与默认档（只认已知档位名；没有这个字段返回 nil）。
func nativeEffortSchema(raw json.RawMessage) (efforts []string, def string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	var s struct {
		Properties struct {
			Reasoning struct {
				Properties struct {
					Effort struct {
						Enum    []string `json:"enum"`
						Default string   `json:"default"`
					} `json:"effort"`
				} `json:"properties"`
			} `json:"reasoning"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil, ""
	}
	seen := map[string]bool{}
	for _, e := range s.Properties.Reasoning.Properties.Effort.Enum {
		e = strings.ToLower(strings.TrimSpace(e))
		if _, known := effortRank[e]; known && !seen[e] {
			seen[e] = true
			efforts = append(efforts, e)
		}
	}
	if d := strings.ToLower(strings.TrimSpace(s.Properties.Reasoning.Properties.Effort.Default)); seen[d] {
		def = d
	}
	return efforts, def
}

// EffortInfo 模型对外公布的思考能力：支持的档位、默认档、能否关闭。
// 原生档位模型取目录里的声明；其余模型是提示词开关（默认关闭，五个档位）。
func (m Model) EffortInfo() (supported []string, def string, canDisable bool) {
	if len(m.Efforts) == 0 {
		return SupportedEfforts, "off", true
	}
	for _, e := range m.Efforts {
		if e == "none" {
			canDisable = true
			continue
		}
		supported = append(supported, e)
	}
	def = m.DefaultEffort
	if def == "none" {
		def = "off"
	}
	return supported, def, canDisable
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
