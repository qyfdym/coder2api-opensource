package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

type anthropicEvent struct {
	name string
	data map[string]any
}

// parseAnthropicSSE 把 Anthropic SSE 拆成事件；event 行与 data.type 必须一致。
func parseAnthropicSSE(t *testing.T, body string) []anthropicEvent {
	t.Helper()
	var out []anthropicEvent
	var name string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var d map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
			if d["type"] != name {
				t.Fatalf("event %q carries data.type %v", name, d["type"])
			}
			out = append(out, anthropicEvent{name, d})
		}
	}
	return out
}

func postAnthropic(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// 流式：思考 → 正文 → 工具调用，事件顺序与字段符合 Anthropic 协议；裸 claude 名（带 [1m] 后缀、横线版本号）
// 路由到 Kiro；思考与工具声明都传到了上游。
func TestAnthropicMessagesStream(t *testing.T) {
	up := &kiroUpstream{stream: bytes.Join([][]byte{
		kiroESFrame("reasoningContentEvent", map[string]any{"text": "let me think"}),
		kiroESFrame("assistantResponseEvent", map[string]any{"content": "Checking the weather now."}),
		kiroESFrame("toolUseEvent", map[string]any{"toolUseId": "tu1", "name": "get_weather", "input": `{"city":"SF"}`, "stop": true}),
		kiroESFrame("metadataEvent", map[string]any{"stopReason": "tool_use"}),
	}, nil)}
	h, _, rl := newKiroHandler(t, nil, up, nil, kiroAcct("k1"))

	rec := postAnthropic(h, "/v1/messages", `{
		"model":"claude-sonnet-4-5[1m]","max_tokens":1024,"stream":true,
		"thinking":{"type":"enabled","budget_tokens":8000},
		"system":[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"weather in SF?"}]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type=%q", ct)
	}
	evs := parseAnthropicSSE(t, rec.Body.String())
	var seq []string
	for _, e := range evs {
		s := e.name
		if e.name == "content_block_start" {
			s += ":" + e.data["content_block"].(map[string]any)["type"].(string)
		}
		if e.name == "content_block_delta" {
			s += ":" + e.data["delta"].(map[string]any)["type"].(string)
		}
		if len(seq) == 0 || seq[len(seq)-1] != s {
			seq = append(seq, s)
		}
	}
	want := []string{
		"message_start", "ping",
		"content_block_start:thinking", "content_block_delta:thinking_delta", "content_block_delta:signature_delta", "content_block_stop",
		"content_block_start:text", "content_block_delta:text_delta", "content_block_stop",
		"content_block_start:tool_use", "content_block_delta:input_json_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v\nbody=%s", seq, want, rec.Body.String())
	}
	start := evs[0].data["message"].(map[string]any)
	if start["model"] != "claude-sonnet-4-5[1m]" || !strings.HasPrefix(start["id"].(string), "msg_") {
		t.Errorf("message_start: %v", start)
	}
	var thinking, text, toolJSON string
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		switch d["type"] {
		case "thinking_delta":
			thinking += d["thinking"].(string)
		case "text_delta":
			text += d["text"].(string)
		case "input_json_delta":
			toolJSON += d["partial_json"].(string)
		}
	}
	if thinking != "let me think" || text != "Checking the weather now." || toolJSON != `{"city":"SF"}` {
		t.Errorf("thinking=%q text=%q tool=%q", thinking, text, toolJSON)
	}
	// 块序号连续。
	idx := 0
	for _, e := range evs {
		if e.name == "content_block_start" {
			if int(e.data["index"].(float64)) != idx {
				t.Errorf("block index %v, want %d", e.data["index"], idx)
			}
			idx++
		}
	}
	for _, e := range evs {
		if e.name == "content_block_start" && e.data["content_block"].(map[string]any)["type"] == "tool_use" {
			cb := e.data["content_block"].(map[string]any)
			if cb["id"] != "tu1" || cb["name"] != "get_weather" {
				t.Errorf("tool_use block: %v", cb)
			}
		}
		if e.name == "message_delta" {
			if e.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
				t.Errorf("stop_reason: %v", e.data["delta"])
			}
			u := e.data["usage"].(map[string]any)
			if u["output_tokens"].(float64) <= 0 || u["input_tokens"].(float64) <= 0 {
				t.Errorf("usage: %v", u)
			}
		}
	}
	ob := up.lastBody()
	for _, s := range []string{`"modelId":"claude-sonnet-4.5"`, "thinking_mode", "get_weather", "You are helpful."} {
		if !strings.Contains(ob, s) {
			t.Errorf("outbound Kiro body lacks %q: %.600s", s, ob)
		}
	}
	if e := lastEvent(t, rl); !e.OK || e.Path != "/v1/messages" {
		t.Errorf("request log: ok=%v path=%q", e.OK, e.Path)
	}
}

// 非流式：日期快照名路由到 Kiro；响应是 Anthropic message；工具结果按 tool_use_id 回传到上游。
func TestAnthropicMessagesNonStreamToolResult(t *testing.T) {
	up := &kiroUpstream{stream: bytes.Join([][]byte{
		kiroESFrame("assistantResponseEvent", map[string]any{"content": "It is sunny."}),
		kiroESFrame("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	}, nil)}
	h, _, _ := newKiroHandler(t, nil, up, nil, kiroAcct("k1"))

	rec := postAnthropic(h, "/v1/messages", `{
		"model":"claude-sonnet-4-5-20250929","max_tokens":512,
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"weather in SF?"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"x"},{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"SF"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"72F sunny"}]}]}
		]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var msg struct {
		ID         string           `json:"id"`
		Type       string           `json:"type"`
		Role       string           `json:"role"`
		Model      string           `json:"model"`
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "claude-sonnet-4-5-20250929" || msg.StopReason != "end_turn" {
		t.Errorf("message: %s", rec.Body.String())
	}
	if len(msg.Content) != 1 || msg.Content[0]["type"] != "text" || msg.Content[0]["text"] != "It is sunny." {
		t.Errorf("content: %v", msg.Content)
	}
	if msg.Usage["input_tokens"] == nil || msg.Usage["output_tokens"] == nil {
		t.Errorf("usage: %v", msg.Usage)
	}
	ob := up.lastBody()
	for _, s := range []string{`"modelId":"claude-sonnet-4.5"`, `"toolUseId":"tu1"`, "72F sunny"} {
		if !strings.Contains(ob, s) {
			t.Errorf("outbound Kiro body lacks %q: %.800s", s, ob)
		}
	}
	if strings.Contains(ob, "hmm") {
		t.Error("thinking blocks from history must not be forwarded")
	}
}

