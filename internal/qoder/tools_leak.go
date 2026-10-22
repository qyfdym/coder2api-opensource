package qoder

// 历史工具调用「泄漏回读」。移植自 qoder2api-hub（MIT）qoder_proxy.py 的 issue #8/#9/#11/
// #16/#36/#37/#47/#49/#51/#60/#72 一整套判据与两个流式生成器（_recover_leaked_tool_calls_inner
// 与 _sse_fill_tool_call_ids）。
//
// 背景：flatten_messages 会把 assistant 历史里的 tool_calls 序列化成「marker + JSON 数组」
// 文本喂给上游；长会话里模型偶发照格式复述成正文（没有结构化 tool_calls），客户端就会把
// 这段 JSON 当正文显示、本轮工具调用不执行。这里在严格守卫下把它还原成结构化 tool_calls，
// 并处理各种残缺 / 非法 / 跨帧截断形态——判定全部照搬原实现，务必与其行为逐条一致。

import (
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var errEOF = io.EOF

func nowUnix() int64 { return time.Now().Unix() }

// ---- 标记常量（LEAK_MARKER / LEAK_MARKER_ALT / TOOL_RESULT_* / _JSON_FENCE） ----
const (
	leakMarker       = "[assistant 请求调用工具]"
	leakMarkerAlt    = "<assistant 请求调用工具>"
	markerCore       = "assistant请求调用工具"
	markerOpenCanon  = "[assistant 请求调用工具"
	toolResultMarker = "[工具结果"
	toolResultClose  = "[工具结果结束]"
	jsonFence        = "```"

	holdMaxChars  = 32768 // issue #16：hold 字符上限
	holdMaxFrames = 200   // 连续空转帧上限
)

// markerHoldWindow = max(len(leakMarker), len(leakMarkerAlt), len(toolResultMarker)) - 1
// （按字符数；两个长 marker 均 18，故窗口 17）。
var markerHoldWindow = maxInt(
	utf8.RuneCountInString(leakMarker),
	utf8.RuneCountInString(leakMarkerAlt),
	utf8.RuneCountInString(toolResultMarker)) - 1

func maxInt(xs ...int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

var (
	leakMarkerRe         = regexp.MustCompile(`[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?`)
	leakMarkerAnchoredRe = regexp.MustCompile(`^[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?`)
	dsmlCloseTagRe       = regexp.MustCompile(`</[^>]*DSML[^>]*>\s*$`)
	dsmlLeadRe           = regexp.MustCompile(`^(?:\s*</[^>]*DSML[^>]*>)+`)
	echoLooseRe          = regexp.MustCompile(`(?s)^\[\s*\{\s*"name"\s*:\s*"([^"]{1,200})"\s*,\s*"arguments"\s*:\s*"(.*)"\s*\}\s*\]$`)
	nameFieldRe          = regexp.MustCompile(`"name"\s*:\s*"([^"]{1,120})"`)
)

func lstripWS(s string) string { return strings.TrimLeftFunc(s, isUnicodeSpace) }
func isUnicodeSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0xA0 || r == 0x3000
}

func stripWS(s string) string { return strings.TrimFunc(s, isUnicodeSpace) }

// ---- marker 识别 ----

func markerKey(text string) string {
	return strings.NewReplacer(" ", "", "\u3000", "").Replace(text)
}

// markerPrefixOK text 是否是某个 marker 变体的前半截（_marker_prefix_ok）。
func markerPrefixOK(text string) bool {
	raw := lstripWS(text)
	body := strings.TrimLeft(raw, "[<")
	key := markerKey(body)
	if key == "" {
		s := stripWS(raw)
		return s == "" || s == "[" || s == "<" || s == "[<"
	}
	return strings.HasPrefix(markerCore, key)
}

// lastMarkerSpan 最后一个 marker 变体的 (start,end) 字节偏移，没有则 (-1,-1)（_last_marker_span）。
func lastMarkerSpan(text string) (int, int) {
	locs := leakMarkerRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return -1, -1
	}
	last := locs[len(locs)-1]
	return last[0], last[1]
}

