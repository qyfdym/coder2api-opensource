package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const kiroTestArn = "arn:aws:codewhisperer:us-east-1:123456789012:profile/TEST"

// kiroAcct 测试用 Kiro 账号（token 远未过期、带 profileArn：不触发刷新 / profile 查询）。
func kiroAcct(uid string) *auth.Auth {
	return auth.NewKiro(uid, uid, "at-"+uid, "rt-"+uid, 9999999999, auth.KiroMeta{
		AuthMethod: "social", Provider: "Github", Region: "us-east-1", ProfileArn: kiroTestArn, MachineID: "m-" + uid,
	})
}

// kiroESFrame 编码一帧 AWS event-stream 消息（与 internal/kiro 测试同款，含两段 CRC）。
func kiroESFrame(eventType string, payload any) []byte {
	hdr := func(name, value string) []byte {
		var b bytes.Buffer
		b.WriteByte(byte(len(name)))
		b.WriteString(name)
		b.WriteByte(7)
		_ = binary.Write(&b, binary.BigEndian, uint16(len(value)))
		b.WriteString(value)
		return b.Bytes()
	}
	var hb bytes.Buffer
	hb.Write(hdr(":message-type", "event"))
	hb.Write(hdr(":event-type", eventType))
	hb.Write(hdr(":content-type", "application/json"))
	body, _ := json.Marshal(payload)
	total := 12 + hb.Len() + len(body) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(hb.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))
	msg := append(hb.Bytes(), body...)
	crc := crc32.Update(crc32.Update(0, crc32.IEEETable, prelude), crc32.IEEETable, msg)
	out := append(prelude, msg...)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc)
	return append(out, tail[:]...)
}

// kiroUpstream 假 Kiro 推理入口（经 kiro.Options.Transport 注入）：记录出站请求，回固定的事件流。
type kiroUpstream struct {
	mu     sync.Mutex
	bodies []string
	authz  []string
	stream []byte
}

func (f *kiroUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	resp := func(status int, b []byte) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}, Request: r}
	}
	if r.URL.Host+r.URL.Path != "q.us-east-1.amazonaws.com/generateAssistantResponse" {
		return resp(404, []byte(`{"message":"no route"}`)), nil
	}
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.authz = append(f.authz, r.Header.Get("Authorization"))
	out := f.stream
	f.mu.Unlock()
	return resp(200, out), nil
}

func (f *kiroUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

// kiroChatFake 假 Kiro 账号推理入口（kiroacct.Service.ChatFunc）：按账号 UID 决定返回。
type kiroChatFake struct {
	mu     sync.Mutex
	calls  []string
	models []string
	errOf  map[string]*kiro.Error
}

func (f *kiroChatFake) chat(_ context.Context, cred kiro.Cred, body []byte) (io.ReadCloser, *kiro.Error) {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	f.mu.Lock()
	f.calls = append(f.calls, cred.UID)
	f.models = append(f.models, peek.Model)
	ke := f.errOf[cred.UID]
	f.mu.Unlock()
	if ke != nil {
		return nil, ke
	}
	return io.NopCloser(strings.NewReader(sseOK)), nil
}

func (f *kiroChatFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// offlineKiroTransport 出站一律失败：模型目录回落内置快照，测试不碰网络。
func offlineKiroTransport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("offline test transport: " + r.URL.Host)
	})
}

// newKiroHandler Kiro 账号走 rt（real kiro.Client 协议转换）或 fake.chat（直接给 OpenAI SSE / 错误）。
func newKiroHandler(t *testing.T, up *upstream.Client, rt http.RoundTripper, fake *kiroChatFake, auths ...*auth.Auth) (*Handler, *pool.Pool, *reqlog.Recorder) {
	t.Helper()
	if rt == nil {
		rt = offlineKiroTransport()
	}
	if up == nil {
		up = noWBUpstream(t)
	}
	svc := &kiroacct.Service{
		Client:  kiro.NewClient(kiro.Options{Transport: rt, Logf: t.Logf}),
		AuthDir: t.TempDir(),
	}
	if fake != nil {
		svc.ChatFunc = fake.chat
	}
	p := testPoolWith(auths...)
	rl := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Kiro:         svc,
		SoftCooldown: time.Minute,
		RequestLog:   rl,
		Live:         livecfg.New(livecfg.Snapshot{}),
	})
	return h, p, rl
}