// 非流式工具调用：stop_reason=tool_use，input 是对象。
func TestAnthropicMessagesNonStreamToolUse(t *testing.T) {
	up := &kiroUpstream{stream: bytes.Join([][]byte{
		kiroESFrame("toolUseEvent", map[string]any{"toolUseId": "tu9", "name": "read", "input": `{"path":"a.go"}`, "stop": true}),
		kiroESFrame("metadataEvent", map[string]any{"stopReason": "tool_use"}),
	}, nil)}
	h, _, _ := newKiroHandler(t, nil, up, nil, kiroAcct("k1"))
	rec := postAnthropic(h, "/v1/messages", `{"model":"claude-opus-4-6","max_tokens":100,
		"tools":[{"name":"read","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"read a.go"}]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var msg struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.StopReason != "tool_use" || len(msg.Content) != 1 || msg.Content[0]["type"] != "tool_use" ||
		msg.Content[0]["id"] != "tu9" || msg.Content[0]["input"].(map[string]any)["path"] != "a.go" {
		t.Fatalf("response: %s", rec.Body.String())
	}
}

// 开流前的错误：OpenAI 错误 → Anthropic 错误形态，状态码保留。
func TestAnthropicMessagesErrors(t *testing.T) {
	fake := &kiroChatFake{errOf: map[string]*kiro.Error{"k1": {Kind: kiro.KindBadRequest, Status: 400, Msg: "Improperly formed request"}}}
	h, _, _ := newKiroHandler(t, nil, nil, fake, kiroAcct("k1"))
	for _, stream := range []string{"true", "false"} {
		rec := postAnthropic(h, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":10,"stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if rec.Code != 400 {
			t.Fatalf("stream=%s status=%d body=%s", stream, rec.Code, rec.Body.String())
		}
		var e struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Type != "error" || e.Error.Type != "invalid_request_error" ||
			!strings.Contains(e.Error.Message, "Improperly formed request") {
			t.Errorf("stream=%s error body: %s", stream, rec.Body.String())
		}
	}
	// 请求本身不合法：本地 400，不打上游。
	for _, body := range []string{`{"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, `{"model":"claude-sonnet-4-5","messages":[]}`, `not json`} {
		rec := postAnthropic(h, "/v1/messages", body, nil)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid_request_error"`) {
			t.Errorf("body %q: status=%d %s", body, rec.Code, rec.Body.String())
		}
	}
	if n := len(fake.called()); n != 2 {
		t.Errorf("upstream calls=%d, want 2 (malformed requests stay local)", n)
	}
}