// normalizeLeakMarker 把开头的尖括号 / 残缺 marker 变体归一成方括号规范形态（_normalize_leak_marker）。
func normalizeLeakMarker(text string) string {
	if text == "" {
		return text
	}
	t := lstripWS(text)
	lead := text[:len(text)-len(t)]
	if loc := leakMarkerAnchoredRe.FindStringIndex(t); loc != nil && loc[0] == 0 {
		matched := t[:loc[1]]
		trimmed := strings.TrimRightFunc(matched, isUnicodeSpace)
		closeCh := ""
		if n := len(trimmed); n > 0 {
			last := trimmed[n-1:]
			if last == "]" || last == ">" || last == ")" {
				closeCh = "]"
			}
		}
		return lead + markerOpenCanon + closeCh + t[loc[1]:]
	}
	if strings.HasPrefix(t, "```") {
		rest := t[3:]
		low := strings.ToLower(lstripWS(rest))
		var after string
		if strings.HasPrefix(low, "json") {
			after = lstripWS(lstripWS(rest)[4:])
		} else {
			after = lstripWS(rest)
		}
		if loc := leakMarkerAnchoredRe.FindStringIndex(after); loc != nil && loc[0] == 0 {
			return lead + t[:len(t)-len(after)] + leakMarker + after[loc[1]:]
		}
	}
	return text
}

// ---- DSML 尾标签剥离 ----

func stripDSMLCloseTail(text string) string {
	for {
		next := dsmlCloseTagRe.ReplaceAllString(text, "")
		if next == text {
			return text
		}
		text = next
	}
}

func dsmlTailHoldable(tail string) bool {
	s := stripWS(tail)
	if s == "" {
		return true
	}
	for {
		next := stripWS(dsmlCloseTagRe.ReplaceAllString(s, ""))
		if next == s {
			break
		}
		s = next
	}
	if s == "" {
		return true
	}
	if strings.HasPrefix(s, "</") {
		return utf8.RuneCountInString(s) <= 80
	}
	return s == "<"
}

// ---- JSON 围栏剥离 ----

func stripJSONFence(text string) string {
	t := stripWS(text)
	if strings.HasPrefix(t, jsonFence) {
		inner := t[len(jsonFence):]
		if len(inner) >= 4 && strings.ToLower(inner[:4]) == "json" {
			inner = inner[4:]
		}
		t = lstripWS(inner)
	}
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = strings.TrimRightFunc(t[:len(t)-len(jsonFence)], isUnicodeSpace)
	}
	return t
}

// ---- 名称守卫 ----

func echoNamesDeclared(text string, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	ms := nameFieldRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !allowed[m[1]] {
			return false
		}
	}
	return true
}

func markerOnlyEcho(text string) bool {
	if stripWS(text) == "" {
		return false
	}
	t := normalizeLeakMarker(stripWS(text))
	rest := t
	for strings.HasPrefix(rest, leakMarker) {
		rest = lstripWS(rest[len(leakMarker):])
	}
	rest = stripWS(stripDSMLCloseTail(rest))
	return rest == ""
}

// ---- 单调用抢救（引号未转义） ----

func repairJSONQuotes(text string) string {
	const ph = "\x00"
	s := strings.ReplaceAll(text, `\"`, ph)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, ph, `\"`)
}

func salvageEchoArray(rest string) *jval {
	m := echoLooseRe.FindStringSubmatch(stripWS(rest))
	if m == nil {
		return nil
	}
	name, rawArgs := m[1], m[2]
	for _, cand := range []string{rawArgs, repairJSONQuotes(rawArgs)} {
		if pyJSONValid(cand) {
			return jArrV(jObjV("name", name, "arguments", cand))
		}
	}
	return nil
}

// toolCallOut 回读还原出的结构化调用。
type toolCallOut struct {
	id   string
	typ  string
	name string
	args string
}

