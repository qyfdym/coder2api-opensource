package kiro

// 对照 kiro.rs 后补的几处边界：带日期 / fable 的模型名、工具 schema 里名字叫 required 的参数、
// 输出过长与上下文用满的收尾、刷新请求的客户端标识。

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParseModelDatedSnapshotsAndFable(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
		"claude-opus-5.5-20260922":   "claude-opus-5.5",
		"claude-opus-5-5":            "claude-opus-5.5",
		"claude-sonnet-4-20250514":   "claude-sonnet-4", // 原有别名
		"claude-fable-5-1":           "claude-fable-5.1",
		"claude-fable-5.1":           "claude-fable-5.1",
		"claude-fable-5-1-20261001":  "claude-fable-5.1",
		"Claude-Sonnet-4-6":          "claude-sonnet-4.6",
		"claude-sonnet-5":            "claude-sonnet-5",
		"gpt-5.6-luna":               "gpt-5.6-luna", // 非 Claude 原样
		"deepseek-3.2":               "deepseek-3.2",
		"qwen3-coder-next":           "qwen3-coder-next",
		"claude-3-5-sonnet-20241022": "claude-sonnet-4.5", // 原有别名
		"my-model-20250101":          "my-model-20250101", // 只对 claude- 开头的去日期
	}
	for in, want := range cases {
		if got, _ := ParseModel(in, "-thinking"); got != want {
			t.Errorf("ParseModel(%q) = %q, want %q", in, got, want)
		}
	}
	if got, thinking := ParseModel("claude-fable-5-1-thinking", "-thinking"); got != "claude-fable-5.1" || !thinking {
		t.Errorf("thinking suffix: %q %v", got, thinking)
	}
	if contextWindow("claude-fable-5.1") != 1_000_000 {
		t.Errorf("fable 5.1 should be a 1M-context model")
	}
}

// 参数名恰好叫 required / additionalProperties 的工具不能被清理掉；null 的 properties / type 要修成对象。
func TestEnsureObjectSchemaKeepsParameterNamesAndFixesNulls(t *testing.T) {
	var in map[string]any
	raw := `{
		"type": "object",
		"additionalProperties": false,
		"required": ["required", "mode", 7, null],
		"properties": {
			"required": {"type": "boolean", "description": "whether it is required"},
			"additionalProperties": {"type": "string"},
			"mode": {"type": "object", "additionalProperties": false, "required": [], "properties": {"x": {"type": "string"}},
				"default": {"required": true, "additionalProperties": false}},
			"items": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": null}}
		}
	}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out := ensureObjectSchema(in).(map[string]any)
	props := out["properties"].(map[string]any)
	if _, ok := props["required"].(map[string]any); !ok {
		t.Errorf("parameter named required was dropped: %v", props)
	}
	if _, ok := props["additionalProperties"].(map[string]any); !ok {
		t.Errorf("parameter named additionalProperties was dropped: %v", props)
	}
	if _, has := out["additionalProperties"]; has {
		t.Errorf("top-level additionalProperties must be removed")
	}
	if !reflect.DeepEqual(out["required"], []any{"required", "mode"}) {
		t.Errorf("required = %v, want only the string names", out["required"])
	}
	mode := props["mode"].(map[string]any)
	if _, has := mode["additionalProperties"]; has {
		t.Errorf("nested additionalProperties must be removed")
	}
	if _, has := mode["required"]; has {
		t.Errorf("empty required must be removed")
	}
	// default 是数据：里面的 required / additionalProperties 不是 schema 关键字，原样保留。
	if def := mode["default"].(map[string]any); def["required"] != true || def["additionalProperties"] != false {
		t.Errorf("default value was altered: %v", def)
	}
	arr := props["items"].(map[string]any)["items"].(map[string]any)
	if _, has := arr["additionalProperties"]; has {
		t.Errorf("additionalProperties inside items must be removed")
	}
	if _, has := arr["required"]; has {
		t.Errorf("null required inside items must be removed")
	}

	broken := ensureObjectSchema(map[string]any{"type": nil, "properties": nil, "required": nil}).(map[string]any)
	if broken["type"] != "object" || !reflect.DeepEqual(broken["properties"], map[string]any{}) {
		t.Errorf("null type / properties not repaired: %v", broken)
	}
	if _, has := broken["required"]; has {
		t.Errorf("null required must be removed: %v", broken)
	}
}

func chatStream(t *testing.T, f *fakeUpstream, body string) (collected, bool, *Error) {
	t.Helper()
	rc, err := testClient(f).ChatStream(context.Background(), Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}, []byte(body))
	if err != nil {
		return collected{}, false, err
	}
	frames, done := sseFrames(t, readAll(t, rc))
	return collect(frames), done, nil
}

// 已有输出之后上游用 ContentLengthExceededException 截断：按长度截断收尾，不是错误。
func TestContentLengthExceededAfterOutputIsLengthFinish(t *testing.T) {
	f := newFake()
	f.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "a very long answer that got cut"}),
		exceptionFrame("ContentLengthExceededException", "output too long"),
	))
	got, done, err := chatStream(t, f, `{"model":"qwen3-coder-next","stream":true,"messages":[{"role":"user","content":"write a lot"}]}`)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if !done || got.errFrame != nil || got.content != "a very long answer that got cut" || got.finish != "length" {
		t.Errorf("stream = done:%v err:%v content:%q finish:%q", done, got.errFrame, got.content, got.finish)
	}
	// 非流式同样：完整返回已有输出，finish_reason=length。
	f2 := newFake()
	f2.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "cut answer"}),
		exceptionFrame("ContentLengthExceededException", "output too long"),
	))
	got, _, err = chatStream(t, f2, `{"model":"qwen3-coder-next","messages":[{"role":"user","content":"write a lot"}]}`)
	if err != nil || got.content != "cut answer" || got.finish != "length" {
		t.Errorf("buffered = err:%+v content:%q finish:%q", err, got.content, got.finish)
	}
}

// 还没有任何输出就报 ContentLengthExceededException：是输入过长，不重试、不换入口。
func TestContentLengthExceededBeforeOutputIsTooLarge(t *testing.T) {
	f := newFake()
	f.on(routeIDE, streamResp(exceptionFrame("ContentLengthExceededException", "input too long")))
	_, _, err := chatStream(t, f, `{"model":"qwen3-coder-next","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if err == nil || err.Kind != KindTooLarge {
		t.Fatalf("err = %+v, want KindTooLarge", err)
	}
	if n := len(f.calls(routeIDE)) + len(f.calls(routeCW)); n != 1 {
		t.Errorf("upstream calls = %d, a too-large request must not be retried on other endpoints", n)
	}
}

