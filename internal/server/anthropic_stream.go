package server

// anthropic_stream.go chatCompletions 的输出（OpenAI 形态）→ Anthropic Messages 形态。
//
// anthropicWriter 套在 chatCompletions 外面当 ResponseWriter：
//   - 流式成功（200 + text/event-stream）：边收 OpenAI chunk 边译成 Anthropic 事件（anthropicStream）；
//   - 其余（非流式 JSON、开流前的错误）：先缓冲，finish 时整体翻译。
//
// 工具调用在流尾一次性发出（content_block_start + 一个 input_json_delta + stop）：OpenAI 的
// 参数分片可能交错到达，Anthropic 的块关了就不能再写，边收边发会把参数切坏。正文与思考照常实时流出。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// anthropicSkipHeaders 内层设置、但不该原样转到真实响应上的头（内容形态由本层决定）。
var anthropicSkipHeaders = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Cache-Control": true,
	"Connection": true, "X-Accel-Buffering": true,
}

func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

type anthropicWriter struct {
	rw         http.ResponseWriter
	header     http.Header
	status     int
	wantStream bool
	live       bool
	buf        bytes.Buffer
	conv       *anthropicStream
}

func newAnthropicWriter(rw http.ResponseWriter, wantStream bool, model string, estIn int) *anthropicWriter {
	return &anthropicWriter{
		rw: rw, header: http.Header{}, wantStream: wantStream,
		conv: newAnthropicStream(rw, model, estIn),
	}
}

func (a *anthropicWriter) Header() http.Header { return a.header }

func (a *anthropicWriter) WriteHeader(code int) {
	if a.status != 0 {
		return
	}
	a.status = code
	if code == http.StatusOK && a.wantStream && strings.HasPrefix(a.header.Get("Content-Type"), "text/event-stream") {
		a.live = true
		a.copyHeaders()
		setSSEHeaders(a.rw.Header())
		a.rw.WriteHeader(http.StatusOK)
		a.conv.start()
	}
}

