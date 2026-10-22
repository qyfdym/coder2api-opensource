package qoder

// 流式帧的公共基建 + 请求侧工具归一。移植自 qoder2api-hub（MIT）qoder_proxy.py：
// strip_data_prefix / clean_chunk / _new_id / _leak_chunk_frame /
// _frame_without_content / normalize_tool_choice / normalize_tools /
// translate_max_completion_tokens / _tools_for_chat / _downgrade_custom_tool。

import (
	"strings"
)

// noiseKeys clean_chunk 要剔除的空占位字段（值为空才剔，非空保留）。
var noiseKeys = []string{"extra_fields", "refusal", "reasoning_content"}

// newID 生成形如 call_<32hex> 的稳定前缀 id（_new_id）。
func newID(prefix string) string { return prefix + RandomHex(16) }

// stripDataPrefix 去掉 SSE 行的 "data:" 前缀；注释行 / 空行返回 ""（strip_data_prefix）。
func stripDataPrefix(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ":") {
		return ""
	}
	for strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(line[5:])
	}
	if line == "" || strings.HasPrefix(line, ":") {
		return ""
	}
	return line
}

// cleanChunk 剔除上游 delta 里的空占位字段（clean_chunk）。
// 返回 ""：该帧只剩空 delta 且无 finish_reason，应跳过；
// 否则返回规整后的 JSON（有改动）或原字符串（无改动，逐字节保留）。
func cleanChunk(raw string) string {
	obj, err := parseJString(raw)
	if err != nil || !obj.isObj() {
		return raw
	}
	changed := false
	choices := obj.get("choices")
	if choices.isArr() {
		for _, choice := range choices.arr {
			if !choice.isObj() {
				continue
			}
			delta := choice.get("delta")
			if !delta.isObj() {
				continue
			}
			if fc := delta.get("function_call"); fc != nil {
				var empty bool
				if fc.isObj() {
					empty = !fc.get("name").truthy()
				} else {
					empty = !fc.truthy()
				}
				if empty {
					delta.del("function_call")
					changed = true
				}
			}
			if tc := delta.get("tool_calls"); tc.isArr() && len(tc.arr) == 0 {
				delta.del("tool_calls")
				changed = true
			}
			for _, k := range noiseKeys {
				if v := delta.get(k); v != nil && !v.truthy() {
					delta.del(k)
					changed = true
				}
			}
			if len(delta.keys) == 0 && !choice.get("finish_reason").truthy() {
				return ""
			}
		}
	}
	if changed {
		return obj.dumpPy()
	}
	return raw
}

// metaState 流式回读合成帧时复用的上游 id / model / created。
type metaState struct {
	id      string
	model   string
	created *jval
}

func (m *metaState) absorb(payload *jval) {
	if s := payload.get("id").str(); s != "" {
		m.id = s
	}
	if s := payload.get("model").str(); s != "" {
		m.model = s
	}
	if c := payload.get("created"); c != nil && c.k == jNum {
		m.created = c
	}
}

func (m *metaState) createdVal() *jval {
	if m.created != nil {
		return m.created
	}
	return jIntV(nowUnix())
}

// leakChunkFrame 按 chat.completion.chunk 形态合成一帧（_leak_chunk_frame）。
// fin 为 nil 时 finish_reason 输出 null。
func leakChunkFrame(m *metaState, delta *jval, fin *string) []byte {
	id := m.id
	if id == "" {
		id = "chatcmpl-qoder"
	}
	var finV *jval
	if fin == nil {
		finV = jNullV()
	} else {
		finV = jStrV(*fin)
	}
	choice := jObjV("index", int64(0), "delta", delta, "finish_reason", finV)
	payload := jObjV(
		"id", id,
		"object", "chat.completion.chunk",
		"created", m.createdVal(),
		"model", m.model,
		"choices", jArrV(choice),
	)
	return []byte("data: " + payload.dumpPy() + "\n\n")
}

// frameWithoutContent 复制一帧并去掉 delta.content（_frame_without_content）。
func frameWithoutContent(payload *jval) *jval {
	out := payload.shallow()
	choices := payload.get("choices")
	if !choices.isArr() {
		return out
	}
	newChoices := jArrV()
	for _, ch := range choices.arr {
		if !ch.isObj() {
			newChoices.arr = append(newChoices.arr, ch)
			continue
		}
		ch2 := ch.shallow()
		if delta := ch2.get("delta"); delta.isObj() && delta.has("content") {
			d2 := delta.shallow()
			d2.del("content")
			ch2.set("delta", d2)
		}
		newChoices.arr = append(newChoices.arr, ch2)
	}
	out.set("choices", newChoices)
	return out
}

// frameSource 流式转换流水线的拉取接口：返回 (帧, nil) / (nil, io.EOF) / (nil, err)。
// err 为非 EOF 的真实错误（传输层 / 信封非 200 / 协议级失败），调用方据此停止并分类。
type frameSource interface {
	next() ([]byte, error)
}

// sliceSource 把已缓冲的帧序列（优先回放）接在另一个 source 前面（流式回放首帧用）。
type sliceSource struct {
	frames [][]byte
	i      int
	rest   frameSource
}

func (s *sliceSource) next() ([]byte, error) {
	if s.i < len(s.frames) {
		f := s.frames[s.i]
		s.i++
		return f, nil
	}
	if s.rest == nil {
		return nil, errEOF
	}
	return s.rest.next()
}

