package qoder

// 上游请求体构造。移植自 qoder2api-hub（MIT）qoder_proxy.py 的 build_qoder_body /
// flatten_messages / normalize_roles / sanitize_* / backfill_reasoning_content /
// is_deepseek_model / 历史工具结果截短 / 结构化工具历史白名单。
//
// 以官方 baseprompt.json 为骨架：每次覆写 request/session id、时间戳、模型配置、
// 系统提示词与会话、工具、参数与 business 会话名。凡依赖原实现环境变量的开关一律取
// 默认值硬编码（见下列常量）。

import (
	_ "embed"
	"regexp"
	"strings"
)

//go:embed assets/baseprompt.json
var basepromptJSON []byte

const (
	defaultSystemPrompt = "You are a helpful assistant."

	// 历史工具结果截短（_tool_result_keep_chars / _tool_result_scope 默认值）。
	toolResultKeepDefault  = 2000 // 单侧保留字符数
	toolResultShrinkMargin = 200  // 削后至少省这么多字符才值得加省略标记
	toolResultScopeTailN   = 1    // scope=tail 时保护最后 N 条工具结果

	// 出站体积上限（MAX_UPSTREAM_BODY_BYTES，QD_MAX_BODY_MB 默认 29）。
	maxUpstreamBodyBytes = 29 * 1024 * 1024

	// 省略标记（_shrink_tool_result；固定串，保证前缀缓存字节稳定）。
	toolResultShrinkNotice = "\n[[qoder-proxy: 已省略中间内容；需要完整内容请重新执行该命令]]\n"
)

// 结构化工具历史白名单（STRUCTURED_TOOL_HISTORY_WHITELIST）。
var structuredToolHistoryWhitelist = []string{"qwen", "glm", "gm4", "gm5", "deepseek", "dfmodel"}

// deepseekModelKeys DeepSeek 上游 key。
var deepseekModelKeys = map[string]bool{"dmodel": true, "dfmodel": true}

var baseprompt = func() *jval {
	v, err := parseJ(basepromptJSON)
	if err != nil || !v.isObj() {
		panic("qoder: bad embedded baseprompt.json")
	}
	return v
}()

// ---- 脱敏（sanitize_*） ----

var (
	sanitizeHdrRe     = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\r\n]*;?\s*`)
	sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
	sanitizeKVRe      = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\r\n]*;?\s*`)
)

var sanitizeFeatures = []string{
	"x-anthropic-billing-header", "cc_entrypoint=",
	"You are Claude Code, Anthropic's official CLI for Claude",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
}

var sanitizeRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
}

