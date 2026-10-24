package server

// anthropic_convert.go Anthropic Messages 请求 → 网关原有的 OpenAI chat.completions 请求。
//
// 只做形态翻译，不做取舍：选号、轮换、冷却、跨平台回退、会话粘性、思考档位、用量记账都由
// chatCompletions 原样处理。Anthropic 特有、OpenAI 没有对应的东西（cache_control、服务端工具、
// 思考块签名）丢弃；它们不影响上游能否回答。

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Thinking      json.RawMessage    `json:"thinking"`
	OutputConfig  json.RawMessage    `json:"output_config"`
	Metadata      json.RawMessage    `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// 以下两个只出现在 role=system 的对话中途系统消息上。
	OutputConfig json.RawMessage `json:"output_config"` // 只调思考档位的指令消息（content 为空）
	ClearAt      string          `json:"clear_at"`      // "next_user_message" = 只在本轮生效
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text"`
	Thinking  string           `json:"thinking"`
	Source    *anthropicSource `json:"source"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Input     json.RawMessage  `json:"input"`
	ToolUseID string           `json:"tool_use_id"`
	Content   json.RawMessage  `json:"content"`
	IsError   bool             `json:"is_error"`
	Title     string           `json:"title"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// parseBlocks content 字段：字符串（等价于一个 text 块）或块数组。
func parseBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	if s[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []anthropicBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content must be a string or an array of content blocks")
	}
	return blocks, nil
}

// systemText system 字段：字符串或 text 块数组（Claude Code 发的是带 cache_control 的块数组）。
func systemText(raw json.RawMessage) (string, error) {
	blocks, err := parseBlocks(raw)
	if err != nil {
		return "", fmt.Errorf("system: %v", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// imagePart Anthropic 图片块 → OpenAI image_url 片段（base64 转 data URL）。
func imagePart(src *anthropicSource) (map[string]any, bool) {
	if src == nil {
		return nil, false
	}
	var u string
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return nil, false
		}
		mt := src.MediaType
		if mt == "" {
			mt = "image/png"
		}
		u = "data:" + mt + ";base64," + src.Data
	case "url":
		u = src.URL
	}
	if u == "" {
		return nil, false
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}, true
}

// documentText document 块：纯文本来源转成文字；PDF 等二进制来源上游读不了，换成一句说明。
func documentText(b anthropicBlock) string {
	if b.Source != nil && b.Source.Type == "text" && b.Source.Data != "" {
		if b.Title != "" {
			return b.Title + "\n\n" + b.Source.Data
		}
		return b.Source.Data
	}
	name := b.Title
	if name == "" {
		name = "document"
	}
	return "[Attached " + name + " could not be forwarded: only plain-text documents are supported by this gateway.]"
}

// toolResultContent tool_result 的 content：字符串，或 text / image 块数组。有图片时返回片段数组。
func toolResultContent(b anthropicBlock) any {
	blocks, err := parseBlocks(b.Content)
	if err != nil {
		// 不是合法块数组也别丢：原样当文字。
		blocks = []anthropicBlock{{Type: "text", Text: string(b.Content)}}
	}
	var texts []string
	var images []any
	for _, c := range blocks {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image":
			if p, ok := imagePart(c.Source); ok {
				images = append(images, p)
			}
		case "document":
			texts = append(texts, documentText(c))
		}
	}
	text := strings.Join(texts, "\n")
	if b.IsError && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "error") {
		text = "Error: " + text
	}
	if len(images) == 0 {
		return text
	}
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	return append(parts, images...)
}

// jsonArgs tool_use.input → OpenAI function.arguments（JSON 字符串）。
func jsonArgs(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// claudeSessionRe Claude Code 的 metadata.user_id 老写法：user_<hash>_account_<uuid>_session_<uuid>。
var claudeSessionRe = regexp.MustCompile(`session_([0-9A-Za-z-]{8,})`)

// sessionFromMetadata 从 metadata.user_id 取会话 ID（老写法是带 _session_ 的字符串，新写法是
// JSON 字符串里的 session_id）。网关的会话粘性只认对话级 ID，不认 user_id。
func sessionFromMetadata(raw json.RawMessage) string {
	var meta struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.UserID == "" {
		return ""
	}
	var inner struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(meta.UserID), &inner) == nil && inner.SessionID != "" {
		return inner.SessionID
	}
	if m := claudeSessionRe.FindStringSubmatch(meta.UserID); m != nil {
		return m[1]
	}
	return ""
}