// ---------------------------------------------------------------------------
// 请求侧工具归一
// ---------------------------------------------------------------------------

// customToolHint 把 Responses custom freeform 工具降级成单 input 参数的 function
// 工具时追加的说明（_downgrade_custom_tool / CUSTOM_TOOL_HINT）。
const customToolHint = "This is a freeform tool. Put the COMPLETE raw payload into the single " +
	"'input' string parameter, verbatim. Do not wrap it in JSON, do not wrap " +
	"it in markdown code fences, do not add commentary."

func isCustomTool(t *jval) bool {
	return t.isObj() && strings.ToLower(t.get("type").str()) == "custom"
}

// downgradeCustomTool 把 Responses custom 工具改写成 Chat function 工具。
func downgradeCustomTool(t *jval) *jval {
	desc := t.get("description").str()
	extra := ""
	if fmtv := t.get("format"); fmtv.isObj() {
		if def := fmtv.get("definition"); def.truthy() {
			extra = "\n\nGrammar:\n" + jToStr(def)
		} else if syn := fmtv.get("syntax"); syn.truthy() {
			extra = "\n\nGrammar syntax: " + jToStr(syn)
		}
	}
	fullDesc := strings.TrimSpace(desc + "\n\n" + customToolHint + extra)
	props := jObjV("input", jObjV("type", "string",
		"description", "Complete raw payload for this tool, verbatim."))
	params := jObjV("type", "object", "properties", props, "required", jArrV(jStrV("input")))
	return jObjV("type", "function", "name", t.get("name").str(),
		"description", fullDesc, "parameters", params)
}

func jToStr(v *jval) string {
	if v.isStr() {
		return v.s
	}
	return v.dumpPy()
}

// toolsForChat 只保留 Qoder 上游能表达的工具（_tools_for_chat）：
// custom → 降级为单 input function；function / 无 type → 原样；其它类型丢弃。
func toolsForChat(tools *jval) *jval {
	out := jArrV()
	if !tools.isArr() {
		return out
	}
	for _, t := range tools.arr {
		if !t.isObj() {
			continue
		}
		if isCustomTool(t) {
			out.arr = append(out.arr, downgradeCustomTool(t))
			continue
		}
		kind := t.get("type")
		if kind == nil || kind.isNull() || kind.str() == "function" {
			out.arr = append(out.arr, t)
		}
	}
	return out
}

// normalizeToolChoice 把 OpenAI tool_choice 归一成上游可接受形态（normalize_tool_choice）。
// 默认保留 tools、把「本轮不许调用」写成字符串 "none"（TOOLCHOICE_NONE_DROPS_TOOLS 默认关）。
func normalizeToolChoice(obj *jval) {
	tc := obj.get("tool_choice")
	if tc == nil {
		return
	}
	if tc.isStr() {
		if strings.ToLower(strings.TrimSpace(tc.s)) == "none" {
			obj.set("tool_choice", jStrV("none"))
		}
		return
	}
	if tc.isObj() {
		typ := strings.ToLower(strings.TrimSpace(tc.get("type").str()))
		switch typ {
		case "none":
			obj.set("tool_choice", jStrV("none"))
		case "auto", "required":
			obj.set("tool_choice", jStrV(typ))
		case "function":
			name := tc.get("function").get("name").str()
			if name == "" {
				name = tc.get("name").str()
			}
			name = strings.TrimSpace(name)
			if name == "" {
				name = "auto"
			}
			obj.set("tool_choice", jStrV(name))
		default:
			obj.del("tool_choice")
		}
		return
	}
	obj.del("tool_choice")
}

// normalizeTools 把顶层 name 型工具包成 Chat function schema；丢弃上游无法表达的类型
// （normalize_tools）。
func normalizeTools(obj *jval) {
	tools := obj.get("tools")
	if !tools.isArr() || len(tools.arr) == 0 {
		return
	}
	norm := jArrV()
	for _, t := range tools.arr {
		if !t.isObj() {
			continue
		}
		kind := t.get("type")
		if !(kind == nil || kind.isNull() || kind.str() == "function") {
			continue // namespace / web_search 等上游无法表达，丢弃
		}
		if t.has("name") && !t.has("function") && t.get("type").str() == "function" {
			fn := jObjV("name", t.get("name").str(),
				"description", t.get("description").str(),
				"parameters", orObj(t.get("parameters")))
			if t.has("strict") {
				fn.set("strict", t.get("strict"))
			}
			norm.arr = append(norm.arr, jObjV("type", "function", "function", fn))
		} else {
			norm.arr = append(norm.arr, t)
		}
	}
	obj.set("tools", norm)
}

func orObj(v *jval) *jval {
	if v.isObj() {
		return v
	}
	return jObjV()
}

// translateMaxCompletionTokens max_completion_tokens 别名翻译为 max_tokens
// （translate_max_completion_tokens；已有 max_tokens 时不覆盖）。
func translateMaxCompletionTokens(obj *jval) {
	alias := obj.del("max_completion_tokens")
	if alias == nil || alias.isNull() {
		return
	}
	if obj.has("max_tokens") {
		return
	}
	if f, ok := alias.num(); ok && f > 0 {
		obj.set("max_tokens", jIntV(int64(f)))
	}
}