func postKiroChat(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func TestResolveModelDetailKiroPrefix(t *testing.T) {
	realm, bare, explicit := resolveModelDetail("kiro:claude-sonnet-4.5")
	if realm != auth.RealmKiro || bare != "claude-sonnet-4.5" || !explicit {
		t.Fatalf("got realm=%q bare=%q explicit=%v", realm, bare, explicit)
	}
	if otherRealm(auth.RealmKiro) != auth.RealmKiro {
		t.Fatal("kiro has a single routing realm")
	}
}

// 端到端：OpenAI 请求 → Kiro 事件流 → OpenAI SSE / chat.completion；思考强度按档位注入预算，
// 请求记录写「请求档位 → 实际档位」；没要求思考时不注入（Kiro 默认不思考）。
func TestKiroChatStreamAndNonStream(t *testing.T) {
	up := &kiroUpstream{stream: bytes.Join([][]byte{
		kiroESFrame("assistantResponseEvent", map[string]any{"content": "Hello "}),
		kiroESFrame("assistantResponseEvent", map[string]any{"content": "Kiro"}),
		kiroESFrame("metadataEvent", map[string]any{"stopReason": "end_turn"}),
	}, nil)}
	h, p, rl := newKiroHandler(t, nil, up, nil, kiroAcct("k1"))

	rec := postKiroChat(h, `{"model":"kiro:claude-sonnet-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("stream: status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "Hello ") || !strings.Contains(out, "Kiro") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream body: %s", out)
	}
	ob := up.lastBody()
	if !strings.Contains(ob, "thinking_mode") || !strings.Contains(ob, "32768") {
		t.Errorf("high effort must inject a 32768 thinking budget: %.400s", ob)
	}
	if up.authz[0] != "Bearer at-k1" {
		t.Errorf("authorization=%q", up.authz[0])
	}
	if e := lastEvent(t, rl); e.ReasoningEffort != "high" || e.ReasoningEffortOut != "high" || !e.OK {
		t.Errorf("event: requested=%q out=%q ok=%v", e.ReasoningEffort, e.ReasoningEffortOut, e.OK)
	}

	rec = postKiroChat(h, `{"model":"kiro:claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("non-stream: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello Kiro" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
	if strings.Contains(up.lastBody(), "thinking_mode") {
		t.Error("thinking must stay off when the request does not ask for it")
	}
	if e := lastEvent(t, rl); e.ReasoningEffort != "" || e.ReasoningEffortOut != "off" {
		t.Errorf("default effort: requested=%q out=%q", e.ReasoningEffort, e.ReasoningEffortOut)
	}
	if st, _ := p.Status("k1"); st.SuccessCount != 2 || st.Platform != auth.PlatformKiro || st.AuthKind != "Github" {
		t.Errorf("status: success=%d platform=%q auth_kind=%q", st.SuccessCount, st.Platform, st.AuthKind)
	}
}

// 错误 → 账号处置：限流冷却 / 封禁与会话失效停用 / 认证被拒短冷却并标记待刷新 / 额度耗尽硬冷却，
// 都换下一个号；参数错误与容量不足不罚号、不换号。
func TestKiroErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    *kiro.Error
		code   int
		rotate bool
		check  func(t *testing.T, st pool.Status, a *auth.Auth)
	}{
		{"rate_limit", &kiro.Error{Kind: kiro.KindRateLimit, Status: 429, Msg: "throttled"}, 200, true, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if !st.Cooling || st.Disabled {
				t.Errorf("rate-limited account must cool: %+v", st)
			}
		}},
		{"suspended", &kiro.Error{Kind: kiro.KindSuspended, Status: 403, Msg: "TEMPORARILY_SUSPENDED"}, 200, true, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if !st.Disabled {
				t.Errorf("suspended account must be disabled: %+v", st)
			}
		}},
		{"session_dead", &kiro.Error{Kind: kiro.KindSessionDead, Status: 400, Msg: "invalid_grant"}, 200, true, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if !st.Disabled {
				t.Errorf("session-dead account must be disabled: %+v", st)
			}
		}},
		{"auth", &kiro.Error{Kind: kiro.KindAuth, Status: 403, Msg: "bearer rejected"}, 200, true, func(t *testing.T, st pool.Status, a *auth.Auth) {
			if !st.Cooling || st.Disabled {
				t.Errorf("auth-rejected OAuth account must cool (not disable): %+v", st)
			}
			if !kiroacct.NeedsRefresh(a, time.Minute) {
				t.Error("auth-rejected account must be marked for refresh")
			}
		}},
		{"quota", &kiro.Error{Kind: kiro.KindQuota, Status: 402, Msg: "monthly limit"}, 200, true, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if !st.Cooling {
				t.Errorf("quota-exhausted account must cool: %+v", st)
			}
		}},
		{"server", &kiro.Error{Kind: kiro.KindServer, Status: 500, Msg: "boom"}, 200, true, nil},
		{"bad_request", &kiro.Error{Kind: kiro.KindBadRequest, Status: 400, Msg: "Improperly formed request"}, 400, false, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if st.Cooling || st.Disabled {
				t.Errorf("bad request must not penalize: %+v", st)
			}
		}},
		{"busy", &kiro.Error{Kind: kiro.KindBusy, Status: 503, Msg: "INSUFFICIENT_MODEL_CAPACITY"}, 503, false, func(t *testing.T, st pool.Status, _ *auth.Auth) {
			if st.Cooling || st.Disabled {
				t.Errorf("capacity error must not penalize: %+v", st)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &kiroChatFake{errOf: map[string]*kiro.Error{"ka": tc.err}}
			ka, kb := kiroAcct("ka"), kiroAcct("kb")
			h, p, _ := newKiroHandler(t, nil, nil, fake, ka, kb)
			p.SetCredits("ka", 2000, 0) // 确定性源 → 先选 ka
			rec := postChat(h, "kiro:claude-sonnet-4.5", true)
			if rec.Code != tc.code {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.code, rec.Body.String())
			}
			got := fake.called()
			if tc.rotate && (len(got) != 2 || got[0] != "ka" || got[1] != "kb") {
				t.Fatalf("calls=%v want [ka kb]", got)
			}
			if !tc.rotate && (len(got) != 1 || got[0] != "ka") {
				t.Fatalf("calls=%v want exactly [ka]", got)
			}
			for _, m := range fake.models {
				if m != "claude-sonnet-4.5" {
					t.Errorf("outbound model=%q must be the bare name", m)
				}
			}
			if tc.check != nil {
				st, _ := p.Status("ka")
				tc.check(t, st, ka)
			}
		})
	}
}

