package kiro

// OpenAI chat.completions → Kiro generateAssistantResponse 请求体（移植自 Kiro-Go
// proxy/translator.go 的 OpenAIToKiro 及其配套的历史清洗 / 截断 / 图片 / 工具 schema 处理）。
//
// Kiro 的对话模型与 OpenAI 不同，几条硬约束决定了转换的形状：
//   - 只有一条 currentMessage（最后一条 user 消息），其余一律进 history，且 history 必须
//     以 user 轮开头、user / assistant 交替；
//   - system 提示词没有专门字段：作为 history 开头的一对「user: 指令 / assistant: 好的」；
//   - history 里不能带结构化的工具调用 / 工具结果（会 400 Improperly formed request），
//     只有「最后一个 assistant 工具轮 + 当前消息里回答它的 toolResults」可以保持结构化，
//     更早的工具往返改写成 user 轮里的文本叙述；
//   - 工具 schema 不接受 additionalProperties / 空 required；
//   - 整个请求体有大小上限（≈1MB，超了 400 Input is too long），超限时丢最早的历史。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// 请求体结构
// ---------------------------------------------------------------------------

type kiroPayload struct {
	ConversationState struct {
		AgentContinuationID string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage userInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []historyMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *inferenceConfig `json:"inferenceConfig,omitempty"`
	// AdditionalModelRequestFields 模型的原生扩展参数（目前只有 reasoning.effort，见 effort.go）。
	AdditionalModelRequestFields map[string]any `json:"additionalModelRequestFields,omitempty"`

	// toolNameMap 出站（缩短后）工具名 → 客户端原名；不序列化。
	toolNameMap map[string]string
}

type userInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []kiroImage              `json:"images,omitempty"`
	UserInputMessageContext *userInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type userInputMessageContext struct {
	Tools       []toolWrapper `json:"tools,omitempty"`
	ToolResults []toolResult  `json:"toolResults,omitempty"`
}

type toolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema inputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type inputSchema struct {
	JSON any `json:"json"`
}

type toolResult struct {
	ToolUseID string          `json:"toolUseId"`
	Content   []resultContent `json:"content"`
	Status    string          `json:"status"`
}

type resultContent struct {
	Text string `json:"text"`
}

type kiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type historyMessage struct {
	UserInputMessage         *userInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *assistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type assistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []historyTool `json:"toolUses,omitempty"`
}

type historyTool struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type inferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI 请求结构
// ---------------------------------------------------------------------------

type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         float64         `json:"temperature,omitempty"`
	TopP                float64         `json:"top_p,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Tools               []openAITool    `json:"tools,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

// UnmarshalJSON 同时接受 Chat Completions 的嵌套形 {"type":"function","function":{…}}
// 与 Responses API 的扁平形 {"type":"function","name":…,"parameters":…}——
// 后者按嵌套形解析会得到空名字，Kiro 回 400 Improperly formed request。
func (t *openAITool) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
		Function    *struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Type = raw.Type
	if raw.Function != nil {
		t.Function.Name = raw.Function.Name
		t.Function.Description = raw.Function.Description
		t.Function.Parameters = raw.Function.Parameters
	}
	if t.Function.Name == "" {
		t.Function.Name = raw.Name
	}
	if t.Function.Description == "" {
		t.Function.Description = raw.Description
	}
	if t.Function.Parameters == nil {
		t.Function.Parameters = raw.Parameters
	}
	return nil
}

// ---------------------------------------------------------------------------
// 模型名
// ---------------------------------------------------------------------------

// modelAliases 需要显式改写的模型名：带日期的快照、跨代旧名、非 Anthropic 名字的兜底。
// 普通的「- → .」版本号写法（claude-sonnet-4-5）由 claudeVersionPattern 统一处理。
var modelAliases = []struct{ key, value string }{
	{"claude-sonnet-4-20250514", "claude-sonnet-4"},
	{"claude-3-5-sonnet", "claude-sonnet-4.5"},
	{"claude-3-7-sonnet", "claude-sonnet-4.5"},
	{"claude-3-opus", "claude-sonnet-4.5"},
	{"claude-3-sonnet", "claude-sonnet-4"},
	{"claude-3-haiku", "claude-haiku-4.5"},
}