// 流中途的上游错误帧 → Anthropic error 事件（之后不再发 message_stop）。
func TestAnthropicStreamErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	aw := newAnthropicWriter(rec, true, "claude-x", 10)
	aw.Header().Set("Content-Type", "text/event-stream")
	_, _ = aw.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\ndata: {\"error\":{\"message\":\"Too many requests\",\"type\":\"kiro_rate_limit\"}}\n\ndata: [DONE]\n\n"))
	aw.finish(httptest.NewRequest("GET", "/", nil).Context())
	evs := parseAnthropicSSE(t, rec.Body.String())
	last := evs[len(evs)-1]
	if last.name != "error" || last.data["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Fatalf("last event: %+v\n%s", last, rec.Body.String())
	}
	for _, e := range evs {
		if e.name == "message_stop" {
			t.Error("message_stop must not follow an error event")
		}
	}
}

// x-api-key 鉴权；失败时 Anthropic 客户端拿到 Anthropic 形态的 401，OpenAI 客户端形态不变。
func TestAnthropicAPIKeyAuth(t *testing.T) {
	svc := &kiroacct.Service{Client: kiro.NewClient(kiro.Options{Transport: offlineKiroTransport(), Logf: t.Logf}), AuthDir: t.TempDir()}
	fake := &kiroChatFake{}
	svc.ChatFunc = fake.chat
	h := NewHandler(Config{
		Pool: testPoolWith(kiroAcct("k1")), Upstream: noWBUpstream(t), Kiro: svc,
		SoftCooldown: time.Minute, RequestLog: reqlog.New(reqlog.Config{}),
		Live: livecfg.New(livecfg.Snapshot{APIKey: "secret"}),
	})
	body := `{"model":"claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	if rec := postAnthropic(h, "/v1/messages", body, map[string]string{"x-api-key": "secret"}); rec.Code != 200 {
		t.Fatalf("x-api-key: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := postAnthropic(h, "/v1/messages", body, map[string]string{"x-api-key": "wrong"})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"authentication_error"`) {
		t.Errorf("bad x-api-key: status=%d body=%s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"invalid_api_key"`) {
		t.Errorf("openai client 401 shape changed: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("x-api-key", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"has_more"`) {
		t.Errorf("x-api-key /v1/models: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	h, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{}, kiroAcct("k1"))
	rec := postAnthropic(h, "/v1/messages/count_tokens", `{"model":"claude-sonnet-4-5","system":"be brief","messages":[{"role":"user","content":"hello there, how are you today?"}]}`, nil)
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.InputTokens <= 0 {
		t.Fatalf("count_tokens: %d %s", rec.Code, rec.Body.String())
	}
}

// Anthropic 形态的模型列表：Kiro 模型用裸名、Claude 用横线版本号；单个模型按多种写法都能查到。
func TestAnthropicModels(t *testing.T) {
	h, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{}, kiroAcct("k1"))
	get := func(path string, anthropic bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/v1/models", true)
	var list struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
		FirstID string           `json:"first_id"`
		LastID  string           `json:"last_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Data) == 0 || list.HasMore {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, m := range list.Data {
		if m["type"] != "model" || m["display_name"] == nil || m["created_at"] == nil {
			t.Errorf("item: %v", m)
		}
		if m["id"] == "claude-sonnet-4-5" {
			found = true
		}
		if strings.HasPrefix(m["id"].(string), "kiro:") {
			t.Errorf("kiro ids must be bare for Anthropic clients: %v", m["id"])
		}
	}
	if !found || list.FirstID != list.Data[0]["id"] || list.LastID != list.Data[len(list.Data)-1]["id"] {
		t.Errorf("list: %s", rec.Body.String())
	}
	rec = get("/v1/models?limit=1", true)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 1 || !list.HasMore {
		t.Errorf("limit=1: %s", rec.Body.String())
	}
	// OpenAI 客户端的形态不变。
	if rec = get("/v1/models", false); !strings.Contains(rec.Body.String(), `"object":"list"`) || !strings.Contains(rec.Body.String(), `"kiro:claude-sonnet-4.5"`) {
		t.Errorf("openai list: %s", rec.Body.String())
	}
	for _, id := range []string{"claude-sonnet-4-5", "claude-sonnet-4.5", "claude-sonnet-4-5-20250929"} {
		rec = get("/v1/models/"+id, true)
		var m map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m["id"] != id || m["type"] != "model" {
			t.Errorf("model %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if rec = get("/v1/models/no-such-model", true); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_found_error") {
		t.Errorf("missing model: %d %s", rec.Code, rec.Body.String())
	}
	if rec = get("/v1/models/kiro:claude-sonnet-4.5", false); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"kiro:claude-sonnet-4.5"`) {
		t.Errorf("openai single model: %d %s", rec.Code, rec.Body.String())
	}
}

// 模型路由：没有 Kiro 账号时裸名保持原样（走 WorkBuddy）；显式前缀不动。
func TestAnthropicRouteModel(t *testing.T) {
	h, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{}, kiroAcct("k1"))
	for in, want := range map[string]string{
		"claude-sonnet-4-5":         "kiro:claude-sonnet-4-5",
		"claude-sonnet-5-5[1m]":     "kiro:claude-sonnet-5-5",
		"anthropic/claude-opus-4.6": "kiro:claude-opus-4.6",
		"cn:glm-5.2":                "cn:glm-5.2",
		"qoder:Qwen3.8-Max":         "qoder:Qwen3.8-Max",
		"glm-5.2":                   "glm-5.2",
	} {
		if got := h.anthropicRouteModel(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	noKiro, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{})
	if got := noKiro.anthropicRouteModel("claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Errorf("without kiro accounts: %s", got)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	req := &anthropicRequest{}
	raw := `{
		"model":"m","max_tokens":100,"stream":true,"temperature":0.5,"stop_sequences":["END"],
		"system":[{"type":"text","text":"sys A"},{"type":"text","text":"sys B"}],
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"metadata":{"user_id":"user_abc_account_def_session_0f9e8d7c-1234-5678"},
		"tool_choice":{"type":"any","disable_parallel_tool_use":true},
		"tools":[{"name":"t1","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}]},
			{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"c1","name":"t1","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"boom"},{"type":"text","text":"and?"}]}
		]}`
	if err := json.Unmarshal([]byte(raw), req); err != nil {
		t.Fatal(err)
	}
	body, err := anthropicToOpenAI(req, "kiro:m")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["model"] != "kiro:m" || out["stream"] != true || out["max_tokens"] != 100.0 || out["temperature"] != 0.5 {
		t.Errorf("scalars: %s", body)
	}
	if so, _ := out["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options: %v", out["stream_options"])
	}
	if out["tool_choice"] != "required" || out["parallel_tool_calls"] != false || out["reasoning_effort"] != "high" {
		t.Errorf("tool_choice / parallel / effort: %s", body)
	}
	if th, _ := out["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking: %v", out["thinking"])
	}
	if md, _ := out["metadata"].(map[string]any); md["conversation_id"] != "0f9e8d7c-1234-5678" {
		t.Errorf("metadata: %v", out["metadata"])
	}
	if tools, _ := out["tools"].([]any); len(tools) != 1 {
		t.Errorf("server tools must be dropped: %v", out["tools"])
	}
	msgs := out["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles: %v (%s)", roles, body)
	}
	if msgs[0].(map[string]any)["content"] != "sys A\n\nsys B" {
		t.Errorf("system: %v", msgs[0])
	}
	parts, _ := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/jpeg;base64,QUJD" {
		t.Errorf("image: %v", msgs[1])
	}
	asst := msgs[2].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if asst["content"] != "calling" || len(calls) != 1 || calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Errorf("assistant: %v", asst)
	}
	tool := msgs[3].(map[string]any)
	if tool["tool_call_id"] != "c1" || tool["content"] != "Error: boom" {
		t.Errorf("tool: %v", tool)
	}
	if msgs[4].(map[string]any)["content"] != "and?" {
		t.Errorf("trailing user text: %v", msgs[4])
	}

	// 新写法 user_id（JSON 字符串里的 session_id）。
	if sid := sessionFromMetadata(json.RawMessage(`{"user_id":"{\"device_id\":\"d\",\"session_id\":\"s-123\"}"}`)); sid != "s-123" {
		t.Errorf("json user_id session: %q", sid)
	}
	// 非法角色。
	bad := &anthropicRequest{Messages: []anthropicMessage{{Role: "tool", Content: json.RawMessage(`"x"`)}}}
	if _, err := anthropicToOpenAI(bad, "m"); err == nil {
		t.Error("unknown roles must be rejected")
	}
}

