package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestMatchModelFamily(t *testing.T) {
	qoderNames := []string{"Auto", "Qwen3.8-Max", "Qwen3.8-Flash", "DeepSeek-V4-Pro", "DeepSeek-Flash", "GLM-5.3", "GLM-5.3-Flash", "Kimi-K3"}
	wbNames := []string{"deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4.1-flash-sg", "deepseek-v4-pro", "glm-5.2", "glm-5.3-flash", "hy4-preview"}
	for _, tc := range []struct {
		src   string
		cands []string
		want  string
	}{
		{"deepseek-v4.1-flash", qoderNames, "DeepSeek-Flash"}, // 名字不同，同厂商同档位
		{"deepseek-v4-pro", qoderNames, "DeepSeek-V4-Pro"},
		{"glm-5.3-flash", qoderNames, "GLM-5.3-Flash"},
		{"DeepSeek-Flash", wbNames, "deepseek-v4.1-flash"}, // 多个候选：版本最高，再取名字最短
		{"DeepSeek-V4-Pro", wbNames, "deepseek-v4-pro"},
		{"hy4-preview", qoderNames, ""},            // Qoder 没有混元：不回退
		{"Auto", wbNames, ""},                      // 不认识的厂商：不回退
		{"Qwen3.8-Max", wbNames, ""},               // WorkBuddy 没有 Qwen：不回退
		{"glm-5.2", []string{"GLM-5.3-Flash"}, ""}, // 档位不同（无档位 vs flash）不算等价
	} {
		got, ok := matchModelFamily(tc.src, tc.cands)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("matchModelFamily(%q) = %q,%v want %q", tc.src, got, ok, tc.want)
		}
	}
}

// soft6004Upstream WorkBuddy 上游对任何请求都回 429 6004（带未来的重置时间）→ 该模型在该号上冷却。
func soft6004Upstream(t *testing.T, calls *int32) *upstream.Client {
	reset := time.Now().In(time.FixedZone("CST", 8*3600)).Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	return newFakeUpstream(t, func(string) (int, string, bool) {
		atomic.AddInt32(calls, 1)
		return 429, `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` + reset + ` UTC+8 重置，您也可以切换其他模型继续使用。"}`, false
	})
}

func newFallbackHandler(t *testing.T, up *upstream.Client, fake *qoderChatFake, mapping map[string]string, auths ...*auth.Auth) (*Handler, *reqlog.Recorder) {
	t.Helper()
	p := testPoolWith(auths...)
	p.SetCrossPlatformFallback(true)
	rl := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Qoder: &qoderacct.Service{
			Client:   offlineQoderClient(t),
			AuthDir:  t.TempDir(),
			ChatFunc: fake.chat,
		},
		QoderDefaultRealm: auth.RealmQoderCN,
		SoftCooldown:      time.Minute,
		RequestLog:        rl,
		Live:              livecfg.New(livecfg.Snapshot{ModelFallback: livecfg.NormalizeModelFallback(mapping)}),
	})
	return h, rl
}

func wbAcct(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Nickname: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

func lastEvent(t *testing.T, rl *reqlog.Recorder) reqlog.Event {
	t.Helper()
	s := rl.Snapshot()
	if len(s.Recent) == 0 {
		t.Fatal("no request event recorded")
	}
	// Recent 新的在前还是在后与本测试无关：取 RequestID 最大者不可靠，直接按时间取最新。
	e := s.Recent[0]
	for _, x := range s.Recent {
		if x.Time.After(e.Time) {
			e = x
		}
	}
	return e
}

// 复现线上问题：cn:deepseek-v4.1-flash 在 WorkBuddy 号上 6004 → 该号对该模型冷却 →
// 之前回 400 model_unavailable；现在应自动换到 Qoder 的 DeepSeek-Flash。
func TestCrossPlatformFallbackWorkBuddyModelCooling(t *testing.T) {
	var wbCalls int32
	fake := &qoderChatFake{}
	h, rl := newFallbackHandler(t, soft6004Upstream(t, &wbCalls), fake, nil,
		wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))

	rec := postChat(h, "cn:deepseek-v4.1-flash", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Coder2API-Fallback"); got != "qoder-cn:DeepSeek-Flash" {
		t.Errorf("X-Coder2API-Fallback=%q", got)
	}
	if atomic.LoadInt32(&wbCalls) != 1 {
		t.Errorf("wb calls=%d want 1", wbCalls)
	}
	if len(fake.models) != 1 || fake.models[0] != "DeepSeek-Flash" {
		t.Fatalf("qoder outbound models=%v want [DeepSeek-Flash]", fake.models)
	}
	e := lastEvent(t, rl)
	if e.Model != "cn:deepseek-v4.1-flash" || e.FallbackModel != "qoder-cn:DeepSeek-Flash" || !e.OK {
		t.Errorf("event model=%q fallback=%q ok=%v", e.Model, e.FallbackModel, e.OK)
	}
	if !strings.Contains(e.Account, "qc1") || !strings.Contains(e.Note, "已回退") {
		t.Errorf("event account=%q note=%q", e.Account, e.Note)
	}

	// 第二次：WorkBuddy 号对该模型已在冷却 → 不再打 WorkBuddy 上游，直接回退。
	rec = postChat(h, "cn:deepseek-v4.1-flash", false)
	if rec.Code != 200 {
		t.Fatalf("second: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&wbCalls) != 1 {
		t.Errorf("model-cooled wb account was called again: calls=%d", wbCalls)
	}
	if e := lastEvent(t, rl); !strings.Contains(e.Note, "冷却") || !strings.Contains(e.Note, "wb1") {
		t.Errorf("note should name the cooling account: %q", e.Note)
	}
}

// 用户手动禁用 WorkBuddy 号后同一个模型名也应走 Qoder（之前是 503）。
func TestCrossPlatformFallbackWorkBuddyDisabled(t *testing.T) {
	var wbCalls int32
	fake := &qoderChatFake{}
	h, _ := newFallbackHandler(t, soft6004Upstream(t, &wbCalls), fake, nil,
		wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))
	h.cfg.Pool.Disable("wb1", "test: manual")
	if rec := postChat(h, "cn:deepseek-v4.1-flash", true); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if wbCalls != 0 || len(fake.calls) != 1 {
		t.Fatalf("wb calls=%d qoder calls=%v", wbCalls, fake.calls)
	}
}