// parseLeakedToolCalls 把「marker + JSON 数组」还原成结构化 tool_calls（parse_leaked_tool_calls）。
// 返回 (calls, cleanText, ok)：ok=true 时 calls 非空、cleanText 为 marker 前的散文；
// ok=false 时表示未识别（调用方原样补发 text）。
func parseLeakedToolCalls(text string, allowed map[string]bool) ([]toolCallOut, string, bool) {
	t := stripWS(text)
	t = normalizeLeakMarker(t)
	t = stripDSMLCloseTail(t)
	if strings.HasPrefix(t, "```") && strings.HasSuffix(t, "```") && len(t) > 6 {
		inner := t[3:]
		if len(inner) >= 4 && strings.ToLower(inner[:4]) == "json" {
			inner = inner[4:]
		}
		t = stripWS(inner[:len(inner)-3])
	}
	t = stripWS(dsmlLeadRe.ReplaceAllString(t, ""))
	mpos, mend := lastMarkerSpan(t)
	if mpos < 0 {
		if strings.HasPrefix(t, "[") {
			t = leakMarker + "\n" + t
			mpos, mend = 0, len(leakMarker)
		} else {
			return nil, text, false
		}
	}
	prefix := strings.TrimRightFunc(leakMarkerRe.ReplaceAllString(t[:mpos], ""), isUnicodeSpace)
	t = t[mend:]
	for {
		ls := lstripWS(t)
		loc := leakMarkerAnchoredRe.FindStringIndex(ls)
		if loc == nil || loc[0] != 0 {
			break
		}
		t = ls[loc[1]:]
	}
	t = lstripWS(t)
	t = stripDSMLCloseTail(t)
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = stripDSMLCloseTail(strings.TrimRightFunc(t[:len(t)-len(jsonFence)], isUnicodeSpace))
	}
	rest := stripWS(t)
	if !(strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]")) {
		i := strings.Index(rest, "{")
		j := strings.LastIndex(rest, "}")
		if !strings.HasPrefix(rest, "[") && i >= 0 && i < j {
			cand := "[" + rest[i:j+1] + "]"
			if pyJSONValid(cand) {
				rest = cand
			} else {
				return nil, text, false
			}
		} else {
			return nil, text, false
		}
	}
	var arr *jval
	if v, err := parseJString(rest); err == nil && v.isArr() {
		arr = v
	} else {
		arr = salvageEchoArray(rest)
		if arr == nil {
			return nil, text, false
		}
	}
	if !arr.isArr() || len(arr.arr) == 0 {
		return nil, text, false
	}
	var calls []toolCallOut
	for _, item := range arr.arr {
		if !item.isObj() {
			return nil, text, false
		}
		name := item.get("name")
		args := item.get("arguments")
		if !name.isStr() || strings.TrimSpace(name.s) == "" {
			return nil, text, false
		}
		var argStr string
		switch {
		case args.isObj() || args.isArr():
			argStr = args.dumpPy()
		case args.isStr():
			argStr = args.s
		default:
			return nil, text, false
		}
		if strings.TrimSpace(argStr) != "" {
			if !pyJSONValid(argStr) {
				return nil, text, false
			}
		}
		calls = append(calls, toolCallOut{id: newID("call_"), typ: "function",
			name: strings.TrimSpace(name.s), args: argStr})
	}
	if len(allowed) > 0 {
		for _, c := range calls {
			if !allowed[c.name] {
				return nil, text, false
			}
		}
	}
	return calls, prefix, true
}

// ---- 流式候选判定 ----