// claudeVersionPattern claude-{family}-N-M → claude-{family}-N.M（次版本 1–2 位，带 \b，
// 避免把 claude-sonnet-4-20250514 这类日期快照误改）。fable 是 Kiro 目录里的新系列
// （claude-fable-5.1，上游只认带点的写法）。
var claudeVersionPattern = regexp.MustCompile(`claude-(opus|sonnet|haiku|fable)-(\d+)-(\d{1,2})\b`)

// claudeDateSuffix Anthropic 的日期快照后缀（claude-sonnet-4-5-20250929）：Kiro 目录里没有带日期的 id，
// 去掉后按版本号归一（claude-sonnet-4-5-20250929 → claude-sonnet-4.5）。
var claudeDateSuffix = regexp.MustCompile(`-20\d{6}$`)

// ParseModel 解析客户端模型名：去掉思考后缀（返回 thinking=true），做别名与版本号归一。
func ParseModel(model, thinkingSuffix string) (kiroModel string, thinking bool) {
	model = strings.TrimSpace(model)
	lower := strings.ToLower(model)
	if thinkingSuffix != "" && strings.HasSuffix(lower, strings.ToLower(thinkingSuffix)) {
		thinking = true
		model = model[:len(model)-len(thinkingSuffix)]
		lower = strings.ToLower(model)
	}
	for _, a := range modelAliases {
		if strings.Contains(lower, a.key) {
			return a.value, thinking
		}
	}
	if strings.HasPrefix(lower, "claude-") {
		lower = claudeDateSuffix.ReplaceAllString(lower, "")
	}
	if claudeVersionPattern.MatchString(lower) {
		return claudeVersionPattern.ReplaceAllString(lower, "claude-$1-$2.$3"), thinking
	}
	if strings.HasPrefix(lower, "claude-") {
		return lower, thinking
	}
	return model, thinking
}

// ---------------------------------------------------------------------------
// 转换主流程
// ---------------------------------------------------------------------------

const (
	origin                        = "AI_EDITOR"
	maxToolDescLen                = 10237
	minimalFallbackUserContent    = "."
	toolResultsContinuationPrefix = "Tool results:"
	toolResultImagePlaceholder    = "[Tool returned an image; the image is attached to this message.]"
	// maxPayloadBytes 请求体上限：略低于上游实测阈值（CONTENT_LENGTH_EXCEEDS_THRESHOLD），
	// 给请求头与序列化差异留余量。
	maxPayloadBytes       = 900 * 1024
	truncationPlaceholder = "[Earlier conversation history was truncated to fit the model's input limit. Older messages and tool activity have been omitted.]"
	minRecentHistoryTurns = 4
)

// thinkingPrompt 开启思考的提示词前缀（Kiro 没有结构化的 thinking 参数，靠系统提示词开关）。
func thinkingPrompt(budget int) string {
	return fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", budget)
}

