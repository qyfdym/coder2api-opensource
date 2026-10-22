package qoder

// 保序 JSON 值（jval）与 Python json.dumps 口径的序列化。
//
// 为什么不直接用 map[string]any：原实现（qoder2api-hub，MIT）全程在 Python dict 上
// 改写请求体，dict 保留插入顺序——官方 baseprompt 模板的键序、客户端 tools schema 的
// 键序都会原样出现在上游请求体里。上游前缀缓存按字节命中，同一段会话在不同轮次必须
// 序列化成同一串字节；Go map 的键序虽然也确定（json 包按键排序），但会和原实现的
// 出站形态整体不同，排障时无法与原实现逐字段对照。所以这里做一个最小的保序 JSON：
// 解析保留键序，改写按 Python dict 语义（覆盖原位、新键追加），序列化对齐
// json.dumps(ensure_ascii=False) 的默认分隔符（", " 与 ": "）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

type jkind uint8

const (
	jNull jkind = iota
	jBool
	jNum
	jStr
	jArr
	jObj
)

// jval 保序 JSON 值。对象用平行切片保存键序（对象都很小，线性查找足够）。
type jval struct {
	k    jkind
	b    bool
	s    string // 字符串值；数字时为原始字面量
	arr  []*jval
	keys []string
	vals []*jval
}

// ---- 构造 ----

func jNullV() *jval          { return &jval{k: jNull} }
func jBoolV(b bool) *jval    { return &jval{k: jBool, b: b} }
func jStrV(s string) *jval   { return &jval{k: jStr, s: s} }
func jIntV(n int64) *jval    { return &jval{k: jNum, s: strconv.FormatInt(n, 10)} }
func jNumLit(s string) *jval { return &jval{k: jNum, s: s} }
func jFloatV(f float64) *jval {
	return &jval{k: jNum, s: pyFloatRepr(f)}
}
func jArrV(items ...*jval) *jval {
	if items == nil {
		items = []*jval{}
	}
	return &jval{k: jArr, arr: items}
}

// jObjV 按键值交替的参数构造对象：jObjV("a", v1, "b", v2)。
func jObjV(kv ...any) *jval {
	o := &jval{k: jObj}
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), toJ(kv[i+1]))
	}
	return o
}

// toJ 把常见 Go 值转成 jval（构造辅助；不支持的类型按 null）。
func toJ(v any) *jval {
	switch x := v.(type) {
	case nil:
		return jNullV()
	case *jval:
		if x == nil {
			return jNullV()
		}
		return x
	case string:
		return jStrV(x)
	case bool:
		return jBoolV(x)
	case int:
		return jIntV(int64(x))
	case int64:
		return jIntV(x)
	case float64:
		return jFloatV(x)
	case []*jval:
		return jArrV(x...)
	default:
		return jNullV()
	}
}

// ---- 对象操作（Python dict 语义） ----

func (v *jval) isObj() bool { return v != nil && v.k == jObj }
func (v *jval) isArr() bool { return v != nil && v.k == jArr }
func (v *jval) isStr() bool { return v != nil && v.k == jStr }
func (v *jval) isNull() bool {
	return v == nil || v.k == jNull
}

func (v *jval) idx(key string) int {
	if v == nil || v.k != jObj {
		return -1
	}
	for i, k := range v.keys {
		if k == key {
			return i
		}
	}
	return -1
}

// get 取对象成员（不存在或非对象返回 nil）。
func (v *jval) get(key string) *jval {
	if i := v.idx(key); i >= 0 {
		return v.vals[i]
	}
	return nil
}

func (v *jval) has(key string) bool { return v.idx(key) >= 0 }

// set 已有键原位覆盖，新键追加到末尾（与 Python dict 赋值一致）。
func (v *jval) set(key string, val *jval) {
	if v == nil || v.k != jObj {
		return
	}
	if val == nil {
		val = jNullV()
	}
	if i := v.idx(key); i >= 0 {
		v.vals[i] = val
		return
	}
	v.keys = append(v.keys, key)
	v.vals = append(v.vals, val)
}

// setDefault 键不存在时才写入（dict.setdefault）。
func (v *jval) setDefault(key string, val *jval) *jval {
	if cur := v.get(key); cur != nil {
		return cur
	}
	v.set(key, val)
	return val
}

// del 删除键（dict.pop），返回被删的值。
func (v *jval) del(key string) *jval {
	i := v.idx(key)
	if i < 0 {
		return nil
	}
	old := v.vals[i]
	v.keys = append(v.keys[:i:i], v.keys[i+1:]...)
	v.vals = append(v.vals[:i:i], v.vals[i+1:]...)
	return old
}

// str 字符串值（非字符串返回 ""）。
func (v *jval) str() string {
	if v != nil && v.k == jStr {
		return v.s
	}
	return ""
}

// truthy Python 的真值语义（None/False/0/""/[]/{} 为假）。
func (v *jval) truthy() bool {
	if v == nil {
		return false
	}
	switch v.k {
	case jNull:
		return false
	case jBool:
		return v.b
	case jNum:
		f, err := strconv.ParseFloat(v.s, 64)
		return err != nil || f != 0
	case jStr:
		return v.s != ""
	case jArr:
		return len(v.arr) > 0
	case jObj:
		return len(v.keys) > 0
	}
	return false
}