// outputEffort output_config.effort。
func outputEffort(raw json.RawMessage) string {
	var oc struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &oc) != nil {
		return ""
	}
	return strings.TrimSpace(oc.Effort)
}

// appendSystemReminder 对话中途的系统消息 → 并进前一条 user 消息末尾的 <system-reminder> 文本。
//
// 不转成 OpenAI 的 system 消息：WorkBuddy 的 custom 提示词模式会删掉所有 system 消息，Kiro 会把它们
// 挪到最前面，位置和内容都保不住。并进 user 文本是 Claude Code 在 2.1.154 之前的原始写法
// （技能列表、hook 上下文都以 <system-reminder> 放在 user 消息里），各平台都能原样收到。
func appendSystemReminder(msgs []any, text string) []any {
	if !strings.Contains(text, "<system-reminder>") {
		text = "<system-reminder>\n" + text + "\n</system-reminder>"
	}
	if n := len(msgs); n > 0 {
		if last, ok := msgs[n-1].(map[string]any); ok && last["role"] == "user" {
			switch c := last["content"].(type) {
			case string:
				if c == "" {
					last["content"] = text
				} else {
					last["content"] = c + "\n\n" + text
				}
				return msgs
			case []any:
				last["content"] = append(c, map[string]any{"type": "text", "text": text})
				return msgs
			}
		}
	}
	// 前面是工具结果（tool 消息）或没有 user 消息：单独起一条 user 消息，形态与「tool_result + 文本」相同。
	return append(msgs, map[string]any{"role": "user", "content": text})
}

