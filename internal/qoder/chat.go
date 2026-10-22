package qoder

// 推理主链路 ChatStream。移植自 qoder2api-hub（MIT）qoder_proxy.py 的 open_upstream /
// iter_inner_sse / 流式转发（/v1/chat/completions handler 的 want_stream 分支）与排队重试
// _envelope_queue_wait / should_retry_envelope。
//
// 契约（见包注释与任务说明）：无论请求 stream 真假，输出**永远是 OpenAI SSE**，流尾补一个
// usage 帧和 [DONE]。首个有效内容字节之前的失败在包内重试（连接层瞬时重试 2 次、国际版
// 备用主机、HTTP 200 但信封 418/5xx 重开、排队等待），仍失败返回 (nil, *Error)；已输出内容
// 之后出错则以 error 帧 + [DONE] 收尾，KindFromErrorFrame 可还原 Kind。

import (
	"bufio"
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func nowMillis() int64 { return time.Now().UnixNano() / 1e6 }

// usageHolder 捕获上游 usage 块（流尾据此合成归一 usage 帧）。
type usageHolder struct{ usage *jval }

// httpError openOnce 的 HTTP 非 2xx 错误（携带状态码与已读响应体）。
type httpError struct {
	code   int
	detail string
}

func (e *httpError) Error() string {
	return "qoder http " + strconv.Itoa(e.code) + ": " + truncate(e.detail, 200)
}

// ChatStream 给一个 Qoder 账号发起推理，返回 OpenAI SSE 流。
func (c *Client) ChatStream(ctx context.Context, cred Cred, body []byte) (io.ReadCloser, *Error) {
	realm := NormalizeRealm(cred.Realm)
	payload, err := parseJ(body)
	if err != nil || !payload.isObj() {
		return nil, &Error{Kind: KindBadRequest, Msg: "request body is not a JSON object",
			Body: truncate(string(body), 300)}
	}
	model := payload.get("model").str()
	modelKey := c.resolveUpstreamKey(model, realm)
	allowed := toolNamesFromPayload(payload)

	bodyObj := c.buildQoderBody(payload, cred, modelKey, realm)
	encoded := Encode([]byte(bodyObj.dumpPy()))
	if len(encoded) > maxUpstreamBodyBytes {
		c.logf("outbound payload too large (%d bytes > %d) - refusing to send", len(encoded), maxUpstreamBodyBytes)
		return nil, &Error{Kind: KindTooLarge, Status: 413,
			Msg:  friendlyMessage(KindTooLarge, 413, ""),
			Body: "encoded body " + strconv.Itoa(len(encoded)) + " bytes exceeds " + strconv.Itoa(maxUpstreamBodyBytes)}
	}

	hosts := c.Gateways(realm)
	hostIdx := 0
	transientTries := 0
	envelopeReopens := 0
	queueRounds := 0
	queueStart := time.Now()

	for {
		if ctx.Err() != nil {
			return nil, &Error{Kind: KindTransport, Msg: "context canceled before upstream response",
				Body: ctx.Err().Error()}
		}
		resp, oErr := c.openOnce(ctx, cred, encoded, modelKey, hosts[hostIdx])
		if oErr != nil {
			if he, ok := oErr.(*httpError); ok {
				if qwait := queueWaitSeconds(he.detail); qwait > 0 {
					if c.queueWait(ctx, qwait, &queueRounds, queueStart) {
						continue
					}
					return nil, c.makeError(KindQueued, he.code, he.detail, qwait)
				}
				if (he.code == 418 || he.code >= 500) && envelopeReopens < 2 {
					envelopeReopens++
					sleepCtx(ctx, time.Duration(envelopeReopens)*time.Second)
					continue
				}
				kind := classifyUpstream(he.code, he.detail)
				return nil, c.makeError(kind, he.code, he.detail, 0)
			}
			// 传输层错误
			if isTransientTransport(oErr) && transientTries < 2 {
				if hostIdx+1 < len(hosts) {
					hostIdx++
				} else {
					sleepCtx(ctx, time.Duration(transientTries+1)*time.Second)
				}
				transientTries++
				continue
			}
			return nil, &Error{Kind: KindTransport, Msg: friendlyMessage(KindTransport, 0, ""),
				Body: truncate(oErr.Error(), 300)}
		}

		// HTTP 200：建流并预读首帧（首个有效内容字节之前的失败在此处重试）。
		holder := &usageHolder{}
		pipe := c.buildPipeline(resp.Body, holder, allowed)
		first, pErr := pipe.next()
		if pErr == nil {
			return newChatReadCloser(resp, pipe, first, holder), nil
		}
		if pErr == errEOF {
			// 上游建流后无任何内容：交给 readcloser 输出「空流」错误帧 + DONE。
			return newChatReadCloser(resp, pipe, nil, holder), nil
		}
		resp.Body.Close()
		if us, ok := pErr.(*upstreamStatus); ok {
			if us.protocol {
				// 协议级失败（工具参数非法等）：走 error 帧，不重试、不返回 (nil,*Error)。
				e := &Error{Kind: us.kind, Code: us.code, Msg: friendlyMessage(us.kind, 0, us.detail),
					Body: truncate(us.detail, 2000)}
				return newStaticStream(errorFrameBytes(e)), nil
			}
			if qwait := queueWaitSeconds(us.detail); qwait > 0 {
				if c.queueWait(ctx, qwait, &queueRounds, queueStart) {
					continue
				}
				return nil, c.makeError(KindQueued, us.status, us.detail, qwait)
			}
			if (us.status == 418 || us.status >= 500) && envelopeReopens < 2 {
				envelopeReopens++
				sleepCtx(ctx, time.Duration(envelopeReopens)*time.Second)
				continue
			}
			kind := classifyUpstream(us.status, us.detail)
			return nil, c.makeError(kind, us.status, us.detail, 0)
		}
		if isTransientTransport(pErr) && transientTries < 2 {
			if hostIdx+1 < len(hosts) {
				hostIdx++
			} else {
				sleepCtx(ctx, time.Duration(transientTries+1)*time.Second)
			}
			transientTries++
			continue
		}
		return nil, &Error{Kind: KindTransport, Msg: friendlyMessage(KindTransport, 0, ""),
			Body: truncate(pErr.Error(), 300)}
	}
}

func (c *Client) buildPipeline(body io.Reader, holder *usageHolder, allowed map[string]bool) frameSource {
	env := newEnvelopeSource(body, holder)
	a := newLeakSource(env, allowed, c.logf)
	b := newDSMLSource(a, allowed, c.logf)
	return newFillIDSource(b)
}

// openOnce 构造 COSY 签名请求并打开上游 SSE；2xx 返回 resp，否则返回 *httpError 或传输错误。
func (c *Client) openOnce(ctx context.Context, cred Cred, encoded, modelKey, host string) (*http.Response, error) {
	sess, err := c.Sessions.Get(cred)
	if err != nil {
		return nil, err
	}
	rawURL := host + chatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header = sess.Headers(encoded, rawURL, HeaderOpts{ModelKey: modelKey, SSE: true})
	fixUserAgent(req.Header)
	resp, err := c.streamc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, &httpError{code: resp.StatusCode, detail: string(raw)}
	}
	return resp, nil
}

