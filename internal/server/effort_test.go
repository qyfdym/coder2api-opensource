package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func TestEffortPeekInfo(t *testing.T) {
	for _, tc := range []struct {
		body              string
		requested, wb, qd string
		thinking          string
	}{
		{`{"reasoning_effort":"High"}`, "high", "high", "high", ""},
		{`{"reasoningEffort":"low"}`, "low", "low", "", ""},
		{`{"reasoning":{"effort":"medium"}}`, "medium", "", "medium", ""},
		{`{"thinking":{"type":"enabled","effort":"max"}}`, "max", "", "max", "enabled"},
		{`{"thinking":{"type":"disabled"}}`, "off", "", "", "disabled"},
		{`{"reasoning_effort":"none"}`, "off", "none", "none", ""},
		{`{"reasoning":"weird","thinking":42}`, "", "", "", ""}, // 类型不符：不报错、不取值
		{`{"reasoning":"  "}`, "", "", "", ""},
		{`{}`, "", "", "", ""},
	} {
		var p struct{ effortPeek }
		if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		ei := p.effortPeek.info()
		if ei.requested != tc.requested || ei.wb.Effort != tc.wb || ei.qd != tc.qd || ei.wb.ThinkingType != tc.thinking {
			t.Errorf("%s: got requested=%q wb=%q qd=%q thinking=%q", tc.body, ei.requested, ei.wb.Effort, ei.qd, ei.wb.ThinkingType)
		}
	}
}

func postRawChat(h *Handler, body string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec.Code
}

// 请求记录带「请求的档位」与「实际生效的档位」：WorkBuddy DeepSeek 没指定 → 网关补默认档。
func TestRequestRecordEffortWorkBuddy(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	rl := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: testPoolWith(wbAcct("wb1")), Upstream: up, RequestLog: rl})

	if code := postRawChat(h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[]}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	e := lastEvent(t, rl)
	if e.ReasoningEffort != "" || e.ReasoningEffortOut != "high" {
		t.Errorf("default injection: requested=%q out=%q", e.ReasoningEffort, e.ReasoningEffortOut)
	}

	if code := postRawChat(h, `{"model":"glm-5.2","stream":true,"reasoning_effort":"MEDIUM","messages":[]}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	e = lastEvent(t, rl)
	if e.ReasoningEffort != "medium" || e.ReasoningEffortOut != "medium" {
		t.Errorf("explicit: requested=%q out=%q", e.ReasoningEffort, e.ReasoningEffortOut)
	}
}

// Qoder：按官方目录归一；没选到号时只有请求档位，没有实际档位。
func TestRequestRecordEffortQoder(t *testing.T) {
	fake := &qoderChatFake{}
	h, rl := newFallbackHandler(t, noWBUpstream(t), fake, nil, qoderAcct("qc1", qoder.RealmCN))
	h.cfg.Pool.SetCrossPlatformFallback(false)

	want := h.cfg.Qoder.Client.OutboundEffort(qoder.RealmCN, "DeepSeek-Flash", "medium")
	if want == "" {
		t.Skip("catalog snapshot has no effort levels for DeepSeek-Flash")
	}
	if code := postRawChat(h, `{"model":"qoder-cn:DeepSeek-Flash","stream":true,"reasoning_effort":"medium","messages":[]}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	e := lastEvent(t, rl)
	if e.ReasoningEffort != "medium" || e.ReasoningEffortOut != want {
		t.Errorf("requested=%q out=%q want out %q", e.ReasoningEffort, e.ReasoningEffortOut, want)
	}
	// 没指定：记模型默认档（上游按默认档运行）。
	if code := postRawChat(h, `{"model":"qoder-cn:DeepSeek-Flash","stream":true,"messages":[]}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if e = lastEvent(t, rl); e.ReasoningEffort != "" || e.ReasoningEffortOut == "" {
		t.Errorf("default: requested=%q out=%q", e.ReasoningEffort, e.ReasoningEffortOut)
	}

	if code := postRawChat(h, `{"model":"qoder-intl:DeepSeek-Flash","stream":true,"reasoning_effort":"high","messages":[]}`); code == 200 {
		t.Fatalf("intl has no account: code=%d", code)
	}
	e = lastEvent(t, rl)
	if e.ReasoningEffort != "high" || e.ReasoningEffortOut != "" {
		t.Errorf("no account: requested=%q out=%q", e.ReasoningEffort, e.ReasoningEffortOut)
	}
}
