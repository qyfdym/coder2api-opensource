package kiro

// Claude 4.6 起的模型：思考由 additionalModelRequestFields 的 thinking.type=adaptive + output_config.effort
// 控制（schema 原文取自 2026-10 的真实目录）。提示词标签对它们无效，档位必须走字段。

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	// claude-opus-5.5：thinking.type 只有 adaptive（关不掉），默认 medium。
	opus55Schema = `{"type":"object","properties":{"thinking":{"type":"object","properties":{"type":{"type":"string","enum":["adaptive"]},"display":{"type":"string","enum":["summarized","omitted"]}},"required":["type"]},"output_config":{"type":"object","properties":{"effort":{"type":"string","enum":["low","medium","high","xhigh","max"],"default":"medium"}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":128000}},"additionalProperties":false}`
	// claude-opus-5：adaptive / disabled，默认 high。
	opus5Schema = `{"type":"object","properties":{"thinking":{"type":"object","properties":{"type":{"type":"string","enum":["adaptive","disabled"]},"display":{"type":"string","enum":["summarized","omitted"]}},"required":["type"]},"output_config":{"type":"object","properties":{"effort":{"type":"string","enum":["low","medium","high","xhigh","max"],"default":"high"}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":128000}},"additionalProperties":false}`
	// claude-opus-4.6：没有 xhigh。
	opus46Schema = `{"type":"object","properties":{"thinking":{"type":"object","properties":{"type":{"type":"string","enum":["adaptive","disabled"]},"display":{"type":"string","enum":["summarized","omitted"]}},"required":["type"]},"output_config":{"type":"object","properties":{"effort":{"type":"string","enum":["low","medium","high","max"],"default":"high"}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":64000}},"additionalProperties":false}`
)

func TestParseEffortSchemaAdaptive(t *testing.T) {
	es := parseEffortSchema(json.RawMessage(opus55Schema))
	if es.field != effortFieldOutputConfig || !reflect.DeepEqual(es.efforts, []string{"low", "medium", "high", "xhigh", "max"}) ||
		es.def != "medium" || !reflect.DeepEqual(es.thinkingTypes, []string{"adaptive"}) {
		t.Errorf("opus-5.5: %+v", es)
	}
	es = parseEffortSchema(json.RawMessage(opus46Schema))
	if es.field != effortFieldOutputConfig || !reflect.DeepEqual(es.efforts, []string{"low", "medium", "high", "max"}) ||
		!reflect.DeepEqual(es.thinkingTypes, []string{"adaptive", "disabled"}) {
		t.Errorf("opus-4.6: %+v", es)
	}
	if es := parseEffortSchema(json.RawMessage(gptSchema)); es.field != effortFieldReasoning || es.def != "high" || es.thinkingTypes != nil {
		t.Errorf("gpt: %+v", es)
	}
}