// num 数值（非数字返回 0,false；布尔按 Python 语义当 0/1）。
func (v *jval) num() (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch v.k {
	case jNum:
		f, err := strconv.ParseFloat(v.s, 64)
		return f, err == nil
	case jBool:
		if v.b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// isInt 数字字面量是否为整数形态（Python json.loads 解析为 int）。
func (v *jval) isInt() bool {
	return v != nil && v.k == jNum && !strings.ContainsAny(v.s, ".eE")
}

// clone 深拷贝（对应原实现的 json.loads(json.dumps(x))）。
func (v *jval) clone() *jval {
	if v == nil {
		return nil
	}
	c := &jval{k: v.k, b: v.b, s: v.s}
	if v.arr != nil {
		c.arr = make([]*jval, len(v.arr))
		for i, x := range v.arr {
			c.arr[i] = x.clone()
		}
	}
	if v.keys != nil {
		c.keys = append([]string(nil), v.keys...)
		c.vals = make([]*jval, len(v.vals))
		for i, x := range v.vals {
			c.vals[i] = x.clone()
		}
	}
	return c
}

// shallow 浅拷贝对象 / 数组（对应 dict(m) / list(x)）。
func (v *jval) shallow() *jval {
	if v == nil {
		return nil
	}
	c := &jval{k: v.k, b: v.b, s: v.s}
	if v.arr != nil {
		c.arr = append([]*jval(nil), v.arr...)
	}
	if v.keys != nil {
		c.keys = append([]string(nil), v.keys...)
		c.vals = append([]*jval(nil), v.vals...)
	}
	return c
}

// ---- 解析 ----

var errTrailingJSON = errors.New("qoder json: trailing data")

// parseJ 解析一段完整 JSON（保留键序；重复键按 Python 语义：位置取首次、值取末次）。
func parseJ(data []byte) (*jval, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseJValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errTrailingJSON
	}
	return v, nil
}

func parseJString(s string) (*jval, error) { return parseJ([]byte(s)) }

func parseJValue(dec *json.Decoder) (*jval, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &jval{k: jObj, keys: []string{}, vals: []*jval{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("qoder json: bad object key")
				}
				val, err := parseJValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			a := &jval{k: jArr, arr: []*jval{}}
			for dec.More() {
				val, err := parseJValue(dec)
				if err != nil {
					return nil, err
				}
				a.arr = append(a.arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
		return nil, fmt.Errorf("qoder json: unexpected delim %v", t)
	case string:
		return jStrV(t), nil
	case json.Number:
		return jNumLit(string(t)), nil
	case bool:
		return jBoolV(t), nil
	case nil:
		return jNullV(), nil
	}
	return nil, fmt.Errorf("qoder json: unexpected token %T", tok)
}

// ---- 序列化 ----

// dumpPy 对齐 Python json.dumps(v, ensure_ascii=False)（分隔符 ", " / ": "）。
func (v *jval) dumpPy() string {
	var b strings.Builder
	v.write(&b, ", ", ": ")
	return b.String()
}

// dumpCompact 紧凑形态（出站 SSE 帧用；对端只做 JSON 解析，不关心空白）。
func (v *jval) dumpCompact() string {
	var b strings.Builder
	v.write(&b, ",", ":")
	return b.String()
}

func (v *jval) write(b *strings.Builder, itemSep, keySep string) {
	if v == nil {
		b.WriteString("null")
		return
	}
	switch v.k {
	case jNull:
		b.WriteString("null")
	case jBool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case jNum:
		b.WriteString(pyNumberLiteral(v.s))
	case jStr:
		b.WriteString(pyJSONString(v.s))
	case jArr:
		b.WriteByte('[')
		for i, x := range v.arr {
			if i > 0 {
				b.WriteString(itemSep)
			}
			x.write(b, itemSep, keySep)
		}
		b.WriteByte(']')
	case jObj:
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			b.WriteString(pyJSONString(k))
			b.WriteString(keySep)
			v.vals[i].write(b, itemSep, keySep)
		}
		b.WriteByte('}')
	}
}

// pyNumberLiteral 把 JSON 数字字面量规整成 Python「loads 再 dumps」后的形态：
// 整数字面量原样（Python int 任意精度），浮点按 float.__repr__。
func pyNumberLiteral(lit string) string {
	if lit == "" {
		return "0"
	}
	if !strings.ContainsAny(lit, ".eE") {
		if lit == "-0" {
			return "0"
		}
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return lit
	}
	return pyFloatRepr(f)
}

// pyFloatRepr 复刻 Python float.__repr__：最短往返数字；指数在 [-4, 16) 内用定点
// （整数值补 ".0"），否则科学计数法且指数至少两位（1e-05 / 1e+16）。
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // 形如 -1.2345e+06
	neg := strings.HasPrefix(e, "-")
	if neg {
		e = e[1:]
	}
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	var out string
	if exp >= -4 && exp < 16 {
		if exp >= 0 {
			if len(digits) <= exp+1 {
				out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			} else {
				out = digits[:exp+1] + "." + digits[exp+1:]
			}
		} else {
			out = "0." + strings.Repeat("0", -exp-1) + digits
		}
	} else {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		es := strconv.Itoa(exp)
		if len(es) < 2 {
			es = "0" + es
		}
		out = m + "e" + sign + es
	}
	if neg {
		out = "-" + out
	}
	return out
}
