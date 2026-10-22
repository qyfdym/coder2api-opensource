package qoder

// DeepSeek DSML 工具调用回退解析。移植自 qoder2api-hub（MIT）qoder_proxy.py 的
// parse_dsml_tool_calls / _dsml_tail_hold / _sse_recover_dsml（issue #51）。
//
// chat 流式路径此前完全没有 DSML 回读，模型直接吐原生 DSML 标签时整段协议会当正文返回。
// 这里平时只暂留「像标签开头」的几个字符；正文一旦出现 DSML 就缓冲到流末统一解析，
// 解析出调用则补发散文 + tool_calls + finish=tool_calls，解析不出则原样补发（fail-open）。

import (
	"regexp"
	"strings"
)

var (
	dsmlCallsRe  = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*calls>(.*?)</[^>]*DSML[^>]*calls>`)
	dsmlInvokeRe = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*invoke\s+name=["']([^"']+)["']>(.*?)</[^>]*invoke>`)
	dsmlParamRe  = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*parameter\s+name=["']([^"']+)["'][^>]*>(.*?)</[^>]*parameter>`)
)

// parseDSMLToolCalls 把 DSML 标签块还原成 tool_calls（parse_dsml_tool_calls）。
// 返回 (calls, cleanText, ok)。
func parseDSMLToolCalls(text string) ([]toolCallOut, string, bool) {
	if text == "" || !strings.Contains(text, "DSML") {
		return nil, text, false
	}
	match := dsmlCallsRe.FindStringSubmatchIndex(text)
	var callsBlock string
	if match == nil {
		if dsmlInvokeRe.FindStringIndex(text) == nil {
			return nil, text, false
		}
		callsBlock = text
	} else {
		callsBlock = text[match[2]:match[3]]
	}
	var calls []toolCallOut
	for _, inv := range dsmlInvokeRe.FindAllStringSubmatch(callsBlock, -1) {
		funcName := inv[1]
		paramsBlock := inv[2]
		params := jObjV()
		for _, p := range dsmlParamRe.FindAllStringSubmatch(paramsBlock, -1) {
			params.set(p[1], jStrV(stripWS(p[2])))
		}
		calls = append(calls, toolCallOut{id: newID("call_"), typ: "function",
			name: funcName, args: params.dumpPy()})
	}
	var clean string
	if match != nil {
		clean = stripWS(stripWS(text[:match[0]]) + " " + stripWS(text[match[1]:]))
	} else {
		start := 0
		locs := dsmlInvokeRe.FindAllStringIndex(text, -1)
		if len(locs) > 0 {
			start = locs[0][0]
			for _, l := range locs {
				if l[0] < start {
					start = l[0]
				}
			}
		}
		lt := strings.LastIndex(text[:start], "<")
		if lt >= 0 {
			clean = stripWS(text[:lt])
		} else {
			clean = stripWS(text[:start])
		}
	}
	return calls, clean, len(calls) > 0
}

// dsmlTailHold 返回「疑似 DSML 标签开头」的尾巴（跨帧暂留，_dsml_tail_hold）。
func dsmlTailHold(text string) string {
	rs := []rune(text)
	n := len(rs)
	i := -1
	for j := n - 1; j >= 0; j-- {
		if rs[j] == '<' {
			i = j
			break
		}
	}
	if i < 0 || n-i > 24 {
		return ""
	}
	tail := string(rs[i:])
	if strings.Contains(tail, ">") {
		return ""
	}
	norm := strings.ToUpper(strings.ReplaceAll(tail, "\uFF5C", "|"))
	body := norm[1:]
	if strings.HasPrefix(body, "/") {
		body = body[1:]
	}
	if body == "" {
		return tail
	}
	if !strings.HasPrefix(body, "|") {
		return ""
	}
	rest := body[1:]
	if rest == "" {
		return tail
	}
	if strings.HasPrefix("DSML", rest) || strings.HasPrefix(rest, "DSML") {
		return tail
	}
	return ""
}

// ---------------------------------------------------------------------------
// 流式 DSML 回读生成器（_sse_recover_dsml）
// ---------------------------------------------------------------------------

type dsmlSource struct {
	src       frameSource
	allowed   map[string]bool
	buf       string
	tail      string
	finPay    *jval
	finReason string
	hasFin    bool
	meta      metaState
	recovered bool
	pending   [][]byte
	ended     bool
	logf      func(string, ...any)
}

func newDSMLSource(src frameSource, allowed map[string]bool, logf func(string, ...any)) *dsmlSource {
	return &dsmlSource{src: src, allowed: allowed, logf: logf}
}

func (s *dsmlSource) push(b []byte) { s.pending = append(s.pending, b) }

func (s *dsmlSource) next() ([]byte, error) {
	for {
		if len(s.pending) > 0 {
			f := s.pending[0]
			s.pending = s.pending[1:]
			return f, nil
		}
		if s.ended {
			return nil, errEOF
		}
		frame, err := s.src.next()
		if err == errEOF {
			s.ended = true
			s.endBlock()
			continue
		}
		if err != nil {
			return nil, err
		}
		s.process(frame)
	}
}

func (s *dsmlSource) process(frame []byte) {
	data := stripDataPrefix(string(frame))
	if data == "" || data == "[DONE]" {
		return // 丢弃（[DONE] 由 ChatStream 末尾统一补发）
	}
	payload, err := parseJString(data)
	if err != nil {
		s.push(frame)
		return
	}
	choices := payload.get("choices")
	if !choices.isArr() || len(choices.arr) == 0 {
		s.push(frame)
		return
	}
	if s.meta.id == "" {
		if v := payload.get("id").str(); v != "" {
			s.meta.id = v
		}
	}
	if s.meta.model == "" {
		if v := payload.get("model").str(); v != "" {
			s.meta.model = v
		}
	}
	if s.meta.created == nil {
		if c := payload.get("created"); c != nil && c.k == jNum {
			s.meta.created = c
		}
	}
	ch := choices.arr[0]
	if !ch.isObj() {
		ch = jObjV()
	}
	delta := ch.get("delta")
	if !delta.isObj() {
		delta = jObjV()
	}
	pieceV := delta.get("content")
	fin := ch.get("finish_reason")
	if !pieceV.isStr() || pieceV.s == "" {
		if fin.truthy() {
			s.finPay = payload
			s.finReason = fin.str()
			s.hasFin = true
		} else {
			s.push(frame)
		}
		return
	}
	piece := s.tail + pieceV.s
	s.tail = ""
	if s.buf == "" {
		hold := dsmlTailHold(piece)
		body := piece
		if hold != "" {
			body = piece[:len(piece)-len(hold)]
		}
		if strings.Contains(body, "DSML") {
			s.buf = body
			s.tail = hold
			return
		}
		if body != "" {
			p2 := payload.clone()
			c0 := p2.get("choices").arr[0]
			d := c0.get("delta")
			if !d.isObj() {
				d = jObjV()
				c0.set("delta", d)
			}
			d.set("content", jStrV(body))
			s.push([]byte("data: " + p2.dumpPy() + "\n\n"))
		}
		s.tail = hold
		return
	}
	s.buf += piece
}

func (s *dsmlSource) endBlock() {
	if s.buf != "" {
		calls, clean, ok := parseDSMLToolCalls(s.buf)
		if !ok {
			if lc, lclean, lok := parseLeakedToolCalls(s.buf, s.allowed); lok {
				calls = lc
				clean = lclean
				ok = true
			}
		}
		if ok && len(calls) > 0 {
			s.recovered = true
			if clean != "" {
				s.push(leakChunkFrame(&s.meta, jObjV("content", clean), nil))
			}
			for i, call := range calls {
				args := call.args
				if args == "" {
					args = "{}"
				}
				tc := jObjV("index", int64(i), "id", nonEmpty(call.id, newID("call_")),
					"type", "function",
					"function", jObjV("name", call.name, "arguments", args))
				s.push(leakChunkFrame(&s.meta, jObjV("tool_calls", jArrV(tc)), nil))
			}
		} else {
			logUnrecoveredLeak(s.logf, s.buf)
			s.push(leakChunkFrame(&s.meta, jObjV("content", s.buf), nil))
		}
	}
	if s.tail != "" {
		s.push(leakChunkFrame(&s.meta, jObjV("content", s.tail), nil))
	}
	if s.hasFin {
		payload := s.finPay
		if s.recovered {
			payload = payload.clone()
			if chs := payload.get("choices"); chs.isArr() {
				for _, ch := range chs.arr {
					if ch.isObj() && ch.get("finish_reason").truthy() {
						ch.set("finish_reason", jStrV("tool_calls"))
					}
				}
			}
		}
		s.push([]byte("data: " + payload.dumpPy() + "\n\n"))
	} else if s.recovered {
		tcr := "tool_calls"
		s.push(leakChunkFrame(&s.meta, jObjV(), &tcr))
	}
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