// 对话中途的系统消息（Claude Code 2.1.154+）：文本并进前一条 user 消息的 <system-reminder>；只在本轮
// 生效且已过期的丢弃；只调档位的指令覆盖 output_config。
func TestAnthropicMidConversationSystem(t *testing.T) {
	convert := func(raw string) []map[string]any {
		t.Helper()
		var req anthropicRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		body, err := anthropicToOpenAI(&req, "m")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		var out struct {
			Messages        []map[string]any `json:"messages"`
			ReasoningEffort string           `json:"reasoning_effort"`
		}
		_ = json.Unmarshal(body, &out)
		if out.ReasoningEffort != "" {
			out.Messages = append(out.Messages, map[string]any{"role": "_effort", "content": out.ReasoningEffort})
		}
		return out.Messages
	}

	// Claude Code 实际发的形态：user 后面跟一条 system（技能列表）。
	msgs := convert(`{"model":"m","system":"top","messages":[
		{"role":"user","content":"你好"},
		{"role":"system","content":"The following skills are available for use with the Skill tool: ..."}]}`)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("messages: %v", msgs)
	}
	if c := msgs[1]["content"].(string); !strings.HasPrefix(c, "你好\n\n<system-reminder>\nThe following skills") || !strings.HasSuffix(c, "</system-reminder>") {
		t.Errorf("merged user content: %q", c)
	}

	// 工具结果之后：单独起一条 user 消息；过期的轮次提醒丢弃；块数组里只取文本。
	msgs = convert(`{"model":"m","output_config":{"effort":"high"},"messages":[
		{"role":"user","content":"run it"},
		{"role":"system","content":"stale reminder","clear_at":"next_user_message"},
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]},
		{"role":"system","content":[{"type":"text","text":"fresh reminder"},{"type":"tool_removal","tool":{"type":"tool_reference","name":"bash"}}],"clear_at":"next_user_message"},
		{"role":"system","content":[],"output_config":{"effort":"low"}}]}`)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user,_effort" {
		t.Fatalf("roles: %v (%v)", roles, msgs)
	}
	if msgs[0]["content"] != "run it" {
		t.Errorf("stale turn-scoped reminder must be dropped: %v", msgs[0])
	}
	if msgs[3]["content"] != "<system-reminder>\nfresh reminder\n</system-reminder>" {
		t.Errorf("reminder after tool results: %v", msgs[3])
	}
	if msgs[4]["content"] != "low" {
		t.Errorf("directive effort must override output_config: %v", msgs[4])
	}
}

