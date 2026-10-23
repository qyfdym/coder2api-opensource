package kiro

// AWS event-stream 二进制帧解码（移植自 Kiro-Go proxy/kiro.go 的 parseEventStreamTracked）。
//
// 帧格式：prelude（总长 4B + 头长 4B + prelude CRC 4B）| 头 | payload(JSON) | 消息 CRC 4B。
// 关心的事件（头 :event-type）：
//   assistantResponseEvent  正文增量      {content}
//   reasoningContentEvent   思考增量      {text}
//   toolUseEvent            工具调用分片  {toolUseId,name,input(字符串分片),stop}
//   meteringEvent           本次扣费      {usage}
//   contextUsageEvent       上下文占用    {contextUsagePercentage}
//   metadataEvent           收尾信息      {stopReason, usage…}
// :message-type = exception / error 的帧是流内错误。

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

const maxEventStreamMessageSize = 16 * 1024 * 1024

var (
	errEmptyStream         = errors.New("upstream stream ended before any output")
	errIncompleteToolInput = errors.New("upstream stream ended with incomplete tool input")
	errInvalidEventStream  = errors.New("invalid upstream event stream")
)

// toolUse 一次完整的工具调用。
type toolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// streamCallbacks 解码过程中的回调（与 Kiro-Go 的 KiroStreamCallback 对位）。
type streamCallbacks struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(tu toolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	OnStopReason   func(reason string)
}