// leakBodyOffset 把「marker 之前的围栏前缀 / marker 之后的正文」解析出来（_leak_body_offset）。
// 返回 (state, body)：state ∈ {"pre","body","no"}。
func leakBodyOffset(text string) (string, string) {
	t := lstripWS(normalizeLeakMarker(text))
	if t == "" || strings.HasPrefix(leakMarker, t) {
		return "pre", ""
	}
	if loc := leakMarkerAnchoredRe.FindStringIndex(t); loc != nil && loc[0] == 0 {
		b := lstripWS(t[loc[1]:])
		for {
			loc2 := leakMarkerAnchoredRe.FindStringIndex(b)
			if loc2 == nil || loc2[0] != 0 {
				break
			}
			b = lstripWS(b[loc2[1]:])
		}
		if b != "" && markerPrefixOK(b) {
			return "body", ""
		}
		return "body", b
	}
	if markerPrefixOK(t) {
		return "pre", ""
	}
	if !strings.HasPrefix(t, "`") {
		return "no", ""
	}
	if len(t) < 3 {
		if strings.HasPrefix("```", t) {
			return "pre", ""
		}
		return "no", ""
	}
	if !strings.HasPrefix(t, "```") {
		return "no", ""
	}
	rest := t[3:]
	low := strings.ToLower(lstripWS(rest))
	var after string
	switch {
	case strings.HasPrefix(low, "json"):
		after = lstripWS(lstripWS(rest)[4:])
	case strings.HasPrefix(leakMarker, low) || strings.HasPrefix(low, leakMarker):
		after = lstripWS(rest)
	case strings.HasPrefix("json", low):
		return "pre", ""
	default:
		return "no", ""
	}
	if strings.HasPrefix(leakMarker, after) {
		return "pre", ""
	}
	if strings.HasPrefix(after, leakMarker) {
		return "body", after[len(leakMarker):]
	}
	return "no", ""
}

func leakPrefixHold(text string) bool {
	st, _ := leakBodyOffset(text)
	return st != "no"
}

