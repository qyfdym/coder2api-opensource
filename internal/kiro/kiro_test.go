package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return string(raw)
}

var testCred = Cred{UID: "k1", Nickname: "alice", AccessToken: "at-1", RefreshToken: "rt-1",
	AuthMethod: "social", Provider: "Google", Region: "us-east-1", MachineID: "mid-1",
	ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABC"}

// ── event-stream 解码 ──────────────────────────────────────────────────────

func TestParseEventStreamAllEvents(t *testing.T) {
	stream := joinFrames(
		evt("assistantResponseEvent", map[string]any{"content": "Hel"}),
		evt("reasoningContentEvent", map[string]any{"text": "think"}),
		evt("assistantResponseEvent", map[string]any{"content": "lo"}),
		// 两个并行工具调用的分片交错到达；第二个的首片不带 id。
		evt("toolUseEvent", map[string]any{"toolUseId": "t1", "name": "read", "input": `{"pa`}),
		evt("toolUseEvent", map[string]any{"toolUseId": "t2", "name": "list", "input": `{"dir":"/"}`, "stop": true}),
		evt("toolUseEvent", map[string]any{"toolUseId": "t1", "input": `th":"a.go"}`, "stop": true}),
		evt("meteringEvent", map[string]any{"usage": 0.25}),
		evt("meteringEvent", map[string]any{"usage": 0.5}),
		evt("contextUsageEvent", map[string]any{"contextUsagePercentage": 1.5}),
		evt("metadataEvent", map[string]any{"stopReason": "tool_use", "usage": map[string]any{"inputTokens": 1200, "outputTokens": 30}}),
	)
	var text, reasoning, stop string
	var tools []toolUse
	var credits, pct float64
	var in, out int
	emitted, err := parseEventStream(bytes.NewReader(stream), &streamCallbacks{
		OnText: func(s string, th bool) {
			if th {
				reasoning += s
			} else {
				text += s
			}
		},
		OnToolUse:      func(tu toolUse) { tools = append(tools, tu) },
		OnCredits:      func(c float64) { credits = c },
		OnContextUsage: func(p float64) { pct = p },
		OnStopReason:   func(r string) { stop = r },
		OnComplete:     func(i, o int) { in, out = i, o },
	})
	if err != nil || !emitted {
		t.Fatalf("parse: emitted=%v err=%v", emitted, err)
	}
	if text != "Hello" || reasoning != "think" || stop != "tool_use" {
		t.Errorf("text=%q reasoning=%q stop=%q", text, reasoning, stop)
	}
	if len(tools) != 2 || tools[0].ID != "t2" || tools[1].ID != "t1" || tools[1].Input["path"] != "a.go" || tools[0].Input["dir"] != "/" {
		t.Errorf("tools reassembled wrong: %+v", tools)
	}
	if credits != 0.75 || pct != 1.5 || in != 1200 || out != 30 {
		t.Errorf("credits=%v pct=%v in=%d out=%d", credits, pct, in, out)
	}
}

func TestParseEventStreamErrors(t *testing.T) {
	// 空流。
	if _, err := parseEventStream(bytes.NewReader(nil), nil); err != errEmptyStream {
		t.Errorf("empty stream: %v", err)
	}
	// 流内异常帧 → 按异常类型分类。
	_, err := parseEventStream(bytes.NewReader(exceptionFrame("ThrottlingException", "Too many requests")), nil)
	if ke := streamError(err); ke.Kind != KindRateLimit || ke.Code != "ThrottlingException" {
		t.Errorf("exception frame: %+v", ke)
	}
	// CRC 被篡改。
	bad := evt("assistantResponseEvent", map[string]any{"content": "x"})
	bad[len(bad)-1] ^= 0xff
	if _, err := parseEventStream(bytes.NewReader(bad), nil); streamError(err).Kind != KindServer {
		t.Errorf("crc mismatch: %v", err)
	}
	// 工具参数没收完流就结束了 → 截断（不能把缺参数的调用交给客户端）。
	partial := evt("toolUseEvent", map[string]any{"toolUseId": "t1", "name": "read", "input": `{"pa`})
	if _, err := parseEventStream(bytes.NewReader(partial), &streamCallbacks{OnToolUse: func(toolUse) {}}); streamError(err).Kind != KindTruncated {
		t.Errorf("incomplete tool: %v", err)
	}
}

// ── 请求体转换 ─────────────────────────────────────────────────────────────

func TestBuildPayloadShape(t *testing.T) {
	longName := "mcp__very_long_server_name_for_testing_purposes__" + strings.Repeat("x", 40)
	body := `{
		"model":"claude-sonnet-4-5","max_completion_tokens":2048,"temperature":0.3,
		"messages":[
			{"role":"system","content":"Be terse."},
			{"role":"developer","content":"Use Go."},
			{"role":"user","content":"list files"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{\"dir\":\".\"}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"a.go\nb.go"},
			{"role":"assistant","content":"two files"},
			{"role":"user","content":[{"type":"text","text":"read a.go"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]},
			{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"` + longName + `","arguments":"{\"path\":\"a.go\"}"}}]},
			{"role":"tool","tool_call_id":"c2","content":"package main"}
		],
		"tools":[
			{"type":"function","function":{"name":"ls","description":"list","parameters":{"type":"object","properties":{"dir":{"type":"string"}},"required":[],"additionalProperties":false}}},
			{"type":"function","name":"` + longName + `","description":"","parameters":{"properties":{"path":{"type":"string"}}}}
		]}`
	var req openAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	c := NewClient(Options{})
	model, budget, label := c.resolveThinking(req.Model, EffortRequest{Effort: "high"})
	if model != "claude-sonnet-4.5" || budget != 32768 || label != "high" {
		t.Fatalf("thinking resolve: %s %d %s", model, budget, label)
	}
	p := buildPayload(&req, model, budget)
	h := p.ConversationState.History
	// system + developer 合并成开头的引导对，带思考提示词。
	if h[0].UserInputMessage == nil || !strings.Contains(h[0].UserInputMessage.Content, "<thinking_mode>enabled</thinking_mode>") ||
		!strings.Contains(h[0].UserInputMessage.Content, "<max_thinking_length>32768</max_thinking_length>") ||
		!strings.Contains(h[0].UserInputMessage.Content, "Be terse.") || !strings.Contains(h[0].UserInputMessage.Content, "Use Go.") {
		t.Errorf("priming: %+v", h[0].UserInputMessage)
	}
	if h[1].AssistantResponseMessage == nil || h[1].AssistantResponseMessage.Content != "I will follow these instructions." {
		t.Errorf("priming reply: %+v", h[1])
	}
	// 早期的工具往返：assistant 轮不再带结构化调用，结果改写成 user 轮文本（带工具名）。
	raw, _ := json.Marshal(h)
	hist := string(raw)
	if strings.Contains(hist, `"toolResults"`) || strings.Count(hist, `"toolUses"`) != 1 {
		t.Errorf("history must keep only the active tool turn structured: %s", hist)
	}
	if !strings.Contains(hist, "[ls] a.go") {
		t.Errorf("tool result narration missing: %s", hist)
	}
	// 当前消息：回答最后一个工具轮的结果保持结构化，图片来自上一轮 user 消息之后？（不，图片在历史里）
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 ||
		cur.UserInputMessageContext.ToolResults[0].ToolUseID != "c2" {
		t.Fatalf("current tool results: %+v", cur.UserInputMessageContext)
	}
	if !strings.Contains(cur.Content, "package main") || cur.ModelID != "claude-sonnet-4.5" || cur.Origin != "AI_EDITOR" {
		t.Errorf("current message: %+v", cur)
	}
	tools := cur.UserInputMessageContext.Tools
	if len(tools) != 2 {
		t.Fatalf("tools: %d", len(tools))
	}
	schema := tools[0].ToolSpecification.InputSchema.JSON.(map[string]any)
	if _, ok := schema["additionalProperties"]; ok {
		t.Error("additionalProperties must be stripped")
	}
	if _, ok := schema["required"]; ok {
		t.Error("empty required must be stripped")
	}
	short := tools[1].ToolSpecification.Name
	if len(short) > 64 || p.toolNameMap[short] != longName {
		t.Errorf("long tool name: %q map=%v", short, p.toolNameMap)
	}
	if tools[1].ToolSpecification.InputSchema.JSON.(map[string]any)["type"] != "object" || tools[1].ToolSpecification.Description == "" {
		t.Error("schema type / description defaults missing")
	}
	if p.InferenceConfig == nil || p.InferenceConfig.MaxTokens != 2048 || p.InferenceConfig.Temperature != 0.3 {
		t.Errorf("inference config: %+v", p.InferenceConfig)
	}
	// 图片：历史 user 轮里的 data URL 被转成 Kiro 图片。
	if !strings.Contains(hist, `"format":"png"`) {
		t.Errorf("image not converted: %s", hist)
	}
	if p.ConversationState.ConversationID == "" {
		t.Error("conversation id missing")
	}
}

func TestBuildPayloadTruncatesOversizedHistory(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	msgs := []openAIMessage{{Role: "system", Content: "sys"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, openAIMessage{Role: "user", Content: big}, openAIMessage{Role: "assistant", Content: "ok"})
	}
	msgs = append(msgs, openAIMessage{Role: "user", Content: "final"})
	p := buildPayload(&openAIRequest{Model: "claude-sonnet-4.5", Messages: msgs}, "claude-sonnet-4.5", 0)
	if n := payloadSize(p); n > maxPayloadBytes {
		t.Fatalf("payload %d > %d", n, maxPayloadBytes)
	}
	raw, _ := json.Marshal(p.ConversationState.History)
	if !strings.Contains(string(raw), "truncated to fit") || !strings.HasPrefix(p.ConversationState.History[0].UserInputMessage.Content, "sys") {
		t.Errorf("truncation must keep priming and insert placeholder")
	}
	if p.ConversationState.CurrentMessage.UserInputMessage.Content != "final" {
		t.Error("current message must survive truncation")
	}
}

func TestParseModel(t *testing.T) {
	cases := []struct {
		in, out  string
		thinking bool
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4.5", false},
		{"claude-sonnet-4.5-thinking", "claude-sonnet-4.5", true},
		{"claude-sonnet-4-20250514", "claude-sonnet-4", false},
		{"Claude-Opus-4.7", "claude-opus-4.7", false},
		{"claude-3-5-sonnet-latest", "claude-sonnet-4.5", false},
		{"auto", "auto", false},
	}
	for _, c := range cases {
		got, th := ParseModel(c.in, "-thinking")
		if got != c.out || th != c.thinking {
			t.Errorf("ParseModel(%q) = %q,%v want %q,%v", c.in, got, th, c.out, c.thinking)
		}
	}
}

// ── 思考强度 ───────────────────────────────────────────────────────────────

func TestResolveThinking(t *testing.T) {
	c := NewClient(Options{})
	cases := []struct {
		model string
		req   EffortRequest
		want  string
		bud   int
	}{
		{"claude-sonnet-4.5", EffortRequest{}, "off", 0},
		{"claude-sonnet-4.5", EffortRequest{Effort: "low"}, "low", 4096},
		{"claude-sonnet-4.5", EffortRequest{Effort: "minimal"}, "low", 4096},
		{"claude-sonnet-4.5", EffortRequest{Effort: "xhigh"}, "xhigh", 65536},
		{"claude-sonnet-4.5", EffortRequest{Effort: "none"}, "off", 0},
		{"claude-sonnet-4.5-thinking", EffortRequest{}, "max", 200000},
		{"claude-sonnet-4.5-thinking", EffortRequest{Effort: "off"}, "off", 0},
		{"claude-sonnet-4.5", EffortRequest{ThinkingType: "enabled", BudgetTokens: 10000}, "medium", 10000},
		{"claude-sonnet-4.5", EffortRequest{ThinkingType: "enabled", BudgetTokens: 10}, "low", 1024},
		{"claude-sonnet-4.5", EffortRequest{ThinkingType: "adaptive"}, "max", 200000},
		{"claude-sonnet-4.5", EffortRequest{ThinkingType: "disabled", Effort: "high"}, "off", 0},
		{"claude-sonnet-4.5", EffortRequest{Effort: "auto"}, "high", 32768},
	}
	for _, cs := range cases {
		_, b, l := c.resolveThinking(cs.model, cs.req)
		if l != cs.want || b != cs.bud {
			t.Errorf("%s %+v → %s/%d want %s/%d", cs.model, cs.req, l, b, cs.want, cs.bud)
		}
		if got := c.OutboundEffort(cs.model, cs.req); got != cs.want {
			t.Errorf("OutboundEffort %+v = %s", cs.req, got)
		}
	}
	r := effortFromBody([]byte(`{"reasoning":{"effort":"Medium"},"thinking":{"type":"enabled","budget_tokens":"2048"}}`))
	if r.Effort != "medium" || r.ThinkingType != "enabled" || r.BudgetTokens != 2048 {
		t.Errorf("effortFromBody: %+v", r)
	}
	if r := effortFromBody([]byte(`{"thinking":false}`)); r.ThinkingType != "disabled" {
		t.Errorf("thinking:false → %+v", r)
	}
}

// ── ChatStream 端到端（假上游）─────────────────────────────────────────────

func TestChatStreamStreaming(t *testing.T) {
	f := newFake()
	f.on(routeIDE, streamResp(
		evt("reasoningContentEvent", map[string]any{"text": "let me think"}),
		evt("assistantResponseEvent", map[string]any{"content": "Hello "}),
		evt("assistantResponseEvent", map[string]any{"content": "world"}),
		evt("toolUseEvent", map[string]any{"toolUseId": "tu1", "name": "get_weather", "input": `{"city":"SF"}`, "stop": true}),
		evt("meteringEvent", map[string]any{"usage": 0.42}),
		evt("contextUsageEvent", map[string]any{"contextUsagePercentage": 1.0}),
		evt("metadataEvent", map[string]any{"stopReason": "tool_use"}),
	))
	c := testClient(f)
	body := `{"model":"claude-sonnet-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	rc, err := c.ChatStream(context.Background(), testCred, []byte(body))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	frames, done := sseFrames(t, readAll(t, rc))
	got := collect(frames)
	if !done || !got.role || got.content != "Hello world" || got.reasoning != "let me think" || got.finish != "tool_calls" {
		t.Fatalf("stream: done=%v %+v", done, got)
	}
	if len(got.tools) != 1 || got.tools[0]["id"] != "tu1" {
		t.Fatalf("tool calls: %+v", got.tools)
	}
	fn := got.tools[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Errorf("tool function: %+v", fn)
	}
	if got.usage == nil || got.usage["credit"] != 0.42 || got.usage["prompt_tokens"].(float64) != 2000 {
		t.Errorf("usage: %+v", got.usage)
	}

	// 出站请求：Bearer、UA（带机器标识）、profileArn、思考提示词、agent mode。
	calls := f.calls(routeIDE)
	if len(calls) != 1 {
		t.Fatalf("upstream calls: %d", len(calls))
	}
	h := calls[0].Header
	if h.Get("Authorization") != "Bearer at-1" || !strings.Contains(h.Get("User-Agent"), "KiroIDE-") ||
		!strings.HasSuffix(h.Get("x-amz-user-agent"), "-mid-1") || h.Get("x-amzn-kiro-agent-mode") != "vibe" ||
		h.Get("X-Amz-Target") != "" || h.Get("Amz-Sdk-Invocation-Id") == "" {
		t.Errorf("headers: %v", h)
	}
	var sent map[string]any
	_ = json.Unmarshal(calls[0].Body, &sent)
	if sent["profileArn"] != testCred.ProfileArn || !strings.Contains(string(calls[0].Body), "max_thinking_length\\u003e32768") {
		t.Errorf("payload: %s", calls[0].Body)
	}
}

func TestChatStreamNonStreamAndThinkingTags(t *testing.T) {
	f := newFake()
	// 正文里带 <thinking> 块、标签被切在两个分片之间。
	f.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "<thin"}),
		evt("assistantResponseEvent", map[string]any{"content": "king>plan</thinking>Answer is 42"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	rc, err := c.ChatStream(context.Background(), testCred,
		[]byte(`{"model":"claude-sonnet-4.5-thinking","messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	frames, done := sseFrames(t, readAll(t, rc))
	got := collect(frames)
	if !done || got.content != "Answer is 42" || got.reasoning != "plan" || got.finish != "stop" {
		t.Fatalf("non-stream: %+v", got)
	}
	// 没开思考时 <thinking> 块整段丢弃。
	rc, err = c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ = sseFrames(t, readAll(t, rc))
	if got := collect(frames); got.content != "Answer is 42" || got.reasoning != "" {
		t.Fatalf("thinking off: %+v", got)
	}
}

func TestChatStreamErrorsBeforeOutput(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
		calls  int // 打到第一个入口的次数
		cwHits int // 打到 CodeWhisperer 入口的次数（换入口回退）
	}{
		{"suspended", 403, `{"message":"Your User ID temporarily is suspended","reason":"TEMPORARILY_SUSPENDED"}`, KindSuspended, 1, 0},
		{"auth", 403, `{"message":"The bearer token included in the request is invalid"}`, KindAuth, 1, 0},
		{"quota", 402, `{"message":"You have reached the limit","reason":"MONTHLY_REQUEST_COUNT"}`, KindQuota, 1, 0},
		{"too large", 400, `{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`, KindTooLarge, 1, 1},
		{"throttle", 429, `{"message":"Too many requests"}`, KindRateLimit, 1, 1},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			f := newFake()
			f.on(routeIDE, errResp(cs.status, cs.body))
			f.on(routeCW, errResp(cs.status, cs.body))
			c := testClient(f)
			rc, err := c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if rc != nil || err == nil || err.Kind != cs.want || err.Status != cs.status {
				t.Fatalf("got rc=%v err=%+v want kind %s", rc != nil, err, cs.want)
			}
			if n := len(f.calls(routeIDE)); n < cs.calls {
				t.Errorf("ide calls=%d", n)
			}
			if n := len(f.calls(routeCW)); n != cs.cwHits {
				t.Errorf("codewhisperer calls=%d want %d", n, cs.cwHits)
			}
		})
	}
}