// parseEventStream 解码整条流。emitted 报告是否已经触发过输出回调（OnText / OnToolUse）：
// 首个输出之前的失败可以安全重试，之后的不行（会把两次尝试的输出拼在一起）。
func parseEventStream(body io.Reader, cb *streamCallbacks) (emitted bool, err error) {
	if cb == nil {
		cb = &streamCallbacks{}
	}
	var inputTokens, outputTokens int
	var totalCredits float64
	var contextPcts []float64
	var sawOutput bool
	pending := &pendingToolUses{}
	onTool := func(tu toolUse) {
		sawOutput = true
		if cb.OnToolUse != nil {
			emitted = true
			cb.OnToolUse(tu)
		}
	}

	for {
		prelude := make([]byte, 12)
		if _, rerr := io.ReadFull(body, prelude); rerr != nil {
			if rerr == io.EOF {
				break
			}
			return emitted, rerr
		}
		totalLength := int(be32(prelude[0:4]))
		headersLength := int(be32(prelude[4:8]))
		if totalLength < 16 || totalLength > maxEventStreamMessageSize {
			return emitted, fmt.Errorf("%w: invalid frame length %d", errInvalidEventStream, totalLength)
		}
		if headersLength > totalLength-16 {
			return emitted, fmt.Errorf("%w: invalid headers length %d", errInvalidEventStream, headersLength)
		}
		if be32(prelude[8:12]) != crc32.ChecksumIEEE(prelude[:8]) {
			return emitted, fmt.Errorf("%w: prelude CRC mismatch", errInvalidEventStream)
		}
		message := make([]byte, totalLength-len(prelude))
		if _, rerr := io.ReadFull(body, message); rerr != nil {
			return emitted, rerr
		}
		want := crc32.Update(crc32.Update(0, crc32.IEEETable, prelude), crc32.IEEETable, message[:len(message)-4])
		if be32(message[len(message)-4:]) != want {
			return emitted, fmt.Errorf("%w: message CRC mismatch", errInvalidEventStream)
		}
		headers, herr := parseEventHeaders(message[:headersLength])
		if herr != nil {
			return emitted, fmt.Errorf("%w: %v", errInvalidEventStream, herr)
		}
		payload := message[headersLength : len(message)-4]
		event := map[string]any{}
		if len(payload) > 0 {
			if jerr := json.Unmarshal(payload, &event); jerr != nil {
				return emitted, fmt.Errorf("%w: decode payload: %v", errInvalidEventStream, jerr)
			}
		}

		if mt := headers[":message-type"]; mt == "error" || mt == "exception" {
			exType := headers[":exception-type"]
			if exType == "" {
				exType = headers[":error-code"]
			}
			detail, _ := event["message"].(string)
			if detail == "" {
				detail, _ = event["Message"].(string)
			}
			if detail == "" {
				detail = string(payload)
			}
			full := strings.TrimSpace(exType + ": " + detail)
			kind := classify(0, full)
			return emitted, &Error{Kind: kind, Code: exType, Msg: friendlyMessage(kind), Body: truncate(full, 2000)}
		}

		inputTokens, outputTokens = updateTokens(event, inputTokens, outputTokens)

		switch headers[":event-type"] {
		case "assistantResponseEvent":
			if s, ok := event["content"].(string); ok && s != "" {
				sawOutput = true
				if cb.OnText != nil {
					emitted = true
					cb.OnText(s, false)
				}
			}
		case "reasoningContentEvent":
			if s, ok := event["text"].(string); ok && s != "" {
				sawOutput = true
				if cb.OnText != nil {
					emitted = true
					cb.OnText(s, true)
				}
			}
		case "toolUseEvent":
			if terr := handleToolUseEvent(event, pending, onTool); terr != nil {
				return emitted, terr
			}
		case "meteringEvent":
			if u, ok := event["usage"].(float64); ok {
				totalCredits += u
			}
		case "contextUsageEvent":
			if p, ok := event["contextUsagePercentage"].(float64); ok {
				contextPcts = append(contextPcts, p)
			}
		case "metadataEvent":
			// stopReason 随 metadataEvent 下发（没有独立的 stop 事件）；流里有正文却始终没有
			// stopReason = 被截断（见 stream.go 的完整性判定）。
			if r := firstString(event, "stopReason", "stop_reason"); r != "" && cb.OnStopReason != nil {
				cb.OnStopReason(r)
			}
		}
	}

	// 收到 stop 帧之前流就结束的工具调用按到达顺序补发。
	if err := pending.flushAll(onTool); err != nil {
		return emitted, err
	}
	if !sawOutput {
		return emitted, errEmptyStream
	}
	if cb.OnCredits != nil && totalCredits > 0 {
		cb.OnCredits(totalCredits)
	}
	if cb.OnContextUsage != nil {
		for _, p := range contextPcts {
			cb.OnContextUsage(p)
		}
	}
	if cb.OnComplete != nil {
		cb.OnComplete(inputTokens, outputTokens)
	}
	return emitted, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// parseEventHeaders 解析帧头（只保留字符串值；其余类型按长度跳过）。
func parseEventHeaders(data []byte) (map[string]string, error) {
	headers := map[string]string{}
	for off := 0; off < len(data); {
		nameLen := int(data[off])
		off++
		if nameLen == 0 || off+nameLen >= len(data) {
			return nil, errors.New("malformed event header name")
		}
		name := string(data[off : off+nameLen])
		off += nameLen
		vt := data[off]
		off++
		vl := 0
		switch vt {
		case 0, 1: // bool true / false：无值
			continue
		case 2:
			vl = 1
		case 3:
			vl = 2
		case 4:
			vl = 4
		case 5, 8:
			vl = 8
		case 9:
			vl = 16
		case 6, 7:
			if off+2 > len(data) {
				return nil, errors.New("malformed variable event header")
			}
			vl = int(data[off])<<8 | int(data[off+1])
			off += 2
		default:
			return nil, fmt.Errorf("unsupported event header type %d", vt)
		}
		if off+vl > len(data) {
			return nil, errors.New("truncated event header value")
		}
		if vt == 7 {
			headers[name] = string(data[off : off+vl])
		}
		off += vl
	}
	return headers, nil
}

// ---------------------------------------------------------------------------
// token 统计（上游各版本字段名不一，逐个尝试）
// ---------------------------------------------------------------------------

func updateTokens(event map[string]any, curIn, curOut int) (int, int) {
	cands := []map[string]any{event}
	collectUsageMaps(event, &cands)
	in, out := curIn, curOut
	outKeys := []string{"outputTokens", "completionTokens", "totalOutputTokens", "output_tokens", "completion_tokens", "total_output_tokens"}
	for _, u := range cands {
		if u == nil {
			continue
		}
		if v, ok := readTokenNumber(u, outKeys...); ok {
			out = v
		}
		if v, ok := readTokenNumber(u, "inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens"); ok {
			in = v
			continue
		}
		uncached, _ := readTokenNumber(u, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(u, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(u, "cacheWriteInputTokens", "cache_write_input_tokens",
			"cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			in = uncached + cacheRead + cacheWrite
			continue
		}
		if total, ok := readTokenNumber(u, "totalTokens", "total_tokens"); ok && total > 0 {
			o := out
			if v, vok := readTokenNumber(u, outKeys...); vok {
				o = v
			}
			if total-o > 0 {
				in = total - o
			}
		}
	}
	return in, out
}

func collectUsageMaps(v any, out *[]map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]any); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []any:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func readTokenNumber(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if p, err := n.Int64(); err == nil {
				return int(p), true
			}
		case string:
			if p, err := strconv.Atoi(n); err == nil {
				return p, true
			}
			if p, err := strconv.ParseFloat(n, 64); err == nil {
				return int(p), true
			}
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 工具调用分片拼接
// ---------------------------------------------------------------------------

type toolUseState struct {
	id        string
	name      string
	input     strings.Builder
	generated bool
}

// pendingToolUses 一条流里在途的工具调用：按 toolUseId 分别累积（并行调用的分片可能交错），
// order 保留到达顺序（工具调用顺序对客户端有语义，不能交给 map 的随机遍历）。
type pendingToolUses struct {
	byID   map[string]*toolUseState
	order  []string
	lastID string
}

func (p *pendingToolUses) get(id string) *toolUseState {
	if p.byID == nil {
		return nil
	}
	return p.byID[id]
}

func (p *pendingToolUses) add(s *toolUseState) {
	if p.byID == nil {
		p.byID = map[string]*toolUseState{}
	}
	p.byID[s.id] = s
	p.order = append(p.order, s.id)
	p.lastID = s.id
}

// rekey 把本地生成的临时 id 换成上游后来给出的真实 id（保持到达顺序）。
func (p *pendingToolUses) rekey(s *toolUseState, newID string) {
	old := s.id
	delete(p.byID, old)
	for i, id := range p.order {
		if id == old {
			p.order[i] = newID
			break
		}
	}
	s.id, s.generated = newID, false
	p.byID[newID] = s
	if p.lastID == old {
		p.lastID = newID
	}
}

func (p *pendingToolUses) remove(id string) {
	delete(p.byID, id)
	for i, x := range p.order {
		if x == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.lastID == id {
		p.lastID = ""
	}
}

// flushAll 流结束时按到达顺序补发仍未收尾的工具调用；参数不完整的直接报错（交给重试，
// 不能把缺参数的调用交给客户端执行）。
func (p *pendingToolUses) flushAll(emit func(toolUse)) error {
	order, byID := p.order, p.byID
	p.byID, p.order, p.lastID = nil, nil, ""
	for _, id := range order {
		if s := byID[id]; s != nil {
			if err := finishToolUse(s, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// handleToolUseEvent 把一帧 toolUseEvent 累积进 pending。不同 toolUseId 的分片并行累积；
// 不带 id 的分片续接最近一个工具。
func handleToolUseEvent(event map[string]any, pending *pendingToolUses, emit func(toolUse)) error {
	id := firstString(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstString(event, "name", "toolName", "tool_name")
	stop := firstBool(event, "stop", "isStop", "done")

	var s *toolUseState
	switch {
	case id != "":
		s = pending.get(id)
		if s == nil && pending.lastID != "" {
			// 上游可能先发不带 id 的首片、之后才给真实 id：收养之前开的临时条目，
			// 而不是把一个调用拆成两半（两半参数都解析不了）。
			if prev := pending.get(pending.lastID); prev != nil && prev.generated && (name == "" || prev.name == name) {
				pending.rekey(prev, id)
				s = prev
			}
		}
		if s == nil {
			if name == "" {
				return nil // 孤立分片：没见过的 id 又没有名字，无从开启
			}
			s = &toolUseState{id: id, name: name}
			pending.add(s)
		} else {
			if name != "" && s.name == "" {
				s.name = name
			}
			pending.lastID = s.id
		}
	case pending.lastID != "" && pending.get(pending.lastID) != nil:
		s = pending.get(pending.lastID)
		if name != "" && s.name != name {
			// 不带 id 但换了名字：先收尾在途的那个，再开新的，免得把别的参数拼进去。
			if err := finishToolUse(s, emit); err != nil {
				return err
			}
			pending.remove(s.id)
			s = &toolUseState{id: "toolu_" + newUUID(), name: name, generated: true}
			pending.add(s)
		}
	case name != "":
		s = &toolUseState{id: "toolu_" + newUUID(), name: name, generated: true}
		pending.add(s)
	default:
		return nil
	}

	if in, ok := event["input"].(string); ok {
		s.input.WriteString(in)
	} else if obj, ok := event["input"].(map[string]any); ok {
		raw, _ := json.Marshal(obj)
		s.input.Reset()
		s.input.Write(raw)
	}
	if stop {
		if err := finishToolUse(s, emit); err != nil {
			return err
		}
		pending.remove(s.id)
	}
	return nil
}

func finishToolUse(s *toolUseState, emit func(toolUse)) error {
	if s == nil || s.name == "" {
		return nil
	}
	if s.id == "" {
		s.id = "toolu_" + newUUID()
	}
	var input map[string]any
	if s.input.Len() > 0 {
		if err := json.Unmarshal([]byte(s.input.String()), &input); err != nil {
			return fmt.Errorf("%w: %v", errIncompleteToolInput, err)
		}
	}
	if input == nil {
		input = map[string]any{}
	}
	if emit != nil {
		emit(toolUse{ID: s.id, Name: s.name, Input: input})
	}
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}
