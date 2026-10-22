package qoder

// Python json.loads 的「成败 + 报错类别 + 报错位置」复刻（只诊断，不产出值）。
//
// 为什么要复刻到报错位置：工具调用泄漏回读的流式暂存判据（原实现
// _json_array_prefix_ok，qoder2api-hub MIT）直接依赖 CPython json 扫描器的异常消息
// 前缀（Unterminated string / Invalid \uXXXX escape / Expecting ',' delimiter /
// Expecting property name / Extra data）与 err.pos。Go encoding/json 的报错形态与
// 位置口径完全不同，若用它近似，同一段半截 JSON 在两边会得到不同的「继续暂存 /
// 立即证伪」结论——前者多压住几帧无所谓，后者会把内部协议文本当正文漏给用户。
// 所以这里按 CPython _json.c（3.11）的扫描顺序逐条复刻；位置按 Python str 下标
// （码点）计，调用方用 []rune 切片。

type pyJSONErr struct {
	msg string
	pos int // 码点下标
}

type pyScan struct {
	r []rune
}

func pyIsWS(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *pyScan) ws(i int) int {
	for i < len(p.r) && pyIsWS(p.r[i]) {
		i++
	}
	return i
}

// scanErr stop=true 表示 StopIteration（顶层转成 "Expecting value"）。
type scanErr struct {
	pyJSONErr
	stop bool
}

func stopAt(pos int) *scanErr { return &scanErr{pyJSONErr{"Expecting value", pos}, true} }
func errAt(msg string, pos int) *scanErr {
	return &scanErr{pyJSONErr{msg, pos}, false}
}

// pyJSONCheck 等价于 Python json.loads(s) 是否成功；失败返回消息与位置。
func pyJSONCheck(s string) *pyJSONErr {
	return pyJSONCheckRunes([]rune(s))
}

func pyJSONCheckRunes(r []rune) *pyJSONErr {
	p := &pyScan{r: r}
	i := p.ws(0)
	end, err := p.scanOnce(i)
	if err != nil {
		return &err.pyJSONErr
	}
	end = p.ws(end)
	if end != len(p.r) {
		return &pyJSONErr{"Extra data", end}
	}
	return nil
}

// pyJSONValid json.loads(s) 是否成功（Python 口径，含 NaN / Infinity 字面量）。
func pyJSONValid(s string) bool { return pyJSONCheck(s) == nil }

func (p *pyScan) scanOnce(idx int) (int, *scanErr) {
	n := len(p.r)
	if idx >= n {
		return 0, stopAt(idx)
	}
	switch p.r[idx] {
	case '"':
		return p.scanString(idx)
	case '{':
		return p.parseObject(idx + 1)
	case '[':
		return p.parseArray(idx + 1)
	case 'n':
		if idx+3 < n && p.r[idx+1] == 'u' && p.r[idx+2] == 'l' && p.r[idx+3] == 'l' {
			return idx + 4, nil
		}
	case 't':
		if idx+3 < n && p.r[idx+1] == 'r' && p.r[idx+2] == 'u' && p.r[idx+3] == 'e' {
			return idx + 4, nil
		}
	case 'f':
		if idx+4 < n && string(p.r[idx+1:idx+5]) == "alse" {
			return idx + 5, nil
		}
	case 'N':
		if idx+2 < n && p.r[idx+1] == 'a' && p.r[idx+2] == 'N' {
			return idx + 3, nil
		}
	case 'I':
		if idx+7 < n && string(p.r[idx+1:idx+8]) == "nfinity" {
			return idx + 8, nil
		}
	case '-':
		if idx+8 < n && string(p.r[idx+1:idx+9]) == "Infinity" {
			return idx + 9, nil
		}
	}
	return p.matchNumber(idx)
}

func pyIsDigit(c rune) bool { return c >= '0' && c <= '9' }