func adaptiveClient(t *testing.T) (*Client, *fakeUpstream, Cred) {
	t.Helper()
	f := newFake()
	f.on(hostCW+"/ListAvailableModels", func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"models":[
			{"modelId":"claude-opus-5.5","modelName":"Claude Opus 5.5","additionalModelRequestFieldsSchema":` + opus55Schema + `},
			{"modelId":"claude-opus-5","modelName":"Claude Opus 5","additionalModelRequestFieldsSchema":` + opus5Schema + `},
			{"modelId":"claude-opus-4.6","modelName":"Claude Opus 4.6","additionalModelRequestFieldsSchema":` + opus46Schema + `},
			{"modelId":"claude-sonnet-4.5","modelName":"Claude Sonnet 4.5","additionalModelRequestFieldsSchema":null}]}`)
	})
	f.on(routeIDE, streamResp(
		evt("reasoningContentEvent", map[string]any{"text": "thinking hard"}),
		evt("assistantResponseEvent", map[string]any{"content": "512"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	cred := Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}
	if _, err := c.RefreshModels(context.Background(), cred); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	return c, f, cred
}

func TestChatStreamAdaptiveThinking(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		extra      string
		wantFields string // additionalModelRequestFields 的 JSON；"" = 不带
		wantLabel  string
		wantReason bool
	}{
		// Claude Desktop / Claude Code 的写法：thinking.type=enabled（Anthropic 转换层把 adaptive 归一成 enabled）+ 档位。
		{"opus-5.5 max", "claude-opus-5.5", `"thinking":{"type":"enabled"},"reasoning_effort":"max",`,
			`{"output_config":{"effort":"max"},"thinking":{"type":"adaptive"}}`, "max", true},
		{"opus-5.5 medium", "claude-opus-5.5", `"reasoning_effort":"medium",`,
			`{"output_config":{"effort":"medium"},"thinking":{"type":"adaptive"}}`, "medium", true},
		{"dash name routes the same", "claude-opus-5-5", `"reasoning_effort":"high",`,
			`{"output_config":{"effort":"high"},"thinking":{"type":"adaptive"}}`, "high", true},
		{"budget maps to a level", "claude-opus-5.5", `"thinking":{"type":"enabled","budget_tokens":20000},`,
			`{"output_config":{"effort":"high"},"thinking":{"type":"adaptive"}}`, "high", true},
		{"thinking suffix is max", "claude-opus-5.5-thinking", ``,
			`{"output_config":{"effort":"max"},"thinking":{"type":"adaptive"}}`, "max", true},
		// 没指定：不带字段，模型按目录默认档自己思考；思考内容不下发（客户端没要）。
		{"unspecified", "claude-opus-5.5", ``, ``, "", false},
		// opus-5.5 关不掉思考：关闭落到最低档。
		{"opus-5.5 off falls to low", "claude-opus-5.5", `"reasoning_effort":"none",`,
			`{"output_config":{"effort":"low"},"thinking":{"type":"adaptive"}}`, "low", true},
		// 能关的模型发 thinking.type=disabled。
		{"opus-5 off disables", "claude-opus-5", `"thinking":{"type":"disabled"},`,
			`{"thinking":{"type":"disabled"}}`, "off", false},
		// 4.6 系没有 xhigh：落到 high。
		{"opus-4.6 xhigh falls to high", "claude-opus-4.6", `"reasoning_effort":"xhigh",`,
			`{"output_config":{"effort":"high"},"thinking":{"type":"adaptive"}}`, "high", true},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			c, f, cred := adaptiveClient(t)
			body := `{"model":"` + cs.model + `",` + cs.extra + `"messages":[{"role":"user","content":"count"}]}`
			rc, err := c.ChatStream(context.Background(), cred, []byte(body))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			got := collect(func() []map[string]any { fr, _ := sseFrames(t, readAll(t, rc)); return fr }())
			if (got.reasoning != "") != cs.wantReason {
				t.Errorf("reasoning = %q, want kept=%v", got.reasoning, cs.wantReason)
			}
			calls := f.calls(routeIDE)
			if len(calls) != 1 {
				t.Fatalf("upstream calls: %d", len(calls))
			}
			var sent struct {
				Extra json.RawMessage `json:"additionalModelRequestFields"`
			}
			_ = json.Unmarshal(calls[0].Body, &sent)
			if string(sent.Extra) != cs.wantFields {
				t.Errorf("fields = %s, want %s", sent.Extra, cs.wantFields)
			}
			if strings.Contains(string(calls[0].Body), "thinking_mode") {
				t.Errorf("adaptive model must not get the thinking prompt: %.300s", calls[0].Body)
			}
			if label := c.OutboundEffort(cs.model, effortFromBody([]byte(body))); label != cs.wantLabel {
				t.Errorf("OutboundEffort = %q, want %q", label, cs.wantLabel)
			}
		})
	}
}

// 目录里没有 schema 的 Claude（4.5 系）仍走提示词开关，不带字段（带了上游会 400）。
func TestChatStreamPromptThinkingForClaude45(t *testing.T) {
	c, f, cred := adaptiveClient(t)
	rc, err := c.ChatStream(context.Background(), cred,
		[]byte(`{"model":"claude-sonnet-4.5","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, rc)
	body := string(f.calls(routeIDE)[0].Body)
	if strings.Contains(body, "additionalModelRequestFields") || !strings.Contains(body, "max_thinking_length\\u003e32768") {
		t.Errorf("claude-4.5 payload: %.400s", body)
	}
}

func TestAdaptiveEffortInfo(t *testing.T) {
	c, _, _ := adaptiveClient(t)
	m, _ := c.LookupModel("claude-opus-5.5")
	if s, d, can := m.EffortInfo(); !reflect.DeepEqual(s, []string{"low", "medium", "high", "xhigh", "max"}) || d != "medium" || can {
		t.Errorf("opus-5.5: %v %q %v", s, d, can)
	}
	m, _ = c.LookupModel("claude-opus-5")
	if _, d, can := m.EffortInfo(); d != "high" || !can {
		t.Errorf("opus-5: %q %v", d, can)
	}
}