// buildPayload OpenAI 请求 → Kiro 请求体。thinkingBudget > 0 时注入思考提示词。
func buildPayload(req *openAIRequest, modelID string, thinkingBudget int) *kiroPayload {
	var systemPrompt string
	var rest []openAIMessage
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			if s := extractMessageText(msg.Content); s != "" {
				systemPrompt += s + "\n"
			}
			continue
		}
		rest = append(rest, msg)
	}
	if thinkingBudget > 0 {
		systemPrompt = thinkingPrompt(thinkingBudget) + "\n\n" + systemPrompt
	}

	kiroTools, nameMap := convertTools(req.Tools)
	outName := func(name string) string {
		short := shortenToolName(name)
		return short
	}

	history := make([]historyMessage, 0, len(rest))
	var curContent string
	var curImages []kiroImage
	var curResults []toolResult

	for i, msg := range rest {
		isLast := i == len(rest)-1
		switch msg.Role {
		case "user":
			content, images := extractUserContent(msg.Content)
			content = normalizeUserContent(content, len(images) > 0)
			if isLast {
				curContent, curImages = content, images
			} else {
				history = append(history, historyMessage{UserInputMessage: &userInputMessage{
					Content: content, ModelID: modelID, Origin: origin, Images: images,
				}})
			}
		case "assistant":
			content := extractMessageText(msg.Content)
			var uses []historyTool
			for _, tc := range msg.ToolCalls {
				var input map[string]any
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = map[string]any{}
				}
				uses = append(uses, historyTool{ToolUseID: tc.ID, Name: outName(tc.Function.Name), Input: input})
			}
			history = append(history, historyMessage{AssistantResponseMessage: &assistantResponseMessage{
				Content: content, ToolUses: uses,
			}})
		case "tool":
			text, imgs := extractUserContent(msg.Content)
			var content string
			if len(imgs) > 0 {
				curImages = append(curImages, imgs...)
				content = strings.TrimSpace(text)
				if content == "" {
					content = toolResultImagePlaceholder
				}
			} else {
				content = extractMessageText(msg.Content)
			}
			curResults = append(curResults, toolResult{
				ToolUseID: msg.ToolCallID, Content: []resultContent{{Text: content}}, Status: "success",
			})
			next := i + 1
			if (next >= len(rest) || rest[next].Role != "tool") && !isLast {
				// 一组工具结果之后还有后续消息：结构化地放进 history 的 user 轮，
				// 由 sanitizeHistory 统一改写成文本（这里不预填叙述，避免重复）。
				history = append(history, historyMessage{UserInputMessage: &userInputMessage{
					ModelID: modelID, Origin: origin, Images: curImages,
					UserInputMessageContext: &userInputMessageContext{ToolResults: curResults},
				}})
				curResults, curImages = nil, nil
			}
		}
	}

	if systemPrompt != "" {
		history = append([]historyMessage{
			{UserInputMessage: &userInputMessage{Content: strings.TrimSpace(systemPrompt), ModelID: modelID, Origin: origin}},
			{AssistantResponseMessage: &assistantResponseMessage{Content: "I will follow these instructions."}},
		}, history...)
	}

	curIDs := collectToolResultIDs(curResults)
	keepCur := currentResultsMatchLastAssistant(history, curIDs)
	if keepCur {
		history = sanitizeHistory(history, curIDs)
	} else {
		history = sanitizeHistory(history, nil)
	}

	final := curContent
	if len(curResults) > 0 {
		final = joinHistoryText(final, buildToolResultsContinuation(curResults))
	}
	if final == "" {
		if len(curImages) > 0 {
			final = normalizeUserContent("", true)
		} else {
			final = minimalFallbackUserContent
		}
	}

	p := &kiroPayload{toolNameMap: nameMap}
	p.ConversationState.ChatTriggerType = "MANUAL"
	p.ConversationState.ConversationID = buildConversationID(modelID, systemPrompt, firstConversationAnchor(rest))
	p.ConversationState.CurrentMessage.UserInputMessage = userInputMessage{
		Content: final, ModelID: modelID, Origin: origin, Images: curImages,
	}
	var attach []toolResult
	if keepCur {
		attach = curResults
	}
	if len(kiroTools) > 0 || len(attach) > 0 {
		p.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &userInputMessageContext{
			Tools: kiroTools, ToolResults: attach,
		}
	}
	if len(history) > 0 {
		p.ConversationState.History = history
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxCompletionTokens
	}
	if maxTok > 0 || req.Temperature > 0 || req.TopP > 0 {
		p.InferenceConfig = &inferenceConfig{MaxTokens: maxTok, Temperature: req.Temperature, TopP: req.TopP}
	}
	truncatePayloadToLimit(p, systemPrompt != "")
	return p
}