// API Key 账号被拒没有刷新这条路：直接停用。
func TestKiroAPIKeyAuthDisables(t *testing.T) {
	key := auth.NewKiro("kk", "cli", "ksk_x", "", 0, auth.KiroMeta{AuthMethod: "api_key", Provider: "APIKey", Region: "us-east-1", APIKey: "ksk_x"})
	fake := &kiroChatFake{errOf: map[string]*kiro.Error{"kk": {Kind: kiro.KindAuth, Status: 403, Msg: "revoked"}}}
	h, p, _ := newKiroHandler(t, nil, nil, fake, key, kiroAcct("kb"))
	p.SetCredits("kk", 2000, 0)
	if rec := postChat(h, "kiro:claude-sonnet-4.5", false); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := p.Status("kk"); !st.Disabled {
		t.Fatalf("rejected API key must be disabled: %+v", st)
	}
}

// 平台隔离：kiro: 模型只选 Kiro 账号；池里没有 Kiro 号（回退关闭）→ 503，不落到 WorkBuddy 上游。
func TestKiroNeverRoutesToWorkBuddy(t *testing.T) {
	fake := &kiroChatFake{}
	h, _, _ := newKiroHandler(t, nil, nil, fake, wbAcct("wb1"))
	if rec := postChat(h, "kiro:claude-sonnet-4.5", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.called()) != 0 {
		t.Fatal("no kiro account, kiro chat must not be called")
	}
}