// jsonArrayPrefixOK text 是否仍可能扩展成合法 JSON 数组（_json_array_prefix_ok）。
func jsonArrayPrefixOK(text string) bool {
	s := stripWS(text)
	if s == "" {
		return true
	}
	if !strings.HasPrefix(s, "[") {
		return false
	}
	perr := pyJSONCheck(s)
	if perr == nil {
		return true
	}
	rs := []rune(s)
	n := len(rs)
	msg, pos := perr.msg, perr.pos
	if strings.HasPrefix(msg, "Unterminated string") {
		return true
	}
	if strings.HasPrefix(msg, "Invalid \\uXXXX escape") {
		seg1 := ""
		if pos-1 >= 0 && pos+1 <= n {
			lo := pos - 1
			if lo < 0 {
				lo = 0
			}
			seg1 = string(rs[lo:min2(pos+1, n)])
		}
		seg2 := ""
		if pos <= n {
			seg2 = string(rs[pos:min2(pos+2, n)])
		}
		return seg1 == `\u` || seg2 == `\u`
	}
	if strings.Contains(s, "arguments") &&
		(strings.HasPrefix(msg, "Expecting ',' delimiter") ||
			strings.HasPrefix(msg, "Expecting property name")) {
		return true
	}
	if strings.HasPrefix(msg, "Extra data") {
		tail := ""
		if pos <= n {
			tail = stripWS(string(rs[pos:]))
		}
		if tail == "" || strings.HasPrefix("```", tail) {
			return true
		}
		if strings.HasPrefix(tail, jsonFence) {
			tail = stripWS(tail[len(jsonFence):])
			if tail == "" {
				return true
			}
		}
		return dsmlTailHoldable(tail)
	}
	return pos >= n-1
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// findMarkerSplit 把正文切成 (立即发出, 暂存候选)（_find_marker_split）。
func findMarkerSplit(text string, allowPrefix bool) (string, string) {
	t := normalizeLeakMarker(text)
	if t == "" {
		return "", ""
	}
	best := -1
	if loc := leakMarkerRe.FindStringIndex(t); loc != nil {
		best = loc[0]
	}
	if i := strings.Index(t, toolResultMarker); i != -1 && (best == -1 || i < best) {
		best = i
	}
	if best != -1 {
		return t[:best], t[best:]
	}
	if !allowPrefix {
		return t, ""
	}
	rt := []rune(t)
	var tail []rune
	if len(rt) > markerHoldWindow {
		tail = rt[len(rt)-markerHoldWindow:]
	} else {
		tail = rt
	}
	for n := len(tail); n > 0; n-- {
		seg := string(tail[len(tail)-n:])
		if markerPrefixOK(seg) || strings.HasPrefix(toolResultMarker, seg) {
			cut := len(string(rt[:len(rt)-n]))
			return t[:cut], t[cut:]
		}
	}
	return t, ""
}

// stillLeakCandidate 整段目前是否仍是「marker + JSON 数组」候选（_still_leak_candidate）。
func stillLeakCandidate(text string) bool {
	state, body := leakBodyOffset(text)
	if state == "pre" {
		return true
	}
	if state == "no" {
		return false
	}
	rest := lstripWS(body)
	if rest == "" {
		return true
	}
	trimmed := lstripWS(strings.TrimLeft(rest, "\"'>=: ]"))
	if trimmed == "" {
		return true
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		rest = trimmed
		if strings.HasPrefix(rest, "{") {
			return true
		}
	}
	if strings.HasSuffix(rest, "```") {
		rest = rest[:len(rest)-3]
	}
	return jsonArrayPrefixOK(rest)
}

// leakedPartialDroppable 截断/非法的 marker+JSON 回声块是否应整段吞掉（_leaked_partial_droppable）。
func leakedPartialDroppable(text string, allowed map[string]bool) bool {
	if text == "" || len(allowed) == 0 {
		return false
	}
	t := stripWS(text)
	if t == "" {
		return false
	}
	if t == leakMarker {
		return true
	}
	state, body := leakBodyOffset(t)
	if state != "body" {
		return false
	}
	rest := stripWS(body)
	if strings.HasSuffix(rest, "```") {
		rest = stripWS(rest[:len(rest)-3])
	}
	if !jsonArrayPrefixOK(rest) {
		return false
	}
	return !pyJSONValid(rest)
}

// ---- 工具结果回声（issue #11） ----

func toolEchoHead(text string) (string, bool) {
	t := stripWS(text)
	if !strings.HasPrefix(t, toolResultMarker) {
		return "", false
	}
	rest := t[len(toolResultMarker):]
	close := strings.Index(rest, "]")
	if close == -1 || strings.Contains(rest[:close], "\n") {
		return "", false
	}
	return rest[close+1:], true
}

func toolEchoDroppable(text string, allowed map[string]bool) bool {
	if text == "" || len(allowed) == 0 {
		return false
	}
	body, ok := toolEchoHead(text)
	if !ok {
		return false
	}
	if strings.Contains(body, toolResultClose) {
		return true
	}
	rest := lstripWS(body)
	if rest == "" {
		return true
	}
	return startsWithAny(rest, "{[\"<")
}

func toolEchoPrefixHold(text string) bool {
	t := lstripWS(text)
	if t == "" {
		return true
	}
	if strings.HasPrefix(toolResultMarker, t) {
		return true
	}
	body, ok := toolEchoHead(t)
	if !ok {
		return false
	}
	if strings.Contains(body, toolResultClose) {
		return true
	}
	rest := lstripWS(body)
	if rest == "" {
		return true
	}
	return startsWithAny(rest, "{[\"<")
}

func echoHoldCandidate(text string) bool {
	return stillLeakCandidate(text) || toolEchoPrefixHold(text)
}

func startsWithAny(s, set string) bool {
	if s == "" {
		return false
	}
	return strings.IndexByte(set, s[0]) >= 0
}

// echoWithBadArgs 「工具协议回声 + 内层参数非法」判定（_echo_with_bad_args）。
func echoWithBadArgs(text string, allowed map[string]bool) bool {
	if text == "" {
		return false
	}
	t := stripJSONFence(normalizeLeakMarker(stripWS(text)))
	for {
		next := stripWS(stripJSONFence(stripWS(stripDSMLCloseTail(t))))
		if next == t {
			break
		}
		t = next
	}
	mpos := strings.LastIndex(t, leakMarker)
	if mpos < 0 {
		return false
	}
	t = t[mpos:]
	for strings.HasPrefix(t, leakMarker) {
		t = lstripWS(t[len(leakMarker):])
	}
	t = stripDSMLCloseTail(t)
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = stripDSMLCloseTail(strings.TrimRightFunc(t[:len(t)-len(jsonFence)], isUnicodeSpace))
	}
	rest := stripWS(t)
	if !(strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]")) {
		return false
	}
	arr, err := parseJString(rest)
	if err != nil || !arr.isArr() || len(arr.arr) == 0 {
		return false
	}
	bad := 0
	for _, item := range arr.arr {
		if !item.isObj() {
			return false
		}
		name := item.get("name")
		if !name.isStr() || strings.TrimSpace(name.s) == "" {
			return false
		}
		if len(allowed) > 0 && !allowed[strings.TrimSpace(name.s)] {
			return false
		}
		if !item.has("arguments") {
			return false
		}
		args := item.get("arguments")
		if args.isObj() || args.isArr() {
			continue
		}
		if !args.isStr() {
			return false
		}
		if strings.TrimSpace(args.s) == "" {
			continue
		}
		if !pyJSONValid(args.s) {
			bad++
		}
	}
	return bad > 0
}