// ---------------------------------------------------------------------------
// 内容提取
// ---------------------------------------------------------------------------

func extractUserContent(content any) (string, []kiroImage) {
	if s, ok := content.(string); ok {
		return s, nil
	}
	var text string
	var images []kiroImage
	visit := func(part map[string]any) {
		if t, ok := extractTextPart(part); ok {
			text += t
		}
		if img := extractImagePart(part); img != nil {
			images = append(images, *img)
		}
	}
	if part, ok := content.(map[string]any); ok {
		visit(part)
	}
	if parts, ok := content.([]any); ok {
		for _, p := range parts {
			if part, ok := p.(map[string]any); ok {
				visit(part)
			}
		}
	}
	if len(images) > 0 {
		text = sanitizeImagePlaceholders(text)
	}
	return text, images
}

func extractMessageText(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if text, _ := extractUserContent(content); strings.TrimSpace(text) != "" {
		return text
	}
	switch v := content.(type) {
	case map[string]any:
		if nested, ok := v["content"]; ok {
			if t := extractMessageText(nested); strings.TrimSpace(t) != "" {
				return t
			}
		}
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if t := extractMessageText(item); strings.TrimSpace(t) != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "")
		}
	}
	if raw, err := json.Marshal(content); err == nil {
		return string(raw)
	}
	return ""
}

func extractTextPart(part map[string]any) (string, bool) {
	switch t, _ := part["type"].(string); t {
	case "text", "input_text":
		if s, ok := part["text"].(string); ok {
			return s, true
		}
	}
	if s, ok := part["text"].(string); ok {
		return s, true
	}
	return "", false
}

func extractImagePart(part map[string]any) *kiroImage {
	if pt, _ := part["type"].(string); pt != "" {
		switch pt {
		case "image", "image_url", "input_image", "file", "input_file":
		default:
			return nil
		}
	}
	if f, ok := part["file"].(map[string]any); ok {
		if img := extractImagePart(f); img != nil {
			return img
		}
	}
	if src, ok := part["source"].(map[string]any); ok {
		if data, ok := src["data"].(string); ok {
			mt, _ := src["media_type"].(string)
			if img := parseDataURL(data); img != nil {
				return img
			}
			if img := parseBase64Image(data, strings.TrimPrefix(strings.ToLower(mt), "image/")); img != nil {
				return img
			}
		}
		if img := extractImagePart(src); img != nil {
			return img
		}
	}
	for _, k := range []string{"mime", "media_type", "mime_type"} {
		if v, ok := part[k].(string); ok && !strings.HasPrefix(strings.ToLower(v), "image/") {
			return nil
		}
	}
	if v, ok := part["url"].(string); ok {
		if img := parseDataURL(v); img != nil {
			return img
		}
	}
	if v, ok := part["b64_json"].(string); ok {
		if img := parseBase64Image(v, "png"); img != nil {
			return img
		}
	}
	if raw, ok := part["image_url"]; ok {
		switch v := raw.(type) {
		case string:
			if img := parseDataURL(v); img != nil {
				return img
			}
		case map[string]any:
			if u, ok := v["url"].(string); ok {
				if img := parseDataURL(u); img != nil {
					return img
				}
			}
		}
	}
	if v, ok := part["image_base64"].(string); ok {
		if img := parseBase64Image(v, "png"); img != nil {
			return img
		}
	}
	if v, ok := part["data"].(string); ok {
		if img := parseDataURL(v); img != nil {
			return img
		}
		if img := parseBase64Image(v, "png"); img != nil {
			return img
		}
	}
	return nil
}

var (
	imagePlaceholderRe = regexp.MustCompile(`\[Image\s+\d+\]`)
	dataURLRe          = regexp.MustCompile(`^data:image/([a-zA-Z0-9+.-]+)(;[a-zA-Z0-9=._:+-]+)*;base64,(.+)$`)
)