// 开关关闭 → 不回退，保持原有错误语义；请求记录写明没选到号的原因。
func TestCrossPlatformFallbackSwitchOff(t *testing.T) {
	var wbCalls int32
	fake := &qoderChatFake{}
	h, rl := newFallbackHandler(t, soft6004Upstream(t, &wbCalls), fake, nil,
		wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))
	h.cfg.Pool.SetCrossPlatformFallback(false)
	h.cfg.Pool.Disable("wb1", "test: manual")
	rec := postChat(h, "cn:deepseek-v4.1-flash", true)
	if rec.Code != http.StatusServiceUnavailable || len(fake.calls) != 0 {
		t.Fatalf("status=%d qoder calls=%v", rec.Code, fake.calls)
	}
	e := lastEvent(t, rl)
	if e.Account != "" || !strings.Contains(e.Note, "无可用账号") {
		t.Errorf("event account=%q note=%q", e.Account, e.Note)
	}
}

// models.fallback 显式映射优先；值为空串 = 禁止回退。
func TestCrossPlatformFallbackMapping(t *testing.T) {
	var wbCalls int32
	fake := &qoderChatFake{}
	h, _ := newFallbackHandler(t, soft6004Upstream(t, &wbCalls), fake,
		map[string]string{"CN:DeepSeek-V4.1-Flash": "qoder:Qwen3.8-Flash", "glm-5.2": ""},
		wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))
	h.cfg.Pool.Disable("wb1", "test: manual")

	if rec := postChat(h, "cn:deepseek-v4.1-flash", true); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.models) != 1 || fake.models[0] != "Qwen3.8-Flash" {
		t.Fatalf("mapping not applied: %v", fake.models)
	}

	rec := postChat(h, "glm-5.2", true)
	if rec.Code == 200 || len(fake.calls) != 1 {
		t.Fatalf("empty mapping must block fallback: status=%d calls=%v", rec.Code, fake.calls)
	}
}

// 另一平台没有等价模型：不回退，错误提示里说明可以配 models.fallback。
func TestCrossPlatformFallbackNoEquivalent(t *testing.T) {
	var wbCalls int32
	fake := &qoderChatFake{}
	h, rl := newFallbackHandler(t, soft6004Upstream(t, &wbCalls), fake, nil,
		wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))
	h.cfg.Pool.Disable("wb1", "test: manual")
	rec := postChat(h, "cn:hy4-preview", true)
	if rec.Code == 200 || len(fake.calls) != 0 {
		t.Fatalf("status=%d calls=%v", rec.Code, fake.calls)
	}
	if !strings.Contains(rec.Body.String(), "models.fallback") {
		t.Errorf("hint should mention models.fallback: %s", rec.Body.String())
	}
	if e := lastEvent(t, rl); !strings.Contains(e.Note, "没有等价模型") {
		t.Errorf("note=%q", e.Note)
	}
}

// 反方向：Qoder 号限流 → 回退到 WorkBuddy 同名模型（目录走 /v1/models 同一份缓存）。
func TestCrossPlatformFallbackQoderToWorkBuddy(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v4.1-flash"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}()
	var wbModel atomic.Value
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	inner := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			b := make([]byte, 4096)
			n, _ := r.Body.Read(b)
			wbModel.Store(string(b[:n]))
		}
		return inner.RoundTrip(r)
	})
	fake := &qoderChatFake{errOf: map[string]*qoder.Error{"qc1": {Kind: qoder.KindRateLimit, Status: 429, Msg: "too many"}}}
	h, _ := newFallbackHandler(t, up, fake, nil, wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))

	rec := postChat(h, "qoder-cn:GLM-5.2", true)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Coder2API-Fallback"); got != "cn:glm-5.2" {
		t.Errorf("X-Coder2API-Fallback=%q", got)
	}
	if s, _ := wbModel.Load().(string); !strings.Contains(s, `"glm-5.2"`) {
		t.Errorf("workbuddy outbound body should carry glm-5.2: %.200s", s)
	}
}

// 请求本身的问题（参数非法）不回退：换平台也一样会失败。
func TestCrossPlatformFallbackSkipsBadRequest(t *testing.T) {
	var badCalls int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		atomic.AddInt32(&badCalls, 1)
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, false
	})
	fake := &qoderChatFake{}
	h, _ := newFallbackHandler(t, up, fake, nil, wbAcct("wb1"), qoderAcct("qc1", qoder.RealmCN))
	rec := postChat(h, "cn:deepseek-v4.1-flash", true)
	if rec.Code != 400 || len(fake.calls) != 0 {
		t.Fatalf("status=%d qoder calls=%v body=%s", rec.Code, fake.calls, rec.Body.String())
	}
}
