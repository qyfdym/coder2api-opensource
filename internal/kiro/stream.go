package kiro

// ChatStream：OpenAI 请求 → Kiro 推理 → OpenAI SSE（移植自 Kiro-Go handleOpenAIStream /
// handleOpenAINonStream 的输出部分；选号、换号、记账交给网关）。
//
//   - stream=true：首个上游输出之后才返回（之前的失败返回 (nil, *Error) 让网关换号），
//     随后边解码边写 SSE；
//   - stream=false：在包内读完整条流（被截断时同号重试最多 2 次），成功才返回一段完整的
//     SSE，网关照常聚合成 chat.completion——非流式客户端永远拿不到半截回答。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// maxSameAccountRetries 非流式请求遇到截断时的同号重试次数（Kiro-Go 同口径）。
const maxSameAccountRetries = 2

// ChatStream 用一个 Kiro 账号发起推理。body 是 OpenAI chat.completions 请求体（model 为裸模型名）。
func (c *Client) ChatStream(ctx context.Context, cred Cred, body []byte) (io.ReadCloser, *Error) {
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, &Error{Kind: KindBadRequest, Msg: "request body is not a valid chat.completions JSON", Body: truncate(err.Error(), 300)}
	}
	if len(req.Messages) == 0 {
		return nil, &Error{Kind: KindBadRequest, Msg: "messages must not be empty"}
	}
	ep := c.planEffort(req.Model, effortFromBody(body))
	modelID := ep.kiroModel
	payload := buildPayload(&req, modelID, ep.budget)
	if ep.native && ep.nativeEffort != "" {
		payload.AdditionalModelRequestFields = map[string]any{"reasoning": map[string]any{"effort": ep.nativeEffort}}
	}
	plan := chatPlan{
		model:    req.Model,
		thinking: ep.thinking(),
		estIn:    estimateRequestTokens(&req),
		window:   contextWindow(modelID),
	}
	if !req.Stream {
		return c.chatBuffered(ctx, cred, payload, plan)
	}
	return c.chatStreaming(ctx, cred, payload, plan)
}

type chatPlan struct {
	model    string // 响应里回显的模型名（客户端请求的写法）
	thinking bool
	estIn    int
	window   int
}

func (c *Client) chatBuffered(ctx context.Context, cred Cred, p *kiroPayload, plan chatPlan) (io.ReadCloser, *Error) {
	for attempt := 0; attempt <= maxSameAccountRetries; attempt++ {
		var buf bytes.Buffer
		conv := newConverter(plan, func(b []byte) error { buf.Write(b); return nil })
		if err := c.callUpstream(ctx, cred, p, conv.callbacks()); err != nil {
			return nil, err
		}
		if conv.truncated() {
			if ctx.Err() != nil {
				return nil, &Error{Kind: KindTransport, Msg: "context canceled"}
			}
			c.logf("stream for %s ended without stopReason (attempt %d/%d), retrying same account",
				cred.label(), attempt+1, maxSameAccountRetries+1)
			continue
		}
		conv.finish()
		return io.NopCloser(&buf), nil
	}
	return nil, &Error{Kind: KindTruncated, Msg: friendlyMessage(KindTruncated)}
}

func (c *Client) chatStreaming(ctx context.Context, cred Cred, p *kiroPayload, plan chatPlan) (io.ReadCloser, *Error) {
	cctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	ready := make(chan *Error, 1)
	var once sync.Once
	signal := func(e *Error) { once.Do(func() { ready <- e }) }

	conv := newConverter(plan, func(b []byte) error {
		_, err := pw.Write(b)
		return err
	})
	conv.onStart = func() { signal(nil) }

	go func() {
		err := c.callUpstream(cctx, cred, p, conv.callbacks())
		switch {
		case err != nil && !conv.started:
			signal(err)
			pw.CloseWithError(io.ErrUnexpectedEOF)
			return
		case err != nil:
			conv.writeError(err)
		case conv.truncated():
			// 已有输出之后才发现截断：不能重试（会重复输出），也不能伪装成正常结束。
			c.logf("stream for %s ended without stopReason after output; signaling truncation", cred.label())
			conv.writeError(&Error{Kind: KindTruncated, Msg: friendlyMessage(KindTruncated)})
		default:
			conv.finish()
		}
		pw.Close()
	}()

	select {
	case e := <-ready:
		if e != nil {
			cancel()
			return nil, e
		}
		return &pipeReadCloser{r: pr, cancel: cancel}, nil
	case <-ctx.Done():
		cancel()
		return nil, &Error{Kind: KindTransport, Msg: "context canceled before upstream response", Body: ctx.Err().Error()}
	}
}