// ---- allowedNames ----

// toolNamesFromPayload 本次请求声明的工具名集合（_tool_names_from_payload）；空返回 nil。
func toolNamesFromPayload(payload *jval) map[string]bool {
	names := map[string]bool{}
	tools := payload.get("tools")
	if tools.isArr() {
		for _, t := range tools.arr {
			if !t.isObj() {
				continue
			}
			fn := t.get("function")
			if !fn.isObj() {
				fn = t
			}
			if name := fn.get("name").str(); name != "" {
				names[name] = true
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// ---------------------------------------------------------------------------
// 流式回读生成器（_recover_leaked_tool_calls_inner）
// ---------------------------------------------------------------------------

type leakSource struct {
	src     frameSource
	allowed map[string]bool
	hold    *string
	holdF   int
	// finished 暂存的收尾帧：payload + finish_reason。
	finPayload *jval
	finReason  string
	hasFin     bool
	recovered  bool
	meta       metaState
	pending    [][]byte
	ended      bool
	logf       func(string, ...any)
}

func newLeakSource(src frameSource, allowed map[string]bool, logf func(string, ...any)) *leakSource {
	return &leakSource{src: src, allowed: allowed, logf: logf}
}

func (s *leakSource) push(b []byte) { s.pending = append(s.pending, b) }

func (s *leakSource) next() ([]byte, error) {
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
			if perr := s.endBlock(); perr != nil {
				return nil, perr
			}
			continue
		}
		if err != nil {
			// 传输/信封错误：丢弃 hold，向上传播。
			return nil, err
		}
		if perr := s.process(frame); perr != nil {
			return nil, perr
		}
	}
}

func (s *leakSource) process(frame []byte) error {
	text := string(frame)
	data := stripDataPrefix(text)
	var payload *jval
	if data != "" && data != "[DONE]" {
		if v, err := parseJString(data); err == nil {
			payload = v
		}
	}
	if payload == nil || !payload.isObj() {
		s.push(frame)
		return nil
	}
	s.meta.absorb(payload)
	choices := payload.get("choices")
	var choice *jval
	if choices.isArr() && len(choices.arr) > 0 && choices.arr[0].isObj() {
		choice = choices.arr[0]
	} else {
		choice = jObjV()
	}
	delta := choice.get("delta")
	if !delta.isObj() {
		delta = jObjV()
	}
	pieceRaw := delta.get("content")
	piece := ""
	if pieceRaw.isStr() {
		piece = pieceRaw.s
	}
	finV := choice.get("finish_reason")
	fin := finV.isStr() && finV.s != ""
	finReason := finV.str()

	if pieceRaw != nil && !pieceRaw.isNull() && !pieceRaw.isStr() {
		if s.hold != nil {
			s.push(leakChunkFrame(&s.meta, jObjV("content", *s.hold), nil))
			s.hold = nil
			s.holdF = 0
		}
		s.push(frame)
		return nil
	}

	if s.hold != nil {
		h := *s.hold + piece
		if piece != "" {
			s.holdF = 0
		} else {
			s.holdF++
		}
		if stripped := stripDSMLCloseTail(h); stripped != h && stillLeakCandidate(stripped) {
			h = stripped
		}
		s.hold = &h
		if echoHoldCandidate(h) {
			if len(h) > holdMaxChars || s.holdF > holdMaxFrames {
				s.logf("released oversized leak hold (%d chars / %d frames) — fail-open", len(h), s.holdF)
				s.push(leakChunkFrame(&s.meta, jObjV("content", h), nil))
				s.hold = nil
				s.holdF = 0
				if fin {
					s.setFinished(frameWithoutContent(payload), finReason)
				}
				return nil
			}
			if fin {
				s.setFinished(frameWithoutContent(payload), finReason)
			} else if delta.get("tool_calls").truthy() || delta.get("reasoning_content").truthy() || delta.get("role").truthy() {
				s.push([]byte("data: " + frameWithoutContent(payload).dumpPy() + "\n\n"))
			}
			return nil
		}
		if echoWithBadArgs(h, s.allowed) {
			return invalidToolArgs()
		}
		// issue #60：散文 + marker 同块时，先放行散文前缀，只保留 marker 起继续判候选。
		lastStart := -1
		for _, loc := range leakMarkerRe.FindAllStringIndex(h, -1) {
			lastStart = loc[0]
		}
		if lastStart > 0 {
			tail60 := h[lastStart:]
			if stillLeakCandidate(tail60) {
				prose60 := h[:lastStart]
				if stripWS(prose60) != "" {
					s.push(leakChunkFrame(&s.meta, jObjV("content", prose60), nil))
				}
				nh := tail60
				s.hold = &nh
				if fin {
					s.setFinished(frameWithoutContent(payload), finReason)
				}
				return nil
			}
		}
		logUnrecoveredLeak(s.logf, h)
		s.push(leakChunkFrame(&s.meta, jObjV("content", h), nil))
		s.hold = nil
		s.holdF = 0
		if fin {
			s.setFinished(frameWithoutContent(payload), finReason)
		}
		return nil
	}

	if fin && piece == "" {
		s.setFinished(payload, finReason)
		return nil
	}
	if piece != "" {
		emit16, hold16 := findMarkerSplit(piece, true)
		if hold16 != "" {
			if emit16 != "" {
				s.push(leakChunkFrame(&s.meta, jObjV("content", emit16), nil))
			}
			s.hold = &hold16
			s.holdF = 1
			if fin {
				s.setFinished(frameWithoutContent(payload), finReason)
			}
			return nil
		}
		if leakPrefixHold(piece) || toolEchoPrefixHold(piece) {
			s.hold = &piece
			s.holdF = 1
			if fin {
				s.setFinished(frameWithoutContent(payload), finReason)
			}
			return nil
		}
		if fin {
			fr := finReason
			s.push(leakChunkFrame(&s.meta, jObjV("content", piece), &fr))
		} else {
			s.push(leakChunkFrame(&s.meta, jObjV("content", piece), nil))
		}
		return nil
	}
	s.push(frame)
	return nil
}

func (s *leakSource) setFinished(payload *jval, reason string) {
	s.finPayload = payload
	s.finReason = reason
	s.hasFin = true
}

func (s *leakSource) endBlock() error {
	if s.hold != nil {
		h := *s.hold
		calls, clean, ok := parseLeakedToolCalls(h, s.allowed)
		switch {
		case ok:
			s.recovered = true
			for i, call := range calls {
				tc := jObjV("index", int64(i), "id", call.id, "type", call.typ,
					"function", jObjV("name", call.name, "arguments", call.args))
				s.push(leakChunkFrame(&s.meta, jObjV("tool_calls", jArrV(tc)), nil))
			}
			if clean != "" {
				s.push(leakChunkFrame(&s.meta, jObjV("content", clean), nil))
			}
		case echoWithBadArgs(h, s.allowed):
			s.logf("tool-call echo has invalid JSON arguments (%d chars) — failing the turn closed", len(h))
			return invalidToolArgs()
		case leakedPartialDroppable(h, s.allowed):
			if len(s.allowed) > 0 && markerOnlyEcho(h) {
				s.logf("dropped truncated leaked tool-call echo (%d chars, marker_only) — failing the turn closed", len(h))
				return emptyToolEcho(len(h), "marker_only")
			}
			s.logf("dropped truncated leaked tool-call echo (%d chars) — issue #9", len(h))
		case toolEchoDroppable(h, s.allowed):
			s.logf("dropped leaked tool-result echo (%d chars) — issue #11", len(h))
		case markerOnlyEcho(h):
			s.logf("dropped bare tool-call marker (%d chars) — issue #72", len(h))
		default:
			logUnrecoveredLeak(s.logf, h)
			s.push(leakChunkFrame(&s.meta, jObjV("content", h), nil))
		}
		s.hold = nil
	}
	if s.hasFin {
		payload := s.finPayload
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
	return nil
}

// logUnrecoveredLeak 认不出但像协议回声时留痕（_log_unrecovered_leak 的简化：不做节流）。
func logUnrecoveredLeak(logf func(string, ...any), text string) {
	marks := strings.Count(text, leakMarker) + strings.Count(text, leakMarkerAlt)
	dsml := strings.Count(text, "DSML")
	if marks == 0 && dsml == 0 {
		return
	}
	logf("unrecovered tool-protocol leak passed through (%d chars, markers=%d, dsml=%d)", len(text), marks, dsml)
}

// ---------------------------------------------------------------------------
// tool_call id 补全（_sse_fill_tool_call_ids）
// ---------------------------------------------------------------------------

type fillIDSource struct {
	src   frameSource
	ids   map[string]string
	named map[string]bool
}

func newFillIDSource(src frameSource) *fillIDSource {
	return &fillIDSource{src: src, ids: map[string]string{}, named: map[string]bool{}}
}

func fillKeyFor(tc *jval, pos, nCalls int) string {
	idx := tc.get("index")
	if idx != nil && idx.k == jNum && idx.isInt() {
		return "i:" + idx.s
	}
	if nCalls > 1 {
		return "p:" + itoa(pos)
	}
	return "p:0"
}

func (s *fillIDSource) next() ([]byte, error) {
	raw, err := s.src.next()
	if err != nil {
		return nil, err
	}
	data := stripDataPrefix(string(raw))
	if data == "" || data == "[DONE]" {
		return raw, nil
	}
	payload, perr := parseJString(data)
	if perr != nil {
		return raw, nil
	}
	choices := payload.get("choices")
	if !choices.isArr() {
		return raw, nil
	}
	touched := false
	for _, ch := range choices.arr {
		if !ch.isObj() {
			continue
		}
		delta := ch.get("delta")
		if !delta.isObj() {
			continue
		}
		calls := delta.get("tool_calls")
		if !calls.isArr() {
			continue
		}
		for pos, tc := range calls.arr {
			if !tc.isObj() {
				continue
			}
			key := fillKeyFor(tc, pos, len(calls.arr))
			cur := tc.get("id")
			fn := tc.get("function")
			name := ""
			if fn.isObj() {
				name = strings.TrimSpace(fn.get("name").str())
			}
			if cur.isStr() && strings.TrimSpace(cur.s) != "" {
				s.ids[key] = cur.s
				if name != "" {
					s.named[key] = true
				}
				continue
			}
			if name != "" && s.named[key] {
				s.ids[key] = newID("call_")
			}
			newid := s.ids[key]
			if newid == "" {
				newid = newID("call_")
			}
			s.ids[key] = newid
			if name != "" {
				s.named[key] = true
			}
			tc.set("id", jStrV(newid))
			touched = true
		}
	}
	if touched {
		return []byte("data: " + payload.dumpPy() + "\n\n"), nil
	}
	return raw, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
