package kiro

// contextUsagePercentage 是相对模型 maxInputTokens 的百分比（真机实测：qwen3-coder-next 255K ≈ 256000、
// deepseek-3.2 163K ≈ 164000、gpt-5.6-luna 920K ≈ 922000），不能一律按 200K 换算输入 token。

import (
	"context"
	"net/http"
	"testing"
)

func TestPromptTokensUseCatalogContextWindow(t *testing.T) {
	f := newFake()
	f.on(hostCW+"/ListAvailableModels", func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"models":[
			{"modelId":"qwen3-coder-next","modelName":"Qwen","tokenLimits":{"maxInputTokens":256000,"maxOutputTokens":64000}},
			{"modelId":"gpt-5.6-luna","modelName":"GPT","tokenLimits":{"maxInputTokens":922000,"maxOutputTokens":128000}}]}`)
	})
	f.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "OK"}),
		evt("contextUsageEvent", map[string]any{"contextUsagePercentage": 1.0}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	c := testClient(f)
	cred := Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}
	if _, err := c.RefreshModels(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model string
		want  float64
	}{
		{"qwen3-coder-next", 2560},  // 目录窗口 256000
		{"gpt-5.6-luna", 9220},      // 目录窗口 922000
		{"claude-sonnet-4.5", 2000}, // 目录里没有：按模型名估 200K
		{"claude-opus-4.6", 10000},  // 目录里没有：按模型名估 1M
	}
	for _, cs := range cases {
		rc, err := c.ChatStream(context.Background(), cred, []byte(`{"model":"`+cs.model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("%s: %v", cs.model, err)
		}
		frames, _ := sseFrames(t, readAll(t, rc))
		got := collect(frames)
		if got.usage == nil || got.usage["prompt_tokens"].(float64) != cs.want {
			t.Errorf("%s: usage = %+v, want prompt_tokens %v", cs.model, got.usage, cs.want)
		}
	}
}
