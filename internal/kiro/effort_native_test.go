package kiro

// 原生思考档位：目录里带 additionalModelRequestFieldsSchema 的模型，思考靠
// additionalModelRequestFields.reasoning.effort 控制，不注入提示词标签。

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const gptSchema = `{"type":"object","additionalProperties":false,"properties":{"reasoning":{"type":"object","properties":{"effort":{"type":"string","enum":["none","low","medium","high","xhigh","max"],"default":"high"}}}}}`
const kimiSchema = `{"type":"object","additionalProperties":false,"properties":{"reasoning":{"type":"object","additionalProperties":false,"properties":{"effort":{"type":"string","enum":["low","high","max"],"default":"max"}}}}}`

func TestNativeEffortSchema(t *testing.T) {
	e, d := nativeEffortSchema(json.RawMessage(gptSchema))
	if !reflect.DeepEqual(e, []string{"none", "low", "medium", "high", "xhigh", "max"}) || d != "high" {
		t.Errorf("gpt schema: %v %q", e, d)
	}
	e, d = nativeEffortSchema(json.RawMessage(kimiSchema))
	if !reflect.DeepEqual(e, []string{"low", "high", "max"}) || d != "max" {
		t.Errorf("kimi schema: %v %q", e, d)
	}
	for _, raw := range []string{``, `null`, `{}`, `not json`, `{"properties":{"other":{}}}`} {
		if e, d := nativeEffortSchema(json.RawMessage(raw)); e != nil || d != "" {
			t.Errorf("schema %q should yield nothing, got %v %q", raw, e, d)
		}
	}
	// 未知档位名被忽略，默认档必须在可选档位里。
	e, d = nativeEffortSchema(json.RawMessage(`{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","ultra"],"default":"ultra"}}}}}`))
	if !reflect.DeepEqual(e, []string{"low"}) || d != "" {
		t.Errorf("unknown level: %v %q", e, d)
	}
}

func TestPickEffort(t *testing.T) {
	gpt := []string{"none", "low", "medium", "high", "xhigh", "max"}
	kimi := []string{"low", "high", "max"}
	cases := []struct {
		supported []string
		want, got string
	}{
		{gpt, "max", "max"},
		{gpt, "none", "none"},
		{kimi, "medium", "low"}, // 不花超出请求的思考量
		{kimi, "xhigh", "high"},
		{kimi, "none", "low"}, // 没有 none：落到最低档
		{kimi, "max", "max"},
	}
	for _, cs := range cases {
		if got := pickEffort(cs.supported, cs.want); got != cs.got {
			t.Errorf("pickEffort(%v, %s) = %q, want %q", cs.supported, cs.want, got, cs.got)
		}
	}
}

