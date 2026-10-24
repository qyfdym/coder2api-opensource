package kiro

// 思考强度（reasoning_effort）→ Kiro 思考开关与预算。
//
// 没有原生档位的模型（claude-4.5 系、deepseek、glm、qwen 等）：思考靠系统提示词里的
// <thinking_mode>enabled</thinking_mode><max_thinking_length>N</max_thinking_length> 打开，
// 上游随后以 reasoningContentEvent（或正文里的 <thinking> 块）下发思考内容。
// 档位因此映射成 max_thinking_length 预算；客户端没要求思考时不开（这些模型默认不思考）。
// 目录里声明了原生档位的模型走请求体字段，提示词标签对它们不起作用，见下文「原生档位」。
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
// 目录（ListAvailableModels）里有些模型带 additionalModelRequestFieldsSchema，声明了原生思考参数。
// 真机（2026-10）有两种写法：
//   - reasoning.effort：gpt-5.6-*（none/low/medium/high/xhigh/max）、kimi-k3（low/high/max）；
//     发 additionalModelRequestFields: {"reasoning":{"effort":X}}。
//   - thinking + output_config.effort（Anthropic adaptive thinking）：claude-opus/sonnet-4.6 起的全部 Claude
//     （4.6 系没有 xhigh）；发 {"thinking":{"type":"adaptive"},"output_config":{"effort":X}}。
//     thinking.type 的 enum 因模型而异：多数是 adaptive / disabled，claude-opus-5.5 只有 adaptive（关不掉），
//     claude-sonnet-5.5 是 adaptive / between_tools；enabled + budget_tokens 一律 400。
//     这些模型不带字段时按目录默认档自己思考（opus-5.5 默认 medium），提示词标签被当成普通文本忽略——
//     所以客户端调档位必须走字段，否则 low 到 max 的效果完全一样。
// 档位不在 enum 里上游会返回 400，所以发送前先落到模型支持的档位上。没有 schema 的模型照旧走提示词开关。

// 原生档位写在请求体的哪个字段（Model.EffortField）。
const (
	effortFieldReasoning    = "reasoning"
	effortFieldOutputConfig = "output_config"
)

// effortRank 档位强弱（用于把请求档位落到模型支持的档位上）。
var effortRank = map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// effortPlan 一次请求的思考决策。
type effortPlan struct {
	kiroModel string
	// budget 提示词方式的思考预算（0 = 不注入）；原生档位模型恒为 0。
	budget int
	// native 该模型走原生档位字段（field 是哪一种写法）；nativeEffort 为空 = 不带档位（客户端没指定）。
	native       bool
	field        string
	nativeEffort string
	// disable output_config 写法下显式关闭思考（发 thinking.type=disabled，模型支持时才会置位）。
	disable bool
	// label 展示用的实际档位（"off" / low…max；原生且没指定时为空）。
	label string
}

// thinking 回包里是否保留思考内容。
func (p effortPlan) thinking() bool {
	if p.native {
		return !p.disable && p.nativeEffort != "" && p.nativeEffort != "none"
	}
	return p.budget > 0
}

// requestFields 原生档位模型的 additionalModelRequestFields；nil = 不带（提示词开关模型，或客户端没指定）。
func (p effortPlan) requestFields() map[string]any {
	if !p.native {
		return nil
	}
	if p.field == effortFieldOutputConfig {
		if p.disable {
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		}
		if p.nativeEffort == "" {
			return nil
		}
		return map[string]any{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": p.nativeEffort},
		}
	}
	if p.nativeEffort == "" {
		return nil
	}
	return map[string]any{"reasoning": map[string]any{"effort": p.nativeEffort}}
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
	plan.field = m.EffortField
	if plan.field == "" {
		plan.field = effortFieldReasoning
	}
	eff := strings.ToLower(strings.TrimSpace(r.Effort))
	explicitOff := effortOffWords[eff] || strings.EqualFold(strings.TrimSpace(r.ThinkingType), "disabled")
	if label == "off" && !explicitOff {
		return plan // 没指定：不带字段，上游按模型默认档运行
	}
	if explicitOff && plan.field == effortFieldOutputConfig && containsStr(m.ThinkingTypes, "disabled") {
		plan.disable, plan.label = true, "off"
		return plan
	}
	want := label
	if explicitOff {
		want = "none" // 关不掉的模型（opus-5.5 只有 adaptive）由 pickEffort 落到最低档
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

// effortSchema 目录里 additionalModelRequestFieldsSchema 声明的原生思考参数。
type effortSchema struct {
	efforts       []string // 可选档位（只认已知档位名，按目录顺序）
	def           string   // 默认档（必须在 efforts 里，否则为空）
	field         string   // effortFieldReasoning / effortFieldOutputConfig；空 = 没有原生档位
	thinkingTypes []string // output_config 写法下 thinking.type 的 enum
}

type schemaEnum struct {
	Enum    []string `json:"enum"`
	Default string   `json:"default"`
}

// parseEffortSchema 解析 additionalModelRequestFieldsSchema：reasoning.effort（gpt / kimi）优先，
// 其次 output_config.effort（Claude adaptive thinking）。都没有返回零值。
func parseEffortSchema(raw json.RawMessage) effortSchema {
	if len(raw) == 0 || string(raw) == "null" {
		return effortSchema{}
	}
	var s struct {
		Properties struct {
			Reasoning struct {
				Properties struct {
					Effort schemaEnum `json:"effort"`
				} `json:"properties"`
			} `json:"reasoning"`
			OutputConfig struct {
				Properties struct {
					Effort schemaEnum `json:"effort"`
				} `json:"properties"`
			} `json:"output_config"`
			Thinking struct {
				Properties struct {
					Type schemaEnum `json:"type"`
				} `json:"properties"`
			} `json:"thinking"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return effortSchema{}
	}
	if es := knownEfforts(s.Properties.Reasoning.Properties.Effort); len(es.efforts) > 0 {
		es.field = effortFieldReasoning
		return es
	}
	es := knownEfforts(s.Properties.OutputConfig.Properties.Effort)
	if len(es.efforts) == 0 {
		return effortSchema{}
	}
	es.field = effortFieldOutputConfig
	for _, t := range s.Properties.Thinking.Properties.Type.Enum {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			es.thinkingTypes = append(es.thinkingTypes, t)
		}
	}
	return es
}

func knownEfforts(e schemaEnum) effortSchema {
	var out effortSchema
	seen := map[string]bool{}
	for _, v := range e.Enum {
		v = strings.ToLower(strings.TrimSpace(v))
		if _, known := effortRank[v]; known && !seen[v] {
			seen[v] = true
			out.efforts = append(out.efforts, v)
		}
	}
	if d := strings.ToLower(strings.TrimSpace(e.Default)); seen[d] {
		out.def = d
	}
	return out
}

// nativeEffortSchema parseEffortSchema 的档位部分（可选档位与默认档）。
func nativeEffortSchema(raw json.RawMessage) (efforts []string, def string) {
	es := parseEffortSchema(raw)
	return es.efforts, es.def
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// EffortInfo 模型对外公布的思考能力：支持的档位、默认档、能否关闭。
// 原生档位模型取目录里的声明；其余模型是提示词开关（默认关闭，五个档位）。
func (m Model) EffortInfo() (supported []string, def string, canDisable bool) {
	if len(m.Efforts) == 0 {
		return SupportedEfforts, "off", true
	}
	if m.EffortField == effortFieldOutputConfig && containsStr(m.ThinkingTypes, "disabled") {
		canDisable = true
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