// queueWait 排队等待一轮；返回 false = 预算耗尽（应放弃）。受 QueueMaxRounds / QueueMaxWait / ctx 约束。
func (c *Client) queueWait(ctx context.Context, qwait int, rounds *int, start time.Time) bool {
	*rounds++
	if *rounds > c.opts.QueueMaxRounds {
		return false
	}
	elapsed := time.Since(start)
	if elapsed >= c.opts.QueueMaxWait {
		return false
	}
	// retryAfterSeconds + 1s + 0~2s 抖动（多客户端被同时唤醒会一起打同一队列）。
	wait := time.Duration(qwait)*time.Second + time.Second +
		time.Duration(rand.Float64()*2*float64(time.Second))
	if remaining := c.opts.QueueMaxWait - elapsed; wait > remaining {
		wait = remaining
	}
	if wait <= 0 {
		return false
	}
	c.logf("upstream queued, waiting %.1fs (round %d/%d)", wait.Seconds(), *rounds, c.opts.QueueMaxRounds)
	return sleepCtx(ctx, wait)
}

func (c *Client) makeError(kind ErrKind, status int, detail string, retryAfterSecs int) *Error {
	e := &Error{
		Kind:   kind,
		Status: status,
		Code:   extractBusinessCode(detail),
		Msg:    friendlyMessage(kind, status, detail),
		Body:   truncate(detail, 2000),
	}
	if retryAfterSecs > 0 {
		e.RetryAfter = time.Duration(retryAfterSecs) * time.Second
	}
	return e
}