type pipeReadCloser struct {
	r      *io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (p *pipeReadCloser) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *pipeReadCloser) Close() error {
	p.once.Do(func() {
		p.cancel()
		_ = p.r.Close()
	})
	return nil
}

// ---------------------------------------------------------------------------
// 上游回调 → OpenAI SSE
// ---------------------------------------------------------------------------

type thinkingSource int

const (
	sourceUnknown thinkingSource = iota
	sourceReasoningEvent
	sourceTagBlock
)

// converter 一次推理的输出状态机。
type converter struct {
	plan    chatPlan
	out     func([]byte) error
	onStart func()

	id      string
	created int64

	started  bool
	roleSent bool
	werr     error

	// 思考内容的两种来源（reasoningContentEvent / 正文里的 <thinking> 块）只认先出现的那种，
	// 避免同一段思考被输出两遍。
	source  thinkingSource
	textBuf string
	inTag   bool
	dropTag bool

	rawContent   strings.Builder
	rawReasoning strings.Builder
	tools        []toolUse
	stopReason   string
	inTok        int
	ctxTokens    int
	credits      float64
}

func newConverter(plan chatPlan, out func([]byte) error) *converter {
	return &converter{plan: plan, out: out, id: "chatcmpl-" + newUUID(), created: time.Now().Unix()}
}

func (s *converter) callbacks() *streamCallbacks {
	return &streamCallbacks{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			s.markStarted()
			if isThinking {
				s.rawReasoning.WriteString(text)
			} else {
				s.rawContent.WriteString(text)
			}
			s.processText(text, isThinking, false)
		},
		OnToolUse: func(tu toolUse) {
			s.markStarted()
			s.processText("", false, true)
			s.tools = append(s.tools, tu)
			args, _ := json.Marshal(tu.Input)
			s.emitDelta(map[string]any{"tool_calls": []any{map[string]any{
				"index": len(s.tools) - 1, "id": tu.ID, "type": "function",
				"function": map[string]any{"name": tu.Name, "arguments": string(args)},
			}}})
		},
		OnStopReason:   func(r string) { s.stopReason = r },
		OnComplete:     func(in, _ int) { s.inTok = in },
		OnCredits:      func(cr float64) { s.credits = cr },
		OnContextUsage: func(pct float64) { s.ctxTokens = int(pct * float64(s.plan.window) / 100.0) },
	}
}

func (s *converter) markStarted() {
	if !s.started {
		s.started = true
		if s.onStart != nil {
			s.onStart()
		}
	}
}

func (s *converter) write(payload any) {
	if s.werr != nil {
		return
	}
	raw, _ := json.Marshal(payload)
	s.werr = s.out([]byte("data: " + string(raw) + "\n\n"))
}