// nativeClient 带一份动态目录（gpt 与 kimi 有原生档位，qwen 没有）的客户端 + 记录出站请求体的假上游。
func nativeClient(t *testing.T) (*Client, *fakeUpstream, Cred) {
	t.Helper()
	f := newFake()
	f.on(hostCW+"/ListAvailableModels", func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"models":[
			{"modelId":"gpt-5.6-luna","modelName":"GPT 5.6 Luna","additionalModelRequestFieldsSchema":` + gptSchema + `},
			{"modelId":"kimi-k3","modelName":"Kimi K3","additionalModelRequestFieldsSchema":` + kimiSchema + `},
			{"modelId":"qwen3-coder-next","modelName":"Qwen3 Coder Next","additionalModelRequestFieldsSchema":null}]}`)
	})
	f.on(routeIDE, streamResp(
		evt("reasoningContentEvent", map[string]any{"text": "thinking hard"}),
		evt("assistantResponseEvent", map[string]any{"content": "391"}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	cred := Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}
	if _, err := c.RefreshModels(context.Background(), cred); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	return c, f, cred
}

func TestNativeEffortInModelCatalog(t *testing.T) {
	c, _, _ := nativeClient(t)
	gpt, _ := c.LookupModel("gpt-5.6-luna")
	if len(gpt.Efforts) != 6 || gpt.DefaultEffort != "high" {
		t.Fatalf("gpt model: %+v", gpt)
	}
	supported, def, canDisable := gpt.EffortInfo()
	if !reflect.DeepEqual(supported, []string{"low", "medium", "high", "xhigh", "max"}) || def != "high" || !canDisable {
		t.Errorf("gpt EffortInfo: %v %q %v", supported, def, canDisable)
	}
	kimi, _ := c.LookupModel("kimi-k3")
	supported, def, canDisable = kimi.EffortInfo()
	if !reflect.DeepEqual(supported, []string{"low", "high", "max"}) || def != "max" || canDisable {
		t.Errorf("kimi EffortInfo: %v %q %v", supported, def, canDisable)
	}
	qwen, _ := c.LookupModel("qwen3-coder-next")
	supported, def, canDisable = qwen.EffortInfo()
	if !reflect.DeepEqual(supported, SupportedEfforts) || def != "off" || !canDisable {
		t.Errorf("legacy EffortInfo: %v %q %v", supported, def, canDisable)
	}
}

func TestChatStreamNativeEffort(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		extra      string // 请求体里和思考相关的字段（JSON 片段，可为空）
		wantEffort string // "" = 请求体里不带 additionalModelRequestFields
		wantLabel  string
		wantReason bool // 回包里是否保留思考内容
	}{
		{"gpt low", "gpt-5.6-luna", `"reasoning_effort":"low",`, "low", "low", true},
		{"gpt max via nested reasoning", "gpt-5.6-luna", `"reasoning":{"effort":"max"},`, "max", "max", true},
		{"gpt off", "gpt-5.6-luna", `"reasoning_effort":"none",`, "none", "off", false},
		{"gpt unspecified", "gpt-5.6-luna", ``, "", "", false},
		{"gpt thinking suffix", "gpt-5.6-luna-thinking", ``, "max", "max", true},
		{"gpt anthropic budget", "gpt-5.6-luna", `"thinking":{"type":"enabled","budget_tokens":20000},`, "high", "high", true},
		{"kimi medium falls to low", "kimi-k3", `"reasoning_effort":"medium",`, "low", "low", true},
		{"kimi off falls to low", "kimi-k3", `"reasoning_effort":"off",`, "low", "low", true},
		{"kimi unspecified", "kimi-k3", ``, "", "", false},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			c, f, cred := nativeClient(t)
			body := `{"model":"` + cs.model + `",` + cs.extra + `"messages":[{"role":"user","content":"17*23?"}]}`
			rc, err := c.ChatStream(context.Background(), cred, []byte(body))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			frames, _ := sseFrames(t, readAll(t, rc))
			got := collect(frames)
			if got.content != "391" {
				t.Errorf("content = %q", got.content)
			}
			if (got.reasoning != "") != cs.wantReason {
				t.Errorf("reasoning = %q, want kept=%v", got.reasoning, cs.wantReason)
			}
			calls := f.calls(routeIDE)
			if len(calls) != 1 {
				t.Fatalf("upstream calls: %d", len(calls))
			}
			var sent struct {
				Extra map[string]map[string]string `json:"additionalModelRequestFields"`
			}
			_ = json.Unmarshal(calls[0].Body, &sent)
			if gotEffort := sent.Extra["reasoning"]["effort"]; gotEffort != cs.wantEffort {
				t.Errorf("effort sent = %q, want %q (body %s)", gotEffort, cs.wantEffort, calls[0].Body)
			}
			if strings.Contains(string(calls[0].Body), "thinking_mode") {
				t.Errorf("native model must not get the thinking prompt: %s", calls[0].Body)
			}
			if label := c.OutboundEffort(cs.model, effortFromBody([]byte(body))); label != cs.wantLabel {
				t.Errorf("OutboundEffort = %q, want %q", label, cs.wantLabel)
			}
		})
	}
}

// 没有原生档位的模型仍走提示词开关，也不会带 additionalModelRequestFields。
func TestChatStreamLegacyEffortUnchangedForPlainModel(t *testing.T) {
	c, f, cred := nativeClient(t)
	rc, err := c.ChatStream(context.Background(), cred,
		[]byte(`{"model":"qwen3-coder-next","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, rc)
	body := string(f.calls(routeIDE)[0].Body)
	if strings.Contains(body, "additionalModelRequestFields") || !strings.Contains(body, "max_thinking_length\\u003e32768") {
		t.Errorf("legacy model payload: %s", body)
	}
	if got := c.OutboundEffort("qwen3-coder-next", EffortRequest{Effort: "high"}); got != "high" {
		t.Errorf("OutboundEffort = %q", got)
	}
}