// sleepCtx 休眠 d，ctx 取消则提前返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isTransientTransport 传输层瞬时故障（_is_transient_transport）：TLS EOF / 重置 / 超时等。
func isTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"ssl", "tls", "eof", "reset", "timed out", "timeout",
		"broken pipe", "connection"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func statusInt(v *jval) int {
	if v == nil {
		return 0
	}
	if v.k == jNum {
		if f, ok := v.num(); ok {
			return int(f)
		}
	}
	if v.k == jStr {
		if n, err := strconv.Atoi(strings.TrimSpace(v.s)); err == nil {
			return n
		}
	}
	return 0
}

// extractBusinessCode 从上游原文里提取业务码（如 10605 / TOKEN_EXPIRE / 12153），没有则空。
func extractBusinessCode(detail string) string {
	if detail == "" {
		return ""
	}
	if v, err := parseJString(stripWS(detail)); err == nil && v.isObj() {
		if code := v.get("code"); code.isStr() && code.s != "" {
			return code.s
		} else if code.k == jNum {
			return code.s
		}
	}
	for _, m := range []string{"TOKEN_EXPIRE", "10605", "12153"} {
		if strings.Contains(detail, m) {
			return m
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 信封解包（iter_inner_sse）
// ---------------------------------------------------------------------------

type envelopeSource struct {
	r      *bufio.Reader
	holder *usageHolder
}

func newEnvelopeSource(body io.Reader, holder *usageHolder) *envelopeSource {
	return &envelopeSource{r: bufio.NewReaderSize(body, 64*1024), holder: holder}
}

func (s *envelopeSource) next() ([]byte, error) {
	for {
		line, rerr := s.r.ReadString('\n')
		if len(line) > 0 {
			f, e := s.handle(line)
			if e != nil {
				return nil, e
			}
			if f != nil {
				return f, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil, errEOF
			}
			return nil, rerr
		}
	}
}

func (s *envelopeSource) handle(line string) ([]byte, error) {
	data := stripDataPrefix(line)
	if data == "" {
		return nil, nil
	}
	outer, err := parseJString(data)
	if err != nil || !outer.isObj() {
		return nil, nil
	}
	status := outer.get("statusCodeValue")
	bodyV := outer.get("body")
	if status != nil && !status.isNull() {
		if si := statusInt(status); si != 200 {
			detail := ""
			if bodyV.isStr() {
				detail = bodyV.s
			} else {
				detail = truncate(outer.dumpPy(), 400)
			}
			return nil, newUpstreamStatus(si, detail)
		}
	}
	if !bodyV.isStr() {
		return nil, nil
	}
	if bodyV.s == "[DONE]" {
		return nil, errEOF
	}
	inner, ierr := parseJString(bodyV.s)
	hasUsage := false
	if ierr == nil && inner.isObj() {
		if u := inner.get("usage"); u != nil && !u.isNull() && u.truthy() {
			s.holder.usage = u
			hasUsage = true
		}
	}
	cleaned := cleanChunk(bodyV.s)
	if cleaned == "" {
		return nil, nil
	}
	if hasUsage {
		// 纯 usage 帧（choices 为空）：已捕获，不外发（流尾由 ChatStream 合成归一 usage 帧）。
		if ch := inner.get("choices"); !ch.isArr() || len(ch.arr) == 0 {
			return nil, nil
		}
	}
	return []byte("data: " + cleaned + "\n\n"), nil
}

// ---------------------------------------------------------------------------
// 输出 ReadCloser
// ---------------------------------------------------------------------------

type chatReadCloser struct {
	resp    *http.Response
	pipe    frameSource
	first   []byte
	sentF   bool
	holder  *usageHolder
	meta    metaState
	buf     []byte
	ended   bool
	emitted bool
	closed  bool
}

func newChatReadCloser(resp *http.Response, pipe frameSource, first []byte, holder *usageHolder) *chatReadCloser {
	return &chatReadCloser{resp: resp, pipe: pipe, first: first, holder: holder}
}

func (c *chatReadCloser) Read(p []byte) (int, error) {
	for len(c.buf) == 0 && !c.ended {
		c.fill()
	}
	if len(c.buf) == 0 && c.ended {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *chatReadCloser) fill() {
	if c.first != nil && !c.sentF {
		c.sentF = true
		c.absorb(c.first)
		c.emitted = true
		c.buf = append(c.buf, c.first...)
		return
	}
	frame, err := c.pipe.next()
	if err == nil {
		c.absorb(frame)
		c.emitted = true
		c.buf = append(c.buf, frame...)
		return
	}
	if err == errEOF {
		c.buf = append(c.buf, c.finalFrames()...)
		c.ended = true
		return
	}
	c.buf = append(c.buf, c.errorTail(err)...)
	c.ended = true
}

func (c *chatReadCloser) absorb(frame []byte) {
	data := stripDataPrefix(string(frame))
	if data == "" || data == "[DONE]" {
		return
	}
	if v, perr := parseJString(data); perr == nil && v.isObj() {
		c.meta.absorb(v)
	}
}

func (c *chatReadCloser) finalFrames() []byte {
	var out []byte
	if c.holder.usage != nil {
		out = append(out, usageFrame(&c.meta, c.holder.usage)...)
	} else if !c.emitted {
		e := &Error{Kind: KindServer, Msg: "empty upstream stream"}
		out = append(out, []byte("data: "+errorFramePayload(e)+"\n\n")...)
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return out
}

func (c *chatReadCloser) errorTail(err error) []byte {
	var e *Error
	if us, ok := err.(*upstreamStatus); ok {
		kind := us.kind
		if kind == KindNone {
			kind = classifyUpstream(us.status, us.detail)
		}
		status := us.status
		if us.protocol {
			status = 0
		}
		code := us.code
		if code == "" {
			code = extractBusinessCode(us.detail)
		}
		e = &Error{Kind: kind, Status: status, Code: code,
			Msg: friendlyMessage(kind, us.status, us.detail), Body: truncate(us.detail, 2000)}
	} else {
		e = &Error{Kind: KindTransport, Msg: friendlyMessage(KindTransport, 0, ""),
			Body: truncate(err.Error(), 300)}
	}
	return errorFrameBytes(e)
}

func (c *chatReadCloser) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.resp != nil {
		return c.resp.Body.Close()
	}
	return nil
}

// usageFrame 合成归一 usage 帧（choices 为空数组），字段名与网关统计口径一致。
func usageFrame(m *metaState, usage *jval) []byte {
	pt := jIntField(usage, "prompt_tokens")
	ct := jIntField(usage, "completion_tokens")
	tt := jIntField(usage, "total_tokens")
	ctd := usage.get("completion_tokens_details")
	reasoning := jIntField(ctd, "reasoning_tokens")
	cached := jIntField(usage, "prompt_cache_hit_tokens")
	if cached == 0 {
		cached = jIntField(ctd, "cached_tokens")
	}
	if cached == 0 {
		cached = jIntField(usage.get("prompt_tokens_details"), "cached_tokens")
	}
	miss := pt - cached
	if miss < 0 {
		miss = 0
	}
	u := jObjV(
		"prompt_tokens", jIntV(pt),
		"completion_tokens", jIntV(ct),
		"total_tokens", jIntV(tt),
		"prompt_cache_hit_tokens", jIntV(cached),
		"prompt_cache_miss_tokens", jIntV(miss),
		"completion_tokens_details", jObjV("reasoning_tokens", jIntV(reasoning)),
	)
	// credit：只在上游数据里有 credits/credit 时给（按原实现口径，不编造）。
	if credit := usage.get("credits"); credit != nil && !credit.isNull() {
		u.set("credit", credit)
	} else if credit := usage.get("credit"); credit != nil && !credit.isNull() {
		u.set("credit", credit)
	}
	payload := jObjV(
		"id", nonEmpty(m.id, "chatcmpl-qoder"),
		"object", "chat.completion.chunk",
		"created", m.createdVal(),
		"model", m.model,
		"choices", jArrV(),
		"usage", u,
	)
	return []byte("data: " + payload.dumpCompact() + "\n\n")
}

func jIntField(v *jval, key string) int64 {
	if !v.isObj() {
		return 0
	}
	return jInt64(v.get(key))
}

// errorFrameBytes error 帧 + [DONE]。
func errorFrameBytes(e *Error) []byte {
	return []byte("data: " + errorFramePayload(e) + "\n\n" + "data: [DONE]\n\n")
}

// staticStream 预先算好的字节流（协议失败发生在首帧之前时用）。
type staticStream struct {
	data []byte
	off  int
}

func newStaticStream(data []byte) *staticStream { return &staticStream{data: data} }

func (s *staticStream) Read(p []byte) (int, error) {
	if s.off >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.off:])
	s.off += n
	return n, nil
}

func (s *staticStream) Close() error { return nil }