func sanitizeImagePlaceholders(text string) string {
	cleaned := imagePlaceholderRe.ReplaceAllString(text, "")
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

func normalizeUserContent(text string, hasImages bool) string {
	t := strings.TrimSpace(text)
	if t == "" && hasImages {
		return "Please analyze the attached image."
	}
	return t
}

// parseDataURL 只接受 data:image/…;base64,… （远程图片 URL 不下载，Kiro 只收内联字节）。
func parseDataURL(u string) *kiroImage {
	cleaned := strings.TrimSpace(strings.NewReplacer("\n", "", "\r", "").Replace(u))
	if strings.Contains(cleaned, "[Image") {
		return nil
	}
	m := dataURLRe.FindStringSubmatch(cleaned)
	if len(m) != 4 {
		return nil
	}
	return parseBase64Image(m[3], m[1])
}

func parseBase64Image(data, format string) *kiroImage {
	format = strings.ToLower(format)
	if format == "jpg" {
		format = "jpeg"
	}
	ok := false
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if _, err := enc.DecodeString(data); err == nil {
			ok = true
			break
		}
	}
	if !ok {
		return nil
	}
	if format == "" {
		format = "png"
	}
	img := &kiroImage{Format: format}
	img.Source.Bytes = data
	return img
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// convertTools OpenAI 工具 → Kiro toolSpecification。名字超过 64 字符时缩短，并返回
// 「缩短名 → 原名」映射（响应里的工具调用据此还原，客户端才能对上自己的工具表）。
func convertTools(tools []openAITool) ([]toolWrapper, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]toolWrapper, 0, len(tools))
	nameMap := map[string]string{}
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue // Kiro 拒收空名字的工具
		}
		short := shortenToolName(name)
		if short != name {
			nameMap[short] = name
		}
		desc := t.Function.Description
		if len(desc) > maxToolDescLen {
			desc = desc[:maxToolDescLen] + "..."
		}
		var w toolWrapper
		w.ToolSpecification.Name = short
		w.ToolSpecification.Description = normalizeToolDesc(desc, short)
		w.ToolSpecification.InputSchema = inputSchema{JSON: ensureObjectSchema(t.Function.Parameters)}
		out = append(out, w)
	}
	if len(nameMap) == 0 {
		nameMap = nil
	}
	return out, nameMap
}

// ensureObjectSchema 保证 schema 顶层是 object，并清理 Kiro 不接受的字段。
func ensureObjectSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{"type": "object"}
	}
	c := cloneSchemaMap(m)
	cleanSchema(c)
	// 顶层 type 必须是非空字符串、properties 必须是对象（MCP 工具偶尔给出 null，上游会 400）。
	if t, ok := c["type"].(string); !ok || strings.TrimSpace(t) == "" {
		c["type"] = "object"
	}
	if p, has := c["properties"]; has {
		if _, ok := p.(map[string]any); !ok {
			c["properties"] = map[string]any{}
		}
	}
	return c
}

func cloneSchemaMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneSchemaValue(v)
	}
	return out
}

func cloneSchemaValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneSchemaMap(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, cloneSchemaValue(x))
		}
		return out
	default:
		return v
	}
}

// schemaMapKeywords 值是「名字 → 子 schema」映射的关键字：映射本身不是 schema，名字可以是任何词
// （包括 required / additionalProperties），只清理映射里的每个子 schema。
var schemaMapKeywords = map[string]bool{
	"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true,
}

// schemaDataKeywords 值是数据而不是 schema 的关键字，原样保留。
var schemaDataKeywords = map[string]bool{"default": true, "enum": true, "const": true, "examples": true, "example": true}