// anthropicToOpenAI 把 Anthropic 请求翻成 OpenAI chat.completions 请求体（model 用路由后的名字）。
func anthropicToOpenAI(req *anthropicRequest, model string) ([]byte, error) {
	out := map[string]any{"model": model, "stream": req.Stream}
	if req.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	sys, err := systemText(req.System)
	if err != nil {
		return nil, err
	}
	msgs := []any{}
	if sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	// directiveEffort 对话中途「只调档位」的系统消息（content 为空 + output_config.effort）：
	// 对其后的轮次生效，最后一条说了算，覆盖顶层 output_config。
	directiveEffort := ""
	for i, m := range req.Messages {
		blocks, err := parseBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages.%d: %v", i, err)
		}
		switch m.Role {
		case "system":
			// 对话中途的系统消息（Claude Code 2.1.154 起用它下发技能列表、hook 上下文、轮次提醒）。
			if e := outputEffort(m.OutputConfig); e != "" {
				directiveEffort = e
			}
			if m.ClearAt == "next_user_message" && i < lastUser {
				continue // 只在本轮生效的提醒：后面已经有新的 user 消息，Anthropic 不再渲染它
			}
			var texts []string
			for _, b := range blocks {
				// 只取文本；tool_addition / tool_removal（对话中途增删工具）上游没有对应能力，忽略。
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					texts = append(texts, b.Text)
				}
			}
			if len(texts) > 0 {
				msgs = appendSystemReminder(msgs, strings.Join(texts, "\n\n"))
			}
		case "assistant":
			var text strings.Builder
			var calls []any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use", "server_tool_use":
					if b.Type == "server_tool_use" {
						continue // 服务端工具（web_search 等）的调用记录，上游没有对应的工具
					}
					calls = append(calls, map[string]any{
						"id": b.ID, "type": "function",
						"function": map[string]any{"name": b.Name, "arguments": jsonArgs(b.Input)},
					})
				}
				// thinking / redacted_thinking：上游不认 Anthropic 的思考签名，历史里的思考块丢弃。
			}
			if text.Len() == 0 && len(calls) == 0 {
				continue
			}
			msg := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			msgs = append(msgs, msg)
		case "user":
			// tool_result 必须紧跟在带 tool_calls 的 assistant 后面：先出 tool 消息，再出这条 user 的其余内容。
			var parts []any
			hasImage := false
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": toolResultContent(b)})
				case "text":
					if b.Text != "" {
						parts = append(parts, map[string]any{"type": "text", "text": b.Text})
					}
				case "image":
					if p, ok := imagePart(b.Source); ok {
						parts = append(parts, p)
						hasImage = true
					}
				case "document":
					parts = append(parts, map[string]any{"type": "text", "text": documentText(b)})
				}
			}
			if len(parts) == 0 {
				continue
			}
			if !hasImage {
				var texts []string
				for _, p := range parts {
					texts = append(texts, p.(map[string]any)["text"].(string))
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(texts, "\n\n")})
			} else {
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			}
		default:
			return nil, fmt.Errorf("messages.%d.role: %q is not one of user, assistant, system", i, m.Role)
		}
	}
	out["messages"] = msgs

	var tools []any
	for _, t := range req.Tools {
		// 只转客户端工具（带 input_schema）。服务端工具（web_search_20250305、bash_20250124 等）没有
		// schema，上游也不会执行。
		if t.Name == "" || len(strings.TrimSpace(string(t.InputSchema))) == 0 || string(t.InputSchema) == "null" {
			continue
		}
		fn := map[string]any{"name": t.Name, "parameters": json.RawMessage(t.InputSchema)}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		var tc struct {
			Type                   string `json:"type"`
			Name                   string `json:"name"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		}
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			switch tc.Type {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "none":
				out["tool_choice"] = "none"
			case "tool":
				if tc.Name != "" {
					out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
				}
			}
			if tc.DisableParallelToolUse {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	// 思考：thinking.{type, budget_tokens} 网关本来就认（effortPeek）。adaptive = 由模型决定，按开启处理。
	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(req.Thinking, &th) == nil {
		switch th.Type {
		case "enabled":
			t := map[string]any{"type": "enabled"}
			if th.BudgetTokens > 0 {
				t["budget_tokens"] = th.BudgetTokens
			}
			out["thinking"] = t
		case "adaptive":
			out["thinking"] = map[string]any{"type": "enabled"}
		case "disabled":
			out["thinking"] = map[string]any{"type": "disabled"}
		}
	}
	if e := outputEffort(req.OutputConfig); e != "" {
		out["reasoning_effort"] = e
	}
	if directiveEffort != "" {
		out["reasoning_effort"] = directiveEffort
	}
	if sid := sessionFromMetadata(req.Metadata); sid != "" {
		out["metadata"] = map[string]any{"conversation_id": sid}
	}
	return json.Marshal(out)
}

// estimateAnthropicTokens 请求的输入 token 估算（count_tokens 与流开头的 message_start 用；
// 真实用量以上游流尾的 usage 为准）。
func estimateAnthropicTokens(req *anthropicRequest) int {
	total := 0
	if sys, err := systemText(req.System); err == nil {
		total += kiro.EstimateTokens(sys)
	}
	var walk func(blocks []anthropicBlock)
	walk = func(blocks []anthropicBlock) {
		for _, b := range blocks {
			switch b.Type {
			case "text":
				total += kiro.EstimateTokens(b.Text)
			case "thinking":
				total += kiro.EstimateTokens(b.Thinking)
			case "image":
				total += 1600 // 一张常见尺寸截图的量级
			case "document":
				total += kiro.EstimateTokens(documentText(b))
			case "tool_use":
				total += kiro.EstimateTokens(b.Name) + kiro.EstimateTokens(string(b.Input))
			case "tool_result":
				if inner, err := parseBlocks(b.Content); err == nil {
					walk(inner)
				} else {
					total += kiro.EstimateTokens(string(b.Content))
				}
			}
		}
	}
	for _, m := range req.Messages {
		total += 4 // 每条消息的角色与分隔开销
		if blocks, err := parseBlocks(m.Content); err == nil {
			walk(blocks)
		}
	}
	for _, t := range req.Tools {
		total += kiro.EstimateTokens(t.Name) + kiro.EstimateTokens(t.Description) + kiro.EstimateTokens(string(t.InputSchema))
	}
	if total < 1 {
		total = 1
	}
	return total
}