// 端到端：带对话中途系统消息的请求正常走到 Kiro；本地拒绝时请求记录写上模型与原因。
func TestAnthropicMessagesSystemRoleEndToEnd(t *testing.T) {
	up := &kiroUpstream{stream: bytes.Join([][]byte{
		kiroESFrame("assistantResponseEvent", map[string]any{"content": "你好！"}),
		kiroESFrame("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	}, nil)}
	h, _, rl := newKiroHandler(t, nil, up, nil, kiroAcct("k1"))
	rec := postAnthropic(h, "/v1/messages", `{"model":"claude-sonnet-5-5","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"你好"},{"role":"system","content":"skills: pdf, xlsx"}]}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ob := up.lastBody(); !strings.Contains(ob, "skills: pdf, xlsx") || !strings.Contains(ob, `"modelId":"claude-sonnet-5.5"`) {
		t.Errorf("outbound Kiro body: %.600s", ob)
	}

	rec = postAnthropic(h, "/v1/messages", `{"model":"claude-sonnet-5-5","max_tokens":100,
		"messages":[{"role":"user","content":"hi"},{"role":"narrator","content":"x"}]}`, nil)
	if rec.Code != 400 {
		t.Fatalf("unknown role: status=%d", rec.Code)
	}
	if e := lastEvent(t, rl); e.Model != "kiro:claude-sonnet-5-5" || !strings.Contains(e.Note, "narrator") {
		t.Errorf("request log: model=%q note=%q", e.Model, e.Note)
	}
}