func (p *pyScan) matchNumber(start int) (int, *scanErr) {
	r := p.r
	endIdx := len(r) - 1
	idx := start
	if idx <= endIdx && r[idx] == '-' {
		idx++
		if idx > endIdx {
			return 0, stopAt(start)
		}
	}
	if idx > endIdx {
		return 0, stopAt(start)
	}
	switch {
	case r[idx] >= '1' && r[idx] <= '9':
		idx++
		for idx <= endIdx && pyIsDigit(r[idx]) {
			idx++
		}
	case r[idx] == '0':
		idx++
	default:
		return 0, stopAt(start)
	}
	if idx < endIdx && r[idx] == '.' && pyIsDigit(r[idx+1]) {
		idx += 2
		for idx <= endIdx && pyIsDigit(r[idx]) {
			idx++
		}
	}
	if idx < endIdx && (r[idx] == 'e' || r[idx] == 'E') {
		eStart := idx
		idx++
		if idx < endIdx && (r[idx] == '-' || r[idx] == '+') {
			idx++
		}
		for idx <= endIdx && pyIsDigit(r[idx]) {
			idx++
		}
		if !pyIsDigit(r[idx-1]) {
			idx = eStart
		}
	}
	return idx, nil
}

func pyHex4(r []rune) (rune, bool) {
	var v rune
	for _, c := range r {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= c - '0'
		case c >= 'a' && c <= 'f':
			v |= c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v |= c - 'A' + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// scanString begin 指向开引号。
func (p *pyScan) scanString(begin int) (int, *scanErr) {
	r := p.r
	n := len(r)
	i := begin + 1
	for {
		j := i
		for j < n {
			c := r[j]
			if c == '"' || c == '\\' {
				break
			}
			if c <= 0x1f {
				return 0, errAt("Invalid control character at", j)
			}
			j++
		}
		if j >= n {
			return 0, errAt("Unterminated string starting at", begin)
		}
		if r[j] == '"' {
			return j + 1, nil
		}
		j++ // 跳过反斜杠
		if j >= n {
			return 0, errAt("Unterminated string starting at", begin)
		}
		c := r[j]
		if c != 'u' {
			if !runeIn(`"\/bfnrt`, c) {
				return 0, errAt("Invalid \\escape", j-1)
			}
			i = j + 1
			continue
		}
		j++ // 跳过 u
		end := j + 4
		if end >= n {
			return 0, errAt("Invalid \\uXXXX escape", j-1)
		}
		cp, ok := pyHex4(r[j:end])
		if !ok {
			return 0, errAt("Invalid \\uXXXX escape", end-5)
		}
		if cp >= 0xd800 && cp <= 0xdbff && end+6 < n && r[end] == '\\' && r[end+1] == 'u' {
			lo, ok := pyHex4(r[end+2 : end+6])
			if !ok {
				return 0, errAt("Invalid \\uXXXX escape", end+1)
			}
			if lo >= 0xdc00 && lo <= 0xdfff {
				end += 6
			}
		}
		i = end
	}
}

func runeIn(s string, c rune) bool {
	for _, x := range s {
		if x == c {
			return true
		}
	}
	return false
}

func (p *pyScan) parseArray(idx int) (int, *scanErr) {
	r := p.r
	endIdx := len(r) - 1
	idx = p.ws(idx)
	if idx > endIdx || r[idx] != ']' {
		for {
			next, err := p.scanOnce(idx)
			if err != nil {
				return 0, err
			}
			idx = p.ws(next)
			if idx <= endIdx && r[idx] == ']' {
				break
			}
			if idx > endIdx || r[idx] != ',' {
				return 0, errAt("Expecting ',' delimiter", idx)
			}
			idx = p.ws(idx + 1)
		}
	}
	return idx + 1, nil
}

func (p *pyScan) parseObject(idx int) (int, *scanErr) {
	r := p.r
	endIdx := len(r) - 1
	idx = p.ws(idx)
	if idx > endIdx || r[idx] != '}' {
		for {
			if idx > endIdx || r[idx] != '"' {
				return 0, errAt("Expecting property name enclosed in double quotes", idx)
			}
			next, err := p.scanString(idx)
			if err != nil {
				return 0, err
			}
			idx = p.ws(next)
			if idx > endIdx || r[idx] != ':' {
				return 0, errAt("Expecting ':' delimiter", idx)
			}
			idx = p.ws(idx + 1)
			next, err = p.scanOnce(idx)
			if err != nil {
				return 0, err
			}
			idx = p.ws(next)
			if idx <= endIdx && r[idx] == '}' {
				break
			}
			if idx > endIdx || r[idx] != ',' {
				return 0, errAt("Expecting ',' delimiter", idx)
			}
			idx = p.ws(idx + 1)
		}
	}
	return idx + 1, nil
}