func TestChatStreamEndpointFallback(t *testing.T) {
	f := newFake()
	f.on(routeIDE, errResp(500, `{"message":"internal"}`))
	f.on(routeCW, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "ok"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	rc, err := c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := sseFrames(t, readAll(t, rc))
	if got := collect(frames); got.content != "ok" {
		t.Fatalf("fallback content: %+v", got)
	}
	cw := f.calls(routeCW)
	if len(cw) != 1 || cw[0].Header.Get("X-Amz-Target") != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
		t.Fatalf("codewhisperer fallback: %+v", cw)
	}
}

func TestChatStreamTruncation(t *testing.T) {
	f := newFake()
	f.on(routeIDE, streamResp(evt("assistantResponseEvent", map[string]any{"content": "half an ans"})))
	c := testClient(f)
	// 流式：已有输出之后才发现截断 → error 帧，不伪装成正常结束。
	rc, err := c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := readAll(t, rc)
	frames, done := sseFrames(t, raw)
	got := collect(frames)
	if got.errFrame == nil || got.errFrame["type"] != "kiro_truncated" || !done || got.finish != "" {
		t.Fatalf("stream truncation: %s", raw)
	}
	if KindFromErrorFrame("data: "+mustJSON(map[string]any{"error": got.errFrame})) != KindTruncated {
		t.Error("KindFromErrorFrame round trip")
	}
	// 非流式：同号重试 2 次后报截断错误（客户端拿不到半截回答）。
	before := len(f.calls(routeIDE))
	_, err = c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`))
	if err == nil || err.Kind != KindTruncated {
		t.Fatalf("buffered truncation: %+v", err)
	}
	if n := len(f.calls(routeIDE)) - before; n != 1+maxSameAccountRetries {
		t.Errorf("buffered attempts=%d", n)
	}
}

func TestChatStreamAPIKeyAndRegion(t *testing.T) {
	f := newFake()
	f.on("runtime.eu-central-1.kiro.dev/", streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "hi"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	f.on("q.eu-central-1.amazonaws.com"+pathGen, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "eu"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	key := Cred{UID: "kk", AccessToken: "ksk_secret", APIKey: "ksk_secret", AuthMethod: "api_key", Region: "eu-central-1",
		MachineID: MachineIDFromAPIKey("ksk_secret"), ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/IGNORED"}
	rc, err := c.ChatStream(context.Background(), key, []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, rc)
	calls := f.calls("runtime.eu-central-1.kiro.dev/")
	if len(calls) != 1 {
		t.Fatalf("api key must use the CLI runtime: %+v", f.requests)
	}
	h := calls[0].Header
	if h.Get("tokentype") != "API_KEY" || h.Get("Authorization") != "Bearer ksk_secret" ||
		h.Get("Content-Type") != "application/x-amz-json-1.0" || h.Get("x-amzn-codewhisperer-optout") != "false" {
		t.Errorf("api key headers: %v", h)
	}
	if strings.Contains(string(calls[0].Body), "profileArn") || !strings.Contains(string(calls[0].Body), `"origin":"KIRO_CLI"`) {
		t.Errorf("api key payload: %s", calls[0].Body)
	}
	// OAuth 账号的 profile 在 eu-central-1 → 推理打到 q.eu-central-1。
	eu := testCred
	eu.ProfileArn = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/EU"
	rc, err = c.ChatStream(context.Background(), eu, []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, rc), `"eu"`) {
		t.Error("eu profile must be routed to q.eu-central-1")
	}
}

func TestChatStreamToolNameRestored(t *testing.T) {
	long := "mcp__" + strings.Repeat("s", 40) + "__" + strings.Repeat("t", 40)
	short := shortenToolName(long)
	f := newFake()
	f.on(routeIDE, streamResp(
		evt("toolUseEvent", map[string]any{"toolUseId": "x1", "name": short, "input": `{}`, "stop": true}),
		evt("metadataEvent", map[string]any{"stopReason": "tool_use"}),
	))
	c := testClient(f)
	body := `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"` + long + `"}}]}`
	rc, err := c.ChatStream(context.Background(), testCred, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := sseFrames(t, readAll(t, rc))
	got := collect(frames)
	if len(got.tools) != 1 || got.tools[0]["function"].(map[string]any)["name"] != long {
		t.Fatalf("tool name must be restored to the client's original: %+v", got.tools)
	}
}

func TestChatStreamBadRequestBody(t *testing.T) {
	c := testClient(newFake())
	if _, err := c.ChatStream(context.Background(), testCred, []byte(`not json`)); err == nil || err.Kind != KindBadRequest {
		t.Fatalf("bad json: %+v", err)
	}
	if _, err := c.ChatStream(context.Background(), testCred, []byte(`{"model":"x","messages":[]}`)); err == nil || err.Kind != KindBadRequest {
		t.Fatalf("empty messages: %+v", err)
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// ── 刷新 / 登录 / 额度 ─────────────────────────────────────────────────────

func TestRefresh(t *testing.T) {
	f := newFake()
	f.on("prod.us-east-1.auth.desktop.kiro.dev/refreshToken", func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		if !strings.Contains(string(body), `"refreshToken":"rt-1"`) {
			return 400, nil, []byte(`{"message":"bad"}`)
		}
		return 200, nil, []byte(`{"accessToken":"at-2","refreshToken":"rt-2","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/NEW"}`)
	})
	f.on("oidc.eu-central-1.amazonaws.com/token", func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		if m["clientId"] != "cid" || m["clientSecret"] != "cs" || m["grantType"] != "refresh_token" {
			return 400, nil, []byte(`{"error":"invalid_request"}`)
		}
		if m["refreshToken"] == "dead" {
			return 400, nil, []byte(`{"error":"invalid_grant","error_description":"Invalid refresh token provided"}`)
		}
		return 200, nil, []byte(`{"accessToken":"at-i","expiresIn":28800}`)
	})
	c := testClient(f)
	ts, err := c.Refresh(context.Background(), testCred)
	if err != nil || ts.AccessToken != "at-2" || ts.RefreshToken != "rt-2" || ts.ProfileArn == "" || ts.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("social refresh: %+v %v", ts, err)
	}
	idc := Cred{UID: "i", AuthMethod: "idc", RefreshToken: "r", ClientID: "cid", ClientSecret: "cs", Region: "eu-central-1"}
	ts, err = c.Refresh(context.Background(), idc)
	if err != nil || ts.AccessToken != "at-i" || ts.RefreshToken != "r" {
		t.Fatalf("idc refresh must keep the old refresh token when none is returned: %+v %v", ts, err)
	}
	idc.RefreshToken = "dead"
	if _, err = c.Refresh(context.Background(), idc); err == nil || err.Kind != KindSessionDead {
		t.Fatalf("invalid_grant must be session dead: %+v", err)
	}
	if _, err = c.Refresh(context.Background(), Cred{AuthMethod: "idc", RefreshToken: "x"}); err == nil || err.Kind != KindSessionDead {
		t.Fatalf("idc without client must need re-login: %+v", err)
	}
	if _, err = c.Refresh(context.Background(), Cred{APIKey: "ksk_x"}); err == nil {
		t.Fatal("api key must not refresh")
	}
}

func TestDeviceLogin(t *testing.T) {
	f := newFake()
	polls := 0
	f.on("oidc.us-east-1.amazonaws.com/client/register", func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		if !strings.Contains(string(body), BuilderIDStartURL) || !strings.Contains(string(body), "device_code") {
			return 400, nil, []byte(`{"error":"bad"}`)
		}
		return 200, nil, []byte(`{"clientId":"cid","clientSecret":"cs"}`)
	})
	f.on("oidc.us-east-1.amazonaws.com/device_authorization", errResp(200,
		`{"deviceCode":"dc","userCode":"ABCD-EFGH","verificationUri":"https://device.sso.us-east-1.amazonaws.com/","verificationUriComplete":"https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH","interval":1,"expiresIn":600}`))
	f.on("oidc.us-east-1.amazonaws.com/token", func(*http.Request, []byte) (int, http.Header, []byte) {
		polls++
		if polls == 1 {
			return 400, nil, []byte(`{"error":"authorization_pending"}`)
		}
		return 200, nil, []byte(`{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`)
	})
	c := testClient(f)
	s, err := c.StartDeviceLogin(context.Background(), "", "")
	if err != nil || s.UserCode != "ABCD-EFGH" || s.Provider != "BuilderId" || !strings.Contains(s.VerificationURI, "user_code") {
		t.Fatalf("start: %+v %v", s, err)
	}
	if r := c.PollDeviceLogin(context.Background(), s); r.Status != "pending" {
		t.Fatalf("first poll: %+v", r)
	}
	r := c.PollDeviceLogin(context.Background(), s)
	if r.Status != "ok" || r.Tokens.AccessToken != "at" || r.Tokens.RefreshToken != "rt" {
		t.Fatalf("second poll: %+v", r)
	}
	if _, err := c.StartDeviceLogin(context.Background(), "http://evil", "us-east-1"); err == nil {
		t.Fatal("non-https start url must be rejected")
	}
	if _, err := c.StartDeviceLogin(context.Background(), "https://my.awsapps.com/start", "us-east-1.evil.com"); err == nil {
		t.Fatal("invalid region must be rejected")
	}
}

func TestUsageLimits(t *testing.T) {
	f := newFake()
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		if r.URL.Query().Get("profileArn") != testCred.ProfileArn {
			return 400, nil, []byte(`{"message":"missing profile"}`)
		}
		return 200, nil, []byte(`{"nextDateReset":1893456000,
			"subscriptionInfo":{"subscriptionTitle":"KIRO PRO","subscriptionType":"Q_DEVELOPER_STANDALONE_PRO"},
			"userInfo":{"email":"alice@example.com","userId":"uid-1"},
			"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":12,"usageLimit":1000,"currentUsageWithPrecision":12.5,
				"freeTrialInfo":{"currentUsage":100,"usageLimit":500,"freeTrialStatus":"ACTIVE","freeTrialExpiry":1893456000},
				"bonuses":[{"displayName":"Welcome","currentUsage":0,"usageLimit":50,"status":"ACTIVE","expiresAt":1893456000},
				           {"displayName":"Old","currentUsage":0,"usageLimit":999,"status":"EXPIRED"}]}]}`)
	})
	c := testClient(f)
	u, err := c.UsageLimits(context.Background(), testCred)
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "PRO" || u.Email != "alice@example.com" || u.UserID != "uid-1" || u.Current != 12.5 || u.Limit != 1000 || u.NextResetAt != 1893456000 {
		t.Fatalf("usage: %+v", u)
	}
	remain, total := u.Totals(time.Unix(1800000000, 0))
	if remain != 987.5+400+50 || total != 1550 {
		t.Fatalf("totals: %v / %v", remain, total)
	}
}

// ── 凭证导入 ───────────────────────────────────────────────────────────────

func TestParseImport(t *testing.T) {
	// Kiro IDE：token 文件 + 配对的 OIDC 客户端文件。
	items, skipped := ParseImport(`[
		{"accessToken":"at","refreshToken":"rt-idc","expiresAt":"2030-01-02T03:04:05.000Z","authMethod":"IdC","provider":"BuilderId","clientIdHash":"abc","region":"us-east-1"},
		{"clientId":"cid","clientSecret":"cs","expiresAt":"2030-01-01T00:00:00Z"},
		{"credentials":{"refreshToken":"rt-social","authMethod":"social"},"email":"s@example.com","provider":"Github"},
		{"kiroApiKey":"ksk_abc|eu-central-1","nickname":"cli"},
		{"refreshToken":"rt-ms","authMethod":"external_idp"},
		{"email":"nope@example.com"}
	]`)
	if len(items) != 3 || len(skipped) != 2 {
		t.Fatalf("items=%+v skipped=%v", items, skipped)
	}
	if it := items[0]; it.AuthMethod != "idc" || it.ClientID != "cid" || it.ClientSecret != "cs" || it.Provider != "BuilderId" || it.ExpiresAt == 0 {
		t.Errorf("idc pairing: %+v", it)
	}
	if it := items[1]; it.AuthMethod != "social" || it.RefreshToken != "rt-social" || it.Provider != "Github" || it.Email != "s@example.com" {
		t.Errorf("account manager record: %+v", it)
	}
	if it := items[2]; it.AuthMethod != "api_key" || it.APIKey != "ksk_abc" || it.Region != "eu-central-1" {
		t.Errorf("api key: %+v", it)
	}
	// 行格式：密码列丢弃；单独 refresh token；单独 API Key；不完整行跳过。
	items, skipped = ParseImport("a@x.com----p@ss----rt-line----cid----cs\n" +
		strings.Repeat("r", 40) + "\nksk_line\nbad line\n")
	if len(items) != 3 || len(skipped) != 1 {
		t.Fatalf("lines: items=%+v skipped=%v", items, skipped)
	}
	if it := items[0]; it.Email != "a@x.com" || it.RefreshToken != "rt-line" || it.AuthMethod != "idc" {
		t.Errorf("line format: %+v", it)
	}
	for _, it := range items {
		for _, v := range []string{it.RefreshToken, it.ClientID, it.ClientSecret, it.Email, it.Nickname, it.APIKey} {
			if v == "p@ss" {
				t.Fatal("password column must never be kept")
			}
		}
	}
	if items[1].AuthMethod != "social" || items[2].AuthMethod != "api_key" {
		t.Errorf("bare lines: %+v", items[1:])
	}
	if _, skipped := ParseImport(`{bad json`); len(skipped) != 1 {
		t.Error("bad json must be reported")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{403, `{"reason":"TEMPORARILY_SUSPENDED"}`, KindSuspended},
		{429, `{"message":"Too many requests"}`, KindRateLimit},
		{0, `ThrottlingException: Rate exceeded`, KindRateLimit},
		{400, `{"message":"Improperly formed request."}`, KindBadRequest},
		{400, `{"message":"Invalid model. Please select a different model to continue.","reason":"INVALID_MODEL_ID"}`, KindModelUnavailable},
		{500, `{"message":"boom"}`, KindServer},
		{0, `ServiceUnavailableException: INSUFFICIENT_MODEL_CAPACITY`, KindBusy},
		{402, `{}`, KindQuota},
	}
	for _, c := range cases {
		if got := classify(c.status, c.body); got != c.want {
			t.Errorf("classify(%d, %s) = %s want %s", c.status, c.body, got, c.want)
		}
	}
	if classifyRefresh(400, `{"error":"invalid_grant"}`) != KindSessionDead || classifyRefresh(500, "x") != KindServer {
		t.Error("classifyRefresh")
	}
}

func TestStableUIDAndProfileArn(t *testing.T) {
	a, b := StableUID("uid-1"), StableUID("uid-1")
	if a != b || len(a) != 16 || a[0] != 'k' || StableUID("uid-2") == a {
		t.Fatalf("StableUID: %s %s", a, b)
	}
	if _, r, ok := ParseProfileArn("arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABC"); !ok || r != "eu-central-1" {
		t.Error("valid arn rejected")
	}
	for _, bad := range []string{"", "arn:aws:s3:::x", "arn:aws:codewhisperer:evil.com:123456789012:profile/A", "arn:aws:codewhisperer:us-east-1:12:profile/A"} {
		if _, _, ok := ParseProfileArn(bad); ok {
			t.Errorf("bad arn accepted: %q", bad)
		}
	}
	if regionalize("https://q.us-east-1.amazonaws.com/x", "eu-central-1") != "https://q.eu-central-1.amazonaws.com/x" ||
		regionalize("https://codewhisperer.us-east-1.amazonaws.com/x", "eu-central-1") != "https://q.eu-central-1.amazonaws.com/x" ||
		regionalize("https://q.us-east-1.amazonaws.com/x", "evil.com/") != "https://q.us-east-1.amazonaws.com/x" {
		t.Error("regionalize")
	}
}