func hasFingerprint(text string) bool {
	if text == "" {
		return false
	}
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

func sanitizeText(text string) string {
	if text == "" || !hasFingerprint(text) {
		return text
	}
	for _, r := range sanitizeRewrites {
		text = strings.ReplaceAll(text, r[0], r[1])
	}
	text = sanitizeHdrRe.ReplaceAllString(text, "")
	if strings.Contains(text, "cc_") {
		for {
			next := sanitizeKVRe.ReplaceAllString(text, "")
			if next == text {
				break
			}
			text = next
		}
	}
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return stripWS(text)
}

// ---- 角色与 DeepSeek ----

func normalizeRoles(messages *jval) *jval {
	out := jArrV()
	if !messages.isArr() {
		return out
	}
	for _, m := range messages.arr {
		if !m.isObj() {
			out.arr = append(out.arr, m)
			continue
		}
		if m.get("role").str() == "developer" {
			m2 := m.shallow()
			m2.set("role", jStrV("system"))
			out.arr = append(out.arr, m2)
		} else {
			out.arr = append(out.arr, m)
		}
	}
	return out
}

func isDeepseekModel(model, modelKey string) bool {
	if deepseekModelKeys[strings.ToLower(strings.TrimSpace(modelKey))] {
		return true
	}
	low := strings.ToLower(strings.TrimSpace(model))
	if low == "" {
		return false
	}
	if deepseekModelKeys[low] || strings.HasPrefix(low, "deepseek") {
		return true
	}
	return strings.Contains(low, "deepseek")
}

func backfillReasoningContent(messages *jval, model, modelKey string) *jval {
	if !isDeepseekModel(model, modelKey) {
		return messages
	}
	hasTrace := false
	for _, m := range messages.arr {
		if m.isObj() && (m.get("reasoning").truthy() || m.has("reasoning_content")) {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return messages
	}
	out := jArrV()
	for _, m := range messages.arr {
		if m.isObj() && m.get("role").str() == "assistant" {
			m2 := m.shallow()
			if !m2.has("reasoning_content") {
				if m2.get("reasoning").truthy() {
					m2.set("reasoning_content", jStrV(jToStr(m2.get("reasoning"))))
				} else {
					m2.set("reasoning_content", jStrV(""))
				}
			}
			out.arr = append(out.arr, m2)
		} else {
			out.arr = append(out.arr, m)
		}
	}
	return out
}

func sanitizeMessages(messages *jval) *jval {
	out := jArrV()
	for _, m := range messages.arr {
		if !m.isObj() {
			out.arr = append(out.arr, m)
			continue
		}
		item := m.shallow()
		if item.has("content") {
			item.set("content", sanitizeContent(item.get("content")))
		}
		if rc := item.get("reasoning_content"); rc.isStr() {
			item.set("reasoning_content", jStrV(sanitizeText(rc.s)))
		}
		if item.has("tool_calls") {
			item.set("tool_calls", sanitizeToolCalls(item.get("tool_calls")))
		}
		out.arr = append(out.arr, item)
	}
	return out
}

func sanitizeContent(content *jval) *jval {
	if content.isStr() {
		return jStrV(sanitizeText(content.s))
	}
	if content.isArr() {
		out := jArrV()
		for _, part := range content.arr {
			if part.isObj() && part.get("type").str() == "text" && part.has("text") {
				p := part.shallow()
				p.set("text", jStrV(sanitizeText(p.get("text").str())))
				out.arr = append(out.arr, p)
			} else {
				out.arr = append(out.arr, part)
			}
		}
		return out
	}
	return content
}

func sanitizeToolCalls(tcs *jval) *jval {
	if !tcs.isArr() {
		return tcs
	}
	out := jArrV()
	for _, tc := range tcs.arr {
		if tc.isObj() {
			item := tc.shallow()
			if fn := item.get("function"); fn.isObj() && fn.get("arguments").isStr() {
				fn2 := fn.shallow()
				fn2.set("arguments", jStrV(sanitizeText(fn.get("arguments").s)))
				item.set("function", fn2)
			}
			out.arr = append(out.arr, item)
		} else {
			out.arr = append(out.arr, tc)
		}
	}
	return out
}

// ---- 内容压平（_flatten_content_text / _flatten_content） ----

// flattenContentText 把 content 压成纯文本，另收集图片 URL（_flatten_content_text）。
func flattenContentText(content *jval) (string, []string) {
	if content == nil || content.isNull() {
		return "", nil
	}
	if content.isStr() {
		return content.s, nil
	}
	var list *jval
	if content.isObj() {
		list = jArrV(content)
	} else if content.isArr() {
		list = content
	} else {
		return jToStr(content), nil
	}
	var texts []string
	var images []string
	for _, piece := range list.arr {
		if piece.isStr() {
			texts = append(texts, piece.s)
			continue
		}
		if !piece.isObj() {
			continue
		}
		ptype := piece.get("type").str()
		switch ptype {
		case "text", "input_text", "output_text", "summary_text":
			texts = append(texts, piece.get("text").str())
		default:
			if ptype == "image_url" || ptype == "image" || ptype == "input_image" || piece.has("image_url") {
				url := imageURLFromPart(piece)
				if url != "" {
					images = append(images, url)
				}
			}
		}
	}
	var nonEmpty []string
	for _, t := range texts {
		if t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	return strings.Join(nonEmpty, "\n"), images
}

func imageURLFromPart(piece *jval) string {
	u := piece.get("image_url")
	if u == nil || u.isNull() {
		u = piece.get("url")
	}
	url := ""
	if u.isObj() {
		url = u.get("url").str()
	} else if u.isStr() {
		url = u.s
	}
	if url == "" && piece.get("data").truthy() {
		mime := piece.get("mimeType").str()
		if mime == "" {
			mime = piece.get("mime_type").str()
		}
		if mime == "" {
			mime = "image/png"
		}
		url = "data:" + mime + ";base64," + piece.get("data").str()
	}
	return url
}

// flattenContentParts 带图消息保留的结构化 parts（_flatten_content）：有图片时返回
// parts 数组，否则返回纯文本字符串。
func flattenContentParts(content *jval) *jval {
	if content == nil || content.isNull() {
		return jStrV("")
	}
	if content.isStr() {
		return content
	}
	var list *jval
	if content.isObj() {
		list = jArrV(content)
	} else if content.isArr() {
		list = content
	} else {
		return jStrV(jToStr(content))
	}
	var texts []string
	parts := jArrV()
	hasImg := false
	for _, piece := range list.arr {
		if piece.isStr() {
			texts = append(texts, piece.s)
			parts.arr = append(parts.arr, jObjV("type", "text", "text", piece.s))
			continue
		}
		if !piece.isObj() {
			continue
		}
		ptype := piece.get("type").str()
		switch ptype {
		case "input_text", "output_text", "text", "summary_text":
			t := piece.get("text").str()
			texts = append(texts, t)
			parts.arr = append(parts.arr, jObjV("type", "text", "text", t))
		default:
			if ptype == "input_image" || ptype == "image_url" || ptype == "image" || piece.has("image_url") {
				url := imageURLFromPart(piece)
				if url != "" {
					parts.arr = append(parts.arr, jObjV("type", "image_url",
						"image_url", jObjV("url", url)))
					hasImg = true
				}
			}
		}
	}
	if hasImg {
		return parts
	}
	var nonEmpty []string
	for _, t := range texts {
		if t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	return jStrV(strings.Join(nonEmpty, "\n"))
}

// ---- 历史工具结果截短 ----

func isCompleteJSON(text string) bool {
	s := stripWS(text)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	return pyJSONValid(s)
}

func shrinkToolResult(text string, keep int) string {
	if keep <= 0 {
		return text
	}
	if len(text) <= keep*2+toolResultShrinkMargin {
		return text
	}
	if isCompleteJSON(text) && len(text) <= keep*4 {
		return text
	}
	head := text[:keep]
	if nl := strings.LastIndexByte(head, '\n'); nl > keep/2 {
		head = head[:nl]
	}
	tail := text[len(text)-keep:]
	if nl := strings.IndexByte(tail, '\n'); nl != -1 && nl < keep/2 {
		tail = tail[nl+1:]
	}
	return head + toolResultShrinkNotice + tail
}

// currentTurnStart 当前轮起始下标（仅 scope=turn 用；我们默认 tail，故此函数保留但未走）。
func currentTurnStart(messages *jval) int {
	for i := len(messages.arr) - 1; i >= 0; i-- {
		m := messages.arr[i]
		if m.isObj() {
			switch m.get("role").str() {
			case "user", "system", "developer":
				return i + 1
			}
		}
	}
	return 0
}

// ---- 结构化工具历史 ----

func toolIDsOK(messages *jval) bool {
	for _, m := range messages.arr {
		if !m.isObj() {
			continue
		}
		role := m.get("role").str()
		if role == "tool" {
			if strings.TrimSpace(m.get("tool_call_id").str()) == "" {
				return false
			}
		} else if role == "assistant" && m.get("tool_calls").truthy() {
			for _, tc := range m.get("tool_calls").arr {
				if !tc.isObj() || strings.TrimSpace(tc.get("id").str()) == "" {
					return false
				}
			}
		}
	}
	return true
}

// structuredToolHistoryEnabled 默认 auto：CN 出口 + provider 白名单 + id 齐备。
func structuredToolHistoryEnabled(model, modelKey, realm string, messages *jval) bool {
	if !toolIDsOK(messages) {
		return false
	}
	if NormalizeRealm(realm) != RealmCN {
		return false
	}
	for _, cand := range []string{strings.ToLower(strings.TrimSpace(modelKey)),
		strings.ToLower(strings.TrimSpace(model))} {
		if cand == "" {
			continue
		}
		for _, w := range structuredToolHistoryWhitelist {
			if strings.HasPrefix(cand, w) {
				return true
			}
		}
	}
	return false
}

// ---- flatten_messages ----

// flattenMessages 把客户端会话压平成上游可接受的 {role,content} 序列
// 返回 (systemText, hasSystem, flat, images)。
func flattenMessages(messages *jval, keepReasoning, structured bool) (string, bool, *jval, []string) {
	var systemText string
	hasSystem := false
	flat := jArrV()
	var images []string

	keep := toolResultKeepDefault
	// scope=tail（默认）：只保护最后 N 条工具结果，更早的一律削。
	protect := map[int]bool{}
	if keep > 0 && toolResultScopeTailN > 0 {
		var toolIdx []int
		for i, m := range messages.arr {
			if m.isObj() && m.get("role").str() == "tool" {
				toolIdx = append(toolIdx, i)
			}
		}
		start := len(toolIdx) - toolResultScopeTailN
		if start < 0 {
			start = 0
		}
		for _, i := range toolIdx[start:] {
			protect[i] = true
		}
	}

	for idx, m := range messages.arr {
		if !m.isObj() {
			continue
		}
		role := m.get("role").str()
		text, imgs := flattenContentText(m.get("content"))
		images = append(images, imgs...)
		text = sanitizeText(text)

		switch role {
		case "system", "developer":
			if !hasSystem && text != "" {
				systemText = text
				hasSystem = true
			}
			continue
		case "tool":
			name := ""
			if tc := m.get("name").str(); tc != "" {
				name = " (" + tc + ")"
			}
			body := text
			if keep > 0 && !protect[idx] {
				body = shrinkToolResult(body, keep)
			}
			if structured {
				flat.arr = append(flat.arr, jObjV("role", "tool",
					"tool_call_id", m.get("tool_call_id").str(), "content", body))
				continue
			}
			flat.arr = append(flat.arr, jObjV("role", "user",
				"content", toolResultMarker+name+"]\n"+body))
			continue
		}

		if role == "assistant" && m.get("tool_calls").truthy() {
			if structured {
				calls := jArrV()
				for _, tc := range m.get("tool_calls").arr {
					if !tc.isObj() {
						tc = jObjV()
					}
					fn := tc.get("function")
					if !fn.isObj() {
						fn = jObjV()
					}
					args := fn.get("arguments")
					var argStr string
					switch {
					case args.isObj() || args.isArr():
						argStr = args.dumpPy()
					case args.isStr():
						argStr = args.s
					case args == nil || args.isNull():
						argStr = ""
					default:
						argStr = jToStr(args)
					}
					calls.arr = append(calls.arr, jObjV("id", tc.get("id").str(),
						"type", nonEmpty(tc.get("type").str(), "function"),
						"function", jObjV("name", fn.get("name").str(), "arguments", argStr)))
				}
				item := jObjV("role", "assistant", "content", text, "tool_calls", calls)
				if keepReasoning && m.has("reasoning_content") {
					item.set("reasoning_content", jStrV(m.get("reasoning_content").str()))
				}
				flat.arr = append(flat.arr, item)
				continue
			}
			calls := jArrV()
			for _, tc := range m.get("tool_calls").arr {
				fn := tc.get("function")
				if !fn.isObj() {
					fn = jObjV()
				}
				calls.arr = append(calls.arr, jObjV("name", fn.get("name").str(),
					"arguments", fn.get("arguments").str()))
			}
			if len(calls.arr) > 0 {
				text = text + "\n\n" + leakMarker + "\n" + calls.dumpPy()
			}
			item := jObjV("role", "assistant", "content", text)
			if keepReasoning && m.has("reasoning_content") {
				item.set("reasoning_content", jStrV(m.get("reasoning_content").str()))
			}
			flat.arr = append(flat.arr, item)
			continue
		}

		if role == "user" || role == "assistant" {
			var item *jval
			if len(imgs) > 0 {
				parts := flattenContentParts(m.get("content"))
				if parts.isArr() {
					item = jObjV("role", role, "content", parts)
				} else {
					item = jObjV("role", role, "content", text)
				}
			} else {
				item = jObjV("role", role, "content", text)
			}
			if role == "assistant" && keepReasoning && m.has("reasoning_content") {
				item.set("reasoning_content", jStrV(m.get("reasoning_content").str()))
			}
			flat.arr = append(flat.arr, item)
		} else if role != "" {
			flat.arr = append(flat.arr, jObjV("role", "user", "content", text))
		}
	}
	return systemText, hasSystem, flat, images
}

// ---- build_qoder_body ----

// buildQoderBody 构造 agent_chat_generation 上游请求体（build_qoder_body）。
func (c *Client) buildQoderBody(payload *jval, cred Cred, modelKey, realm string) *jval {
	r := NormalizeRealm(realm)
	body := baseprompt.clone()
	messages := normalizeRoles(payload.get("messages"))
	if !messages.isArr() {
		messages = jArrV()
	}
	messages = sanitizeMessages(messages)
	model := payload.get("model").str()
	messages = backfillReasoningContent(messages, model, modelKey)

	useStructured := false
	if v := payload.get("_qd_structured"); v != nil && !v.isNull() {
		useStructured = v.truthy()
	} else {
		useStructured = structuredToolHistoryEnabled(model, modelKey, r, messages)
	}
	systemText, hasSystem, flat, images := flattenMessages(messages,
		isDeepseekModel(model, modelKey), useStructured)

	// 模板只保留 system 基座（样例 user 轮次丢弃），真实会话随后追加。
	var tmplSystem []*jval
	if msgs := body.get("messages"); msgs.isArr() {
		for _, m := range msgs.arr {
			if m.isObj() && m.get("role").str() == "system" {
				tmplSystem = append(tmplSystem, m)
			}
		}
	}
	if !hasSystem {
		// 无 config 覆写：SYSTEM_PROMPT 恒为默认值，保留模板自带 system。
		body.set("messages", jArrV(tmplSystem...))
	} else {
		body.set("messages", jArrV(jObjV("role", "system", "content", systemText)))
	}
	body.get("messages").arr = append(body.get("messages").arr, flat.arr...)

	// 最新用户提示词（chat_context 高亮 + business 会话名）。
	prompt := ""
	for i := len(flat.arr) - 1; i >= 0; i-- {
		m := flat.arr[i]
		if m.get("role").str() == "user" && m.get("content").truthy() {
			cv := m.get("content")
			if cv.isArr() {
				var parts []string
				for _, p := range cv.arr {
					if p.isObj() && p.get("type").str() == "text" {
						parts = append(parts, p.get("text").str())
					}
				}
				prompt = strings.Join(parts, "\n")
			} else {
				prompt = cv.str()
			}
			break
		}
	}

	nid := RandomUUID()
	body.set("request_id", jStrV(nid))
	body.set("chat_record_id", jStrV(nid))
	body.set("request_set_id", jStrV(RandomUUID()))
	body.set("session_id", jStrV(RandomUUID()))
	body.set("stream", jBoolV(true))
	body.set("agent_id", jStrV("agent_common"))
	userType := cred.UserType
	if userType == "" {
		userType = DefaultUserType
	}
	body.set("aliyun_user_type", jStrV(userType))

	// model_config（按出口区域目录取元数据 + 动态 key）。
	catalogMeta := c.catalogMetaFor(r, modelKey)
	mc := body.get("model_config")
	if !mc.isObj() {
		mc = jObjV()
	}
	mc.set("key", jStrV(modelKey))
	mc.set("display_name", jStrV(nonEmpty(catalogMeta.get("display_name").str(), modelKey)))
	mc.set("model", jStrV(""))
	mc.set("format", jStrV("openai"))
	mc.set("is_vl", jBoolV(catalogMeta.get("is_vl").truthy()))
	mc.set("is_reasoning", jBoolV(catalogMeta.get("is_reasoning").truthy()))
	mc.set("api_key", jStrV(""))
	mc.set("url", jStrV(""))
	mc.set("source", jStrV("system"))
	maxIn := catalogMeta.get("max_input_tokens")
	if v, ok := maxIn.num(); ok && v > 0 {
		mc.set("max_input_tokens", jIntV(int64(v)))
	} else {
		mc.set("max_input_tokens", jIntV(180000))
	}
	body.set("model_config", mc)

	// chat_context 高亮与模型配置副本。
	cc := body.get("chat_context")
	if !cc.isObj() {
		cc = jObjV()
	}
	txt := cc.get("text")
	if !txt.isObj() {
		txt = jObjV()
	}
	txt.set("text", jStrV(prompt))
	cc.set("text", txt)
	extra := cc.get("extra")
	if !extra.isObj() {
		extra = jObjV()
	}
	oc := extra.get("originalContent")
	if !oc.isObj() {
		oc = jObjV()
	}
	oc.set("text", jStrV(prompt))
	extra.set("originalContent", oc)
	extra.set("modelConfig", mc.clone())
	cc.set("extra", extra)
	if len(images) > 0 && keepTopLevelImages {
		imgArr := jArrV()
		for _, u := range images {
			imgArr.arr = append(imgArr.arr, jStrV(u))
		}
		cc.set("imageUrls", imgArr)
		body.set("image_urls", imgArr.clone())
	}
	body.set("chat_context", cc)

	// parameters：max_tokens / reasoning_effort。
	params := body.get("parameters")
	if !params.isObj() {
		params = jObjV()
	}
	maxTokens := payload.get("max_tokens")
	if !maxTokens.truthy() {
		maxTokens = payload.get("max_completion_tokens")
	}
	if v, ok := maxTokens.num(); ok && v != 0 {
		params.set("max_tokens", jIntV(int64(v)))
	}
	if effort := requestedEffort(payload); effort != "" {
		norm, note, send := normalizeReasoningEffort(effort, catalogMeta)
		if note != "" {
			c.logf("reasoning_effort %s on '%s' (supported=%v)", note,
				nonEmpty(modelKey, model), supportedEfforts(catalogMeta))
		}
		if send {
			params.set("reasoning_effort", jStrV(norm))
		}
	}
	body.set("parameters", params)

	// tools：客户端给了就用（custom 降级 + 丢弃不支持类型），否则置空。
	tools := payload.get("tools")
	if tools.isArr() && len(tools.arr) > 0 {
		body.set("tools", toolsForChat(tools))
	} else {
		body.set("tools", jArrV())
	}
	normalizeTools(body)

	// business 会话卡片。
	biz := body.get("business")
	if !biz.isObj() {
		biz = jObjV()
	}
	biz.set("id", jStrV(RandomUUID()))
	biz.set("begin_at", jIntV(nowMillis()))
	bname := "chat"
	if prompt != "" {
		bname = runeHead(prompt, 30)
	}
	biz.set("name", jStrV(bname))
	biz.setDefault("product", jStrV("cli"))
	biz.setDefault("type", jStrV("agent"))
	biz.setDefault("stage", jStrV("start"))
	body.set("business", biz)
	return body
}

const keepTopLevelImages = false // QD_KEEP_TOPLEVEL_IMAGES 默认关

func runeHead(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