// 跨平台回退：WorkBuddy 号对该模型限流 → 换到 Kiro 同名模型；响应头写明实际服务的模型。
func TestCrossPlatformFallbackWorkBuddyToKiro(t *testing.T) {
	var wbCalls int32
	fake := &kiroChatFake{}
	h, p, rl := newKiroHandler(t, soft6004Upstream(t, &wbCalls), nil, fake, wbAcct("wb1"), kiroAcct("k1"))
	p.SetCrossPlatformFallback(true)
	rec := postChat(h, "cn:claude-sonnet-4.5", true)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Coder2API-Fallback"); got != "kiro:claude-sonnet-4.5" {
		t.Errorf("X-Coder2API-Fallback=%q", got)
	}
	if atomic.LoadInt32(&wbCalls) != 1 || len(fake.called()) != 1 || fake.models[0] != "claude-sonnet-4.5" {
		t.Fatalf("wb calls=%d kiro calls=%v models=%v", wbCalls, fake.called(), fake.models)
	}
	if e := lastEvent(t, rl); e.FallbackModel != "kiro:claude-sonnet-4.5" || !e.OK || !strings.Contains(e.Account, "k1") {
		t.Errorf("event fallback=%q ok=%v account=%q", e.FallbackModel, e.OK, e.Account)
	}

	// 回退关闭：保持原语义，不碰 Kiro。
	p.SetCrossPlatformFallback(false)
	fake2 := &kiroChatFake{}
	h2, p2, _ := newKiroHandler(t, soft6004Upstream(t, &wbCalls), nil, fake2, wbAcct("wb2"), kiroAcct("k2"))
	p2.SetCrossPlatformFallback(false)
	if rec := postChat(h2, "cn:claude-sonnet-4.5", true); rec.Code == 200 || len(fake2.called()) != 0 {
		t.Fatalf("fallback off: status=%d kiro calls=%v", rec.Code, fake2.called())
	}
}

// Claude 档位不串：opus 不会被当成 sonnet 的等价模型。
func TestKiroEquivalentKeepsTier(t *testing.T) {
	h, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{}, kiroAcct("k1"))
	if tg, ok := h.kiroEquivalent("claude-sonnet-4-5"); !ok || tg.bare != "claude-sonnet-4.5" || tg.realm != auth.RealmKiro {
		t.Errorf("same-name (dash version): %+v %v", tg, ok)
	}
	if tg, ok := h.kiroEquivalent("claude-haiku-9"); !ok || !strings.Contains(tg.bare, "haiku") {
		t.Errorf("family match must keep the haiku tier: %+v %v", tg, ok)
	}
	if _, ok := h.kiroEquivalent("deepseek-v4.1-flash"); ok {
		t.Error("deepseek has no kiro equivalent")
	}
}

// /v1/models：池里有 Kiro 账号才列 kiro: 条目，并公布思考档位。
func TestKiroModelsListing(t *testing.T) {
	list := func(h *Handler) map[string]map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("status=%d", rec.Code)
		}
		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, m := range resp.Data {
			if id, _ := m["id"].(string); strings.HasPrefix(id, "kiro:") {
				out[id] = m
			}
		}
		return out
	}
	h, _, _ := newKiroHandler(t, nil, nil, &kiroChatFake{}, kiroAcct("k1"))
	got := list(h)
	m := got["kiro:claude-sonnet-4.5"]
	if m == nil {
		t.Fatalf("kiro:claude-sonnet-4.5 not listed: %v", got)
	}
	if m["owned_by"] != "kiro" {
		t.Errorf("owned_by=%v", m["owned_by"])
	}
	if effs, _ := m["reasoning_supported_efforts"].([]any); len(effs) != len(kiro.SupportedEfforts) {
		t.Errorf("efforts=%v", m["reasoning_supported_efforts"])
	}
	h2, _, _ := newKiroHandler(t, newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }), nil, &kiroChatFake{}, wbAcct("wb1"))
	if got := list(h2); len(got) != 0 {
		t.Errorf("no kiro account, but kiro models listed: %v", got)
	}
}