// cleanSchema 递归清理会让 Kiro 400 的内容：删掉 additionalProperties；required 只保留字符串名字，
// 没有名字（空 / null / 非数组）就整个删掉。
func cleanSchema(m map[string]any) {
	delete(m, "additionalProperties")
	if req, ok := m["required"]; ok {
		if names := requiredNames(req); len(names) > 0 {
			m["required"] = names
		} else {
			delete(m, "required")
		}
	}
	for k, v := range m {
		if k == "required" || schemaDataKeywords[k] {
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			if schemaMapKeywords[k] {
				for _, sub := range t {
					if s, ok := sub.(map[string]any); ok {
						cleanSchema(s)
					}
				}
			} else {
				cleanSchema(t)
			}
		case []any:
			for _, x := range t {
				if sub, ok := x.(map[string]any); ok {
					cleanSchema(sub)
				}
			}
		}
	}
}

// requiredNames required 里的字符串名字（其它类型的元素和非数组一律丢弃）。
func requiredNames(v any) []any {
	var out []any
	switch arr := v.(type) {
	case []any:
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range arr {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func normalizeToolDesc(desc, name string) string {
	if strings.TrimSpace(desc) != "" {
		return desc
	}
	return "Tool: " + name
}

// shortenToolName Kiro 工具名上限 64 字符；MCP 工具 mcp__server__tool 优先缩成 mcp__tool。
func shortenToolName(name string) string {
	if len(name) <= 64 {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if i := strings.LastIndex(name, "__"); i > 5 {
			if s := "mcp__" + name[i+2:]; len(s) <= 64 {
				return s
			}
		}
	}
	return name[:64]
}

// mapFinishReason Kiro stopReason → OpenAI finish_reason。
func mapFinishReason(reason string, toolCount int) string {
	if toolCount > 0 {
		return "tool_calls"
	}
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length", "model_context_window_exceeded", "context_window_exceeded":
		return "length"
	case "refusal", "content_filter", "content_filtered", "guardrail_intervened":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// 历史清洗
// ---------------------------------------------------------------------------

func collectToolResultIDs(results []toolResult) map[string]bool {
	if len(results) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(results))
	for _, r := range results {
		if id := strings.TrimSpace(r.ToolUseID); id != "" {
			ids[id] = true
		}
	}
	return ids
}

// currentResultsMatchLastAssistant 当前消息的工具结果是否恰好回答了 history 最后一个
// assistant 轮的全部工具调用（只有这种情况结构化配对才能保留）。
func currentResultsMatchLastAssistant(history []historyMessage, ids map[string]bool) bool {
	if len(history) == 0 {
		return false
	}
	last := history[len(history)-1]
	if last.AssistantResponseMessage == nil || len(last.AssistantResponseMessage.ToolUses) == 0 || len(ids) == 0 {
		return false
	}
	for _, tu := range last.AssistantResponseMessage.ToolUses {
		if !ids[tu.ToolUseID] {
			return false
		}
	}
	return true
}

// pollutedToolCallText 早期代理写进 assistant 轮的「[Called tool X …]」叙述：模型见多了会
// 模仿着吐这种文本而不发真正的工具调用，回传上游前剥掉。
var (
	pollutedToolCallText = regexp.MustCompile(`\[Called tool [^\]]*\]`)
	tripleNewline        = regexp.MustCompile(`\n{3,}`)
)

func stripPollutedToolCallText(content string) string {
	if !strings.Contains(content, "[Called tool ") {
		return content
	}
	cleaned := pollutedToolCallText.ReplaceAllString(content, "")
	return strings.TrimSpace(tripleNewline.ReplaceAllString(cleaned, "\n\n"))
}

// narrateToolResults 把结构化工具结果写成 user 轮文本（按工具名标注来源）。
// 工具活动只叙述在 user 轮里：写进 assistant 轮会教模型「用文本调用工具」。
func narrateToolResults(results []toolResult, names map[string]string) string {
	if len(results) == 0 {
		return ""
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		var texts []string
		for _, c := range r.Content {
			if strings.TrimSpace(c.Text) != "" {
				texts = append(texts, c.Text)
			}
		}
		body := strings.Join(texts, "\n")
		if strings.TrimSpace(body) == "" {
			body = "(no output)"
		}
		if n := names[r.ToolUseID]; n != "" {
			parts = append(parts, fmt.Sprintf("[%s] %s", n, body))
		} else {
			parts = append(parts, body)
		}
	}
	return toolResultsContinuationPrefix + "\n\n" + strings.Join(parts, "\n\n")
}

func joinHistoryText(existing, narrated string) string {
	existing, narrated = strings.TrimSpace(existing), strings.TrimSpace(narrated)
	switch {
	case existing != "" && narrated != "":
		return existing + "\n\n" + narrated
	case narrated != "":
		return narrated
	default:
		return existing
	}
}

// sanitizeHistory 把 history 里的结构化工具调用 / 结果改写成文本，最多保留一个「活动」
// 工具轮（history 最后一个 assistant 轮，且它的全部调用都被当前消息的结果回答）。
func sanitizeHistory(history []historyMessage, curIDs map[string]bool) []historyMessage {
	if len(history) == 0 {
		return history
	}
	names := map[string]string{}
	for i := range history {
		if a := history[i].AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				if tu.ToolUseID != "" && tu.Name != "" {
					names[tu.ToolUseID] = tu.Name
				}
			}
		}
	}
	active := -1
	if len(curIDs) > 0 {
		if last := history[len(history)-1].AssistantResponseMessage; last != nil && len(last.ToolUses) > 0 {
			all := true
			for _, tu := range last.ToolUses {
				if !curIDs[tu.ToolUseID] {
					all = false
					break
				}
			}
			if all {
				active = len(history) - 1
			}
		}
	}
	for i := range history {
		msg := &history[i]
		if a := msg.AssistantResponseMessage; a != nil {
			if a.Content != "" {
				a.Content = stripPollutedToolCallText(a.Content)
			}
			if len(a.ToolUses) > 0 && i != active {
				a.ToolUses = nil // 只丢结构化调用，不往 assistant 轮里写任何调用叙述
			}
		}
		if u := msg.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			ctx := u.UserInputMessageContext
			if len(ctx.ToolResults) > 0 {
				u.Content = joinHistoryText(u.Content, narrateToolResults(ctx.ToolResults, names))
				ctx.ToolResults = nil
			}
			ctx.Tools = nil // history 里也不能带工具定义
			u.UserInputMessageContext = nil
		}
		if u := msg.UserInputMessage; u != nil && strings.TrimSpace(u.Content) == "" && len(u.Images) == 0 {
			u.Content = minimalFallbackUserContent
		}
	}
	// 第二遍：去掉空壳 assistant 轮（清洗后没内容的 / 只有 "." 占位的），并合并相邻的
	// 完全相同的 user 轮（客户端死循环重试同一个工具错误时会出现成百上千条）。
	cleaned := history[:0:0]
	for _, msg := range history {
		if a := msg.AssistantResponseMessage; a != nil && len(a.ToolUses) == 0 {
			if c := strings.TrimSpace(a.Content); c == "" || c == minimalFallbackUserContent {
				continue
			}
		}
		if u := msg.UserInputMessage; u != nil && len(cleaned) > 0 {
			if last := cleaned[len(cleaned)-1].UserInputMessage; last != nil &&
				strings.TrimSpace(last.Content) == strings.TrimSpace(u.Content) &&
				strings.TrimSpace(u.Content) != "" && len(u.Images) == 0 {
				continue
			}
		}
		cleaned = append(cleaned, msg)
	}
	return trimLeadingAssistant(cleaned)
}