func (s *converter) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.plan.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func (s *converter) emitDelta(delta map[string]any) {
	if !s.roleSent {
		s.roleSent = true
		s.write(s.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	}
	s.write(s.chunk(delta, nil))
}

func (s *converter) sendContent(text string) {
	if text != "" {
		s.emitDelta(map[string]any{"content": text})
	}
}

// sendReasoning 思考内容走 delta.reasoning_content；客户端没开思考时整段丢弃（Kiro 偶尔
// 不请自来地在正文里带 <thinking> 块）。
func (s *converter) sendReasoning(text string) {
	if text != "" && s.plan.thinking {
		s.emitDelta(map[string]any{"reasoning_content": text})
	}
}

// processText 拆分正文与 <thinking> 块。标签可能被切在两个分片之间，所以正文末尾留 15 个
// 字符不立即输出，等下一片拼上再判断（force=true 时全部冲出）。
func (s *converter) processText(text string, isThinking, force bool) {
	if isThinking {
		if !s.plan.thinking || s.source == sourceTagBlock {
			return
		}
		s.source = sourceReasoningEvent
		s.sendReasoning(text)
		return
	}
	s.textBuf += text
	for {
		if !s.inTag {
			if start := strings.Index(s.textBuf, "<thinking>"); start >= 0 {
				if start > 0 {
					s.sendContent(s.textBuf[:start])
				}
				s.textBuf = s.textBuf[start+len("<thinking>"):]
				s.inTag = true
				switch s.source {
				case sourceReasoningEvent:
					s.dropTag = true
				default:
					s.source = sourceTagBlock
					s.dropTag = false
				}
				continue
			}
			runes := []rune(s.textBuf)
			if force || len(runes) > 50 {
				safe := len(runes)
				if !force {
					safe = max(0, len(runes)-15)
				}
				if safe > 0 {
					s.sendContent(string(runes[:safe]))
					s.textBuf = string(runes[safe:])
				}
			}
			return
		}
		if end := strings.Index(s.textBuf, "</thinking>"); end >= 0 {
			if !s.dropTag {
				s.sendReasoning(s.textBuf[:end])
			}
			s.textBuf = s.textBuf[end+len("</thinking>"):]
			s.inTag, s.dropTag = false, false
			continue
		}
		if force {
			if s.textBuf != "" && !s.dropTag {
				s.sendReasoning(s.textBuf)
			}
			s.textBuf = ""
			s.inTag, s.dropTag = false, false
			return
		}
		if runes := []rune(s.textBuf); len(runes) > 20 {
			safe := len(runes) - 15
			if !s.dropTag {
				s.sendReasoning(string(runes[:safe]))
			}
			s.textBuf = string(runes[safe:])
		}
		return
	}
}

// truncated 流在没有结束信号的情况下中断：有正文 / 思考却没有 stopReason、也没有工具调用。
// 只有思考没有回答同样算截断（思考完整流出、回答前断掉正是要防的形态）。
func (s *converter) truncated() bool {
	if strings.TrimSpace(s.stopReason) != "" || len(s.tools) > 0 {
		return false
	}
	return s.rawContent.Len() > 0 || s.rawReasoning.Len() > 0
}

// finish 冲出缓冲 → finish_reason 帧 → usage 帧 → [DONE]。
func (s *converter) finish() {
	s.processText("", false, true)
	if !s.roleSent {
		s.emitDelta(map[string]any{"content": ""})
	}
	s.write(s.chunk(map[string]any{}, mapFinishReason(s.stopReason, len(s.tools))))

	in := s.inTok
	if s.ctxTokens > 0 {
		in = s.ctxTokens
	} else if in <= 0 {
		in = s.plan.estIn
	}
	content, extracted := extractThinkingFromContent(s.rawContent.String())
	reasoning := s.rawReasoning.String()
	if s.plan.thinking && reasoning == "" {
		reasoning = extracted
	}
	if !s.plan.thinking {
		reasoning = ""
	}
	reasoningTok := estimateTokens(reasoning)
	out := estimateTokens(content) + reasoningTok
	for _, tu := range s.tools {
		out += estimateTokens(tu.Name) + estimateJSONTokens(tu.Input)
	}
	usage := map[string]any{
		"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		"completion_tokens_details": map[string]any{"reasoning_tokens": reasoningTok},
	}
	if s.credits > 0 {
		usage["credit"] = s.credits
	}
	s.write(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.plan.model,
		"choices": []any{}, "usage": usage,
	})
	if s.werr == nil {
		s.werr = s.out([]byte("data: [DONE]\n\n"))
	}
}

// writeError 已有输出之后的失败：冲出缓冲的正文，再以 error 帧 + [DONE] 收尾。
func (s *converter) writeError(e *Error) {
	s.processText("", false, true)
	if s.werr != nil {
		return
	}
	s.werr = s.out([]byte("data: " + errorFramePayload(e) + "\n\n" + "data: [DONE]\n\n"))
}