// 上下文用满（>=100%）且上游没给结束原因：按长度截断收尾，而不是当成流中断。
func TestContextFullWithoutStopReasonIsLengthFinish(t *testing.T) {
	f := newFake()
	f.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "partial"}),
		evt("contextUsageEvent", map[string]any{"contextUsagePercentage": 100.0}),
	))
	got, done, err := chatStream(t, f, `{"model":"qwen3-coder-next","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil || !done || got.errFrame != nil || got.finish != "length" {
		t.Errorf("err:%+v done:%v errFrame:%v finish:%q", err, done, got.errFrame, got.finish)
	}
	// 上游给了结束原因就以它为准。
	f2 := newFake()
	f2.on(routeIDE, streamResp(
		evt("assistantResponseEvent", map[string]any{"content": "full answer"}),
		evt("contextUsageEvent", map[string]any{"contextUsagePercentage": 100.0}),
		evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	))
	got, _, err = chatStream(t, f2, `{"model":"qwen3-coder-next","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil || got.finish != "stop" {
		t.Errorf("explicit stopReason must win: err:%+v finish:%q", err, got.finish)
	}
}

// 令牌刷新带 Kiro IDE 的客户端标识，不用 Go 的默认 User-Agent。
func TestRefreshSendsKiroClientIdentity(t *testing.T) {
	f := newFake()
	const social = "prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	const oidc = "oidc.us-east-1.amazonaws.com/token"
	f.on(social, func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}`)
	})
	f.on(oidc, func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}`)
	})
	c := testClient(f)
	if _, err := c.Refresh(context.Background(), Cred{AuthMethod: "social", RefreshToken: "rt", MachineID: "mid-9"}); err != nil {
		t.Fatalf("social refresh: %v", err)
	}
	if _, err := c.Refresh(context.Background(), Cred{AuthMethod: "idc", RefreshToken: "rt", ClientID: "cid", ClientSecret: "cs", Region: "us-east-1", MachineID: "mid-9"}); err != nil {
		t.Fatalf("idc refresh: %v", err)
	}
	s := f.calls(social)
	if len(s) != 1 || s[0].Header.Get("User-Agent") != "KiroIDE-1.3.11-mid-9" {
		t.Errorf("social refresh User-Agent = %v", s)
	}
	o := f.calls(oidc)
	if len(o) != 1 {
		t.Fatalf("idc refresh calls: %d", len(o))
	}
	ua := o[0].Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "aws-sdk-js/") || !strings.Contains(ua, "api/sso-oidc#") || !strings.HasSuffix(ua, " KiroIDE") ||
		!strings.HasPrefix(o[0].Header.Get("x-amz-user-agent"), "aws-sdk-js/") || o[0].Header.Get("amz-sdk-invocation-id") == "" {
		t.Errorf("idc refresh headers: %v", o[0].Header)
	}
}