func trimLeadingAssistant(history []historyMessage) []historyMessage {
	i := 0
	for i < len(history) && history[i].AssistantResponseMessage != nil {
		i++
	}
	if i >= len(history) {
		if i == 0 {
			return history
		}
		return nil
	}
	return history[i:]
}

func buildToolResultsContinuation(results []toolResult) string {
	var parts []string
	for _, r := range results {
		for _, c := range r.Content {
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	if len(parts) == 0 {
		return minimalFallbackUserContent
	}
	joined := toolResultsContinuationPrefix + "\n\n" + strings.Join(parts, "\n\n")
	if len(joined) > 4000 {
		return joined[:4000]
	}
	return joined
}

// ---------------------------------------------------------------------------
// 请求体大小控制
// ---------------------------------------------------------------------------

// truncatePayloadToLimit 请求体超过 maxPayloadBytes 时从最早的对话轮开始丢，保留：
// system 引导对、最近至少 minRecentHistoryTurns 轮、当前消息；丢过的位置插一条说明。
func truncatePayloadToLimit(p *kiroPayload, hasPriming bool) {
	if p == nil || payloadSize(p) <= maxPayloadBytes {
		return
	}
	history := p.ConversationState.History
	primingCount := 0
	if hasPriming && len(history) >= 2 {
		primingCount = 2
	}
	priming := history[:primingCount]
	conversation := history[primingCount:]
	placeholder := historyMessage{UserInputMessage: &userInputMessage{
		Content: truncationPlaceholder, ModelID: p.ConversationState.CurrentMessage.UserInputMessage.ModelID, Origin: origin,
	}}
	sizes := make([]int, len(conversation))
	for i := range conversation {
		sizes[i] = entrySize(conversation[i])
	}
	p.ConversationState.History = priming
	running := payloadSize(p) + entrySize(placeholder)
	keepFrom := len(conversation)
	for i := len(conversation) - 1; i >= 0; i-- {
		running += sizes[i]
		if running > maxPayloadBytes && len(conversation)-i > minRecentHistoryTurns {
			break
		}
		keepFrom = i
	}
	tail := conversation[keepFrom:]
	for len(tail) > 0 && tail[0].AssistantResponseMessage != nil {
		tail = tail[1:]
	}
	rebuilt := make([]historyMessage, 0, len(priming)+1+len(tail))
	rebuilt = append(rebuilt, priming...)
	if keepFrom > 0 {
		rebuilt = append(rebuilt, placeholder)
	}
	rebuilt = append(rebuilt, tail...)
	p.ConversationState.History = rebuilt
	if payloadSize(p) > maxPayloadBytes {
		cur := &p.ConversationState.CurrentMessage.UserInputMessage
		budget := maxPayloadBytes - (payloadSize(p) - len(cur.Content))
		switch {
		case budget <= 0:
			cur.Content = minimalFallbackUserContent
		case len(cur.Content) > budget:
			cur.Content = cur.Content[:budget]
		}
	}
}

func entrySize(e historyMessage) int {
	raw, err := json.Marshal(e)
	if err != nil {
		return 0
	}
	return len(raw) + 1
}

func payloadSize(p *kiroPayload) int {
	raw, err := json.Marshal(p)
	if err != nil {
		return 0
	}
	return len(raw)
}

// ---------------------------------------------------------------------------
// conversationId
// ---------------------------------------------------------------------------

func firstConversationAnchor(messages []openAIMessage) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if t := strings.TrimSpace(extractMessageText(m.Content)); t != "" {
			return t
		}
	}
	return ""
}

// buildConversationID 同一会话（同模型 + 同 system + 同首条 user 消息）稳定映射到同一个
// conversationId，上游据此复用会话缓存；首条消息是占位文本时随机生成。
func buildConversationID(modelID, systemPrompt, anchor string) string {
	anchor = strings.TrimSpace(anchor)
	switch strings.ToLower(strings.Join(strings.Fields(anchor), " ")) {
	case "", ".", "begin conversation", "please analyze the attached image.":
		return newUUID()
	}
	return uuidSHA1(strings.Join([]string{modelID, strings.TrimSpace(systemPrompt), anchor}, "\n"))
}