func (a *anthropicWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if a.live {
		if err := a.conv.feed(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return a.buf.Write(p)
}

// Flush 内层每帧都会 Flush；实时模式下 anthropicStream 每发一个事件已经自己 flush 过。
func (a *anthropicWriter) Flush() {}

func (a *anthropicWriter) copyHeaders() {
	dst := a.rw.Header()
	for k, v := range a.header {
		if !anthropicSkipHeaders[k] {
			dst[k] = v
		}
	}
}

// finish chatCompletions 返回后调用：收尾实时流，或把缓冲的响应整体翻译后写出。
func (a *anthropicWriter) finish(ctx context.Context) {
	if a.live {
		a.conv.finish()
		return
	}
	if a.status == 0 {
		if ctx.Err() != nil {
			return // 客户端已断开
		}
		writeMessagesError(a.rw, http.StatusBadGateway, "api_error", "gateway returned an empty response")
		return
	}
	a.copyHeaders()
	body := a.buf.Bytes()
	if a.status != http.StatusOK {
		status, typ, msg := anthropicErrorFromOpenAI(a.status, body)
		writeMessagesError(a.rw, status, typ, msg)
		return
	}
	res, err := parseOpenAIResult(a.header.Get("Content-Type"), body)
	if err != nil {
		var fe *frameError
		if errors.As(err, &fe) {
			writeMessagesError(a.rw, fe.status, fe.typ, fe.msg)
			return
		}
		writeMessagesError(a.rw, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if a.wantStream {
		setSSEHeaders(a.rw.Header())
		a.rw.WriteHeader(http.StatusOK)
		a.conv.replay(res)
		return
	}
	writeJSON(a.rw, http.StatusOK, a.conv.message(res))
}

// ---------------------------------------------------------------------------
// 结果与错误的形态转换
// ---------------------------------------------------------------------------

type anthropicToolCall struct {
	id, name string
	args     strings.Builder
}

// input 工具参数（JSON 对象）；上游给的不是合法 JSON 时退成空对象，不让客户端解析失败。
func (t *anthropicToolCall) input() json.RawMessage {
	s := strings.TrimSpace(t.args.String())
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

func (t *anthropicToolCall) toolID() string {
	if t.id == "" {
		t.id = "toolu_" + randomHex(12)
	}
	return t.id
}

// openAIResult 一次完整回答（非流式响应或聚合后的流）。
type openAIResult struct {
	reasoning string
	content   string
	tools     []*anthropicToolCall
	finish    string
	usage     map[string]any
}

// frameError 200 响应体里携带的上游错误（SSE error 帧）。
type frameError struct {
	status   int
	typ, msg string
}

func (e *frameError) Error() string { return e.msg }

// parseOpenAIResult 解析 200 响应体：chat.completion JSON，或（少见）一整段 SSE。
func parseOpenAIResult(contentType string, body []byte) (*openAIResult, error) {
	var resp map[string]any
	if strings.HasPrefix(contentType, "text/event-stream") {
		if ef := firstSSEErrorFrame(body); ef != "" {
			typ, msg := anthropicFrameError(ef)
			return nil, &frameError{status: http.StatusBadGateway, typ: typ, msg: msg}
		}
		agg, err := upstream.Aggregate(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		resp = agg
	} else if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unreadable upstream response: %v", err)
	}
	res := &openAIResult{finish: "stop"}
	res.usage, _ = resp["usage"].(map[string]any)
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return res, nil
	}
	ch, _ := chs[0].(map[string]any)
	if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
		res.finish = fr
	}
	msg, _ := ch["message"].(map[string]any)
	if msg == nil {
		return res, nil
	}
	res.reasoning, _ = msg["reasoning_content"].(string)
	switch c := msg["content"].(type) {
	case string:
		res.content = c
	case []any:
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				s, _ := pm["text"].(string)
				res.content += s
			}
		}
	}
	tcs, _ := msg["tool_calls"].([]any)
	for _, x := range tcs {
		tc, _ := x.(map[string]any)
		if tc == nil {
			continue
		}
		t := &anthropicToolCall{}
		t.id, _ = tc["id"].(string)
		if fn, ok := tc["function"].(map[string]any); ok {
			t.name, _ = fn["name"].(string)
			a, _ := fn["arguments"].(string)
			t.args.WriteString(a)
		}
		res.tools = append(res.tools, t)
	}
	return res, nil
}

func usageInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// anthropicUsage OpenAI usage → Anthropic usage。Anthropic 的 input_tokens 不含缓存命中部分。
func anthropicUsage(u map[string]any, estIn int) map[string]any {
	in, out := estIn, 0
	if v, ok := usageInt(u["prompt_tokens"]); ok {
		in = v
	}
	if v, ok := usageInt(u["completion_tokens"]); ok {
		out = v
	}
	res := map[string]any{"input_tokens": in, "output_tokens": out}
	if u != nil {
		if hit, _, ok := cacheTokensFromUsage(u); ok && hit > 0 && int(hit) <= in {
			res["input_tokens"] = in - int(hit)
			res["cache_read_input_tokens"] = int(hit)
		}
	}
	return res
}

func anthropicStopReason(finish string, hasTools bool) string {
	if hasTools {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// anthropicErrorType HTTP 状态 → Anthropic 错误类型。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// openAIErrorText OpenAI 错误对象 → 一行说明（message 原文 + gateway_hint）。
func openAIErrorText(e map[string]any) string {
	msg, _ := e["message"].(string)
	if msg == "" {
		if c, _ := e["code"].(string); c != "" {
			msg = c
		} else {
			msg = "upstream error"
		}
	}
	if hint, _ := e["gateway_hint"].(string); hint != "" {
		msg += " (" + hint + ")"
	}
	return msg
}

// anthropicErrorFromOpenAI 开流前的错误响应（OpenAI 错误 JSON）→ Anthropic 错误。状态码保留。
func anthropicErrorFromOpenAI(status int, body []byte) (int, string, string) {
	var obj struct {
		Error map[string]any `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &obj) == nil && obj.Error != nil {
		msg = openAIErrorText(obj.Error)
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return status, anthropicErrorType(status), msg
}

// anthropicFrameError 流里的 error 帧 → Anthropic 错误类型与说明（帧里没有 HTTP 状态，按类别词判）。
func anthropicFrameError(payload string) (string, string) {
	data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(payload), "data:"))
	var obj struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal([]byte(data), &obj) != nil || obj.Error == nil {
		return "api_error", "upstream stream error"
	}
	return frameErrorType(obj.Error), openAIErrorText(obj.Error)
}

func frameErrorType(e map[string]any) string {
	code := strings.ToLower(fmt.Sprint(e["type"], " ", e["code"]))
	switch {
	case strings.Contains(code, "rate") || strings.Contains(code, "429") || strings.Contains(code, "throttl"):
		return "rate_limit_error"
	case strings.Contains(code, "busy") || strings.Contains(code, "overload") || strings.Contains(code, "capacity"):
		return "overloaded_error"
	case strings.Contains(code, "bad_request") || strings.Contains(code, "too_large") ||
		strings.Contains(code, "invalid") || strings.Contains(code, "content"):
		return "invalid_request_error"
	}
	return "api_error"
}

// ---------------------------------------------------------------------------
// 流式翻译
// ---------------------------------------------------------------------------

// anthropicThinkingSignature 思考块的签名占位。上游不是 Anthropic，没有真签名；客户端把它原样
// 带回历史时，网关在 anthropicToOpenAI 里丢弃思考块，不会拿去验签。
const anthropicThinkingSignature = "coder2api"

type anthropicStream struct {
	w       http.ResponseWriter
	id      string
	model   string
	estIn   int
	started bool
	done    bool  // 已发 message_stop 或 error
	err     error // 写客户端失败
	pending []byte
	index   int    // 下一个内容块的序号
	open    string // 当前打开的块：""、"thinking"、"text"
	blocks  int    // 已发出的块数
	tools   []*anthropicToolCall
	toolAt  map[int]*anthropicToolCall
	finishR string
	usage   map[string]any
}

func newAnthropicStream(w http.ResponseWriter, model string, estIn int) *anthropicStream {
	return &anthropicStream{
		w: w, id: "msg_" + randomHex(12), model: model, estIn: estIn,
		toolAt: map[int]*anthropicToolCall{}, finishR: "stop",
	}
}

func (s *anthropicStream) event(name string, data any) {
	if s.err != nil {
		return
	}
	raw, _ := json.Marshal(data)
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "+string(raw)+"\n\n"); err != nil {
		s.err = err
		return
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *anthropicStream) start() {
	if s.started {
		return
	}
	s.started = true
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.estIn, "output_tokens": 0},
		},
	})
	s.event("ping", map[string]any{"type": "ping"})
}

// feed 内层写出的 SSE 字节（可能半行），按行处理。
func (s *anthropicStream) feed(p []byte) error {
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(s.pending[:i]), "\r")
		s.pending = s.pending[i+1:]
		s.line(line)
		if s.err != nil {
			return s.err
		}
	}
	return s.err
}

func (s *anthropicStream) line(line string) {
	if s.done {
		return
	}
	switch {
	case strings.HasPrefix(line, ":"):
		s.event("ping", map[string]any{"type": "ping"})
	case strings.HasPrefix(line, "data:"):
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			return
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			return
		}
		s.chunk(chunk, payload)
	}
}

func (s *anthropicStream) chunk(c map[string]any, raw string) {
	if e, ok := c["error"]; ok && e != nil {
		typ, msg := anthropicFrameError(raw)
		s.fail(typ, msg)
		return
	}
	if u, ok := c["usage"].(map[string]any); ok {
		s.usage = u
	}
	chs, _ := c["choices"].([]any)
	for _, ci := range chs {
		ch, _ := ci.(map[string]any)
		if ch == nil {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			s.finishR = fr
		}
		d, _ := ch["delta"].(map[string]any)
		if d == nil {
			d, _ = ch["message"].(map[string]any)
		}
		if d == nil {
			continue
		}
		if t, ok := d["reasoning_content"].(string); ok {
			s.thinking(t)
		}
		if t, ok := d["content"].(string); ok {
			s.text(t)
		}
		if tcs, ok := d["tool_calls"].([]any); ok {
			s.mergeTools(tcs)
		}
	}
}

func (s *anthropicStream) mergeTools(tcs []any) {
	for _, x := range tcs {
		tc, _ := x.(map[string]any)
		if tc == nil {
			continue
		}
		id, _ := tc["id"].(string)
		key := -1
		if v, ok := tc["index"].(float64); ok {
			key = int(v)
		}
		var t *anthropicToolCall
		switch {
		case key >= 0:
			t = s.toolAt[key]
		case id != "":
			for _, x := range s.tools {
				if x.id == id {
					t = x
				}
			}
		case len(s.tools) > 0:
			t = s.tools[len(s.tools)-1]
		}
		if t == nil {
			t = &anthropicToolCall{}
			s.tools = append(s.tools, t)
			if key >= 0 {
				s.toolAt[key] = t
			}
		}
		if t.id == "" {
			t.id = id
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, _ := fn["name"].(string); n != "" && t.name == "" {
				t.name = n
			}
			if a, _ := fn["arguments"].(string); a != "" {
				t.args.WriteString(a)
			}
		}
	}
}

func (s *anthropicStream) openBlock(kind string, block map[string]any) {
	s.closeBlock()
	s.open = kind
	s.blocks++
	s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": block})
}

func (s *anthropicStream) closeBlock() {
	if s.open == "" {
		return
	}
	if s.open == "thinking" {
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
			"delta": map[string]any{"type": "signature_delta", "signature": anthropicThinkingSignature}})
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.open = ""
	s.index++
}

func (s *anthropicStream) thinking(text string) {
	if text == "" {
		return
	}
	if s.open != "thinking" {
		s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text}})
}

func (s *anthropicStream) text(text string) {
	if text == "" {
		return
	}
	if s.open != "text" {
		s.openBlock("text", map[string]any{"type": "text", "text": ""})
	}
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
		"delta": map[string]any{"type": "text_delta", "text": text}})
}

// fail 流中途的上游错误：发 Anthropic error 事件后结束（不再发 message_stop）。
func (s *anthropicStream) fail(typ, msg string) {
	s.start()
	s.event("error", map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	s.done = true
}

// finish 正常收尾：关掉打开的块 → 发工具调用块 → message_delta（含真实用量）→ message_stop。
func (s *anthropicStream) finish() {
	if s.done {
		return
	}
	s.start()
	s.closeBlock()
	for _, t := range s.tools {
		s.blocks++
		s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.index,
			"content_block": map[string]any{"type": "tool_use", "id": t.toolID(), "name": t.name, "input": map[string]any{}}})
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(t.input())}})
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
		s.index++
	}
	if s.blocks == 0 {
		// 空回答也给一个空 text 块：部分客户端假定 content 至少一项。
		s.openBlock("text", map[string]any{"type": "text", "text": ""})
		s.closeBlock()
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(s.finishR, len(s.tools) > 0), "stop_sequence": nil},
		"usage": anthropicUsage(s.usage, s.estIn),
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
	s.done = true
}

// replay 内层给的是完整结果、客户端要的是流：按同样的事件序列一次性发出。
func (s *anthropicStream) replay(res *openAIResult) {
	s.start()
	s.thinking(res.reasoning)
	s.text(res.content)
	s.tools = res.tools
	s.finishR = res.finish
	s.usage = res.usage
	s.finish()
}

// message 非流式 Anthropic 响应。
func (s *anthropicStream) message(res *openAIResult) map[string]any {
	content := []any{}
	if res.reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": res.reasoning, "signature": anthropicThinkingSignature})
	}
	if res.content != "" || (res.reasoning == "" && len(res.tools) == 0) {
		content = append(content, map[string]any{"type": "text", "text": res.content})
	}
	for _, t := range res.tools {
		content = append(content, map[string]any{"type": "tool_use", "id": t.toolID(), "name": t.name, "input": t.input()})
	}
	return map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model,
		"content":       content,
		"stop_reason":   anthropicStopReason(res.finish, len(res.tools) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsage(res.usage, s.estIn),
	}
}
