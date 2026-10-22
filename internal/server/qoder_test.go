package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// qoderChatFake 假 Qoder 推理入口：按账号 UID 决定返回（nil 错误 = 成功 SSE），并记录调用顺序。
type qoderChatFake struct {
	mu     sync.Mutex
	calls  []string
	models []string // 每次调用出站 body 里的 model（验证回退时改写成了 Qoder 模型名）
	errOf  map[string]*qoder.Error
}

func (f *qoderChatFake) chat(_ context.Context, cred qoder.Cred, body []byte) (io.ReadCloser, *qoder.Error) {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	f.mu.Lock()
	f.calls = append(f.calls, cred.UID)
	f.models = append(f.models, peek.Model)
	qe := f.errOf[cred.UID]
	f.mu.Unlock()
	if qe != nil {
		return nil, qe
	}
	return io.NopCloser(strings.NewReader(sseOK)), nil
}

func (f *qoderChatFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// qoderAcct 测试用 Qoder 账号（token 远未过期，不触发刷新）。
func qoderAcct(uid, region string) *auth.Auth {
	return auth.NewQoder(uid, uid, region, "at-"+uid, "drt-"+uid, 9999999999, auth.QoderMeta{})
}

// noWBUpstream WorkBuddy 上游一旦被调用即判失败：Qoder 请求绝不能落到 WorkBuddy 端点。
func noWBUpstream(t *testing.T) *upstream.Client {
	return newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("WorkBuddy upstream must not be called (authz=%q)", authz)
		return 500, `{}`, false
	})
}

func newQoderHandler(t *testing.T, fake *qoderChatFake, hidden []string, auths ...*auth.Auth) (*Handler, *pool.Pool) {
	t.Helper()
	p := testPoolWith(auths...)
	svc := &qoderacct.Service{
		Client:   offlineQoderClient(t),
		AuthDir:  t.TempDir(),
		ChatFunc: fake.chat,
	}
	h := NewHandler(Config{
		Pool:              p,
		Upstream:          noWBUpstream(t),
		Qoder:             svc,
		QoderDefaultRealm: auth.RealmQoderCN,
		SoftCooldown:      time.Minute,
		ModelsHidden:      hidden,
		Live:              livecfg.New(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels(hidden)}),
	})
	return h, p
}

// offlineQoderClient 出站一律失败的 Qoder 客户端：模型目录回落内置快照，测试不碰网络。
func offlineQoderClient(t *testing.T) *qoder.Client {
	t.Helper()
	return qoder.NewClient(qoder.Options{
		DataDir: t.TempDir(),
		Logf:    t.Logf,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("offline test transport: " + r.URL.Host)
		}),
	})
}

// qoderCatalogSplit 从内置目录里挑出「两区都有」「只在国内」「只在国际」的模型名各一个。
func qoderCatalogSplit(t *testing.T) (both, cnOnly, intlOnly string) {
	t.Helper()
	c := offlineQoderClient(t)
	ids := func(realm string) map[string]bool {
		ms, err := c.Models(context.Background(), realm, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, m := range ms {
			if m.Enabled {
				out[m.ID] = true
			}
		}
		return out
	}
	cn, intl := ids(qoder.RealmCN), ids(qoder.RealmIntl)
	for id := range cn {
		if len(c.ModelRealms(id)) == 2 && both == "" && intl[id] {
			both = id
		}
		if !intl[id] && len(c.ModelRealms(id)) == 1 && cnOnly == "" {
			cnOnly = id
		}
	}
	for id := range intl {
		if !cn[id] && len(c.ModelRealms(id)) == 1 && intlOnly == "" {
			intlOnly = id
		}
	}
	if both == "" {
		t.Skip("内置目录里没有两区同名的模型")
	}
	return both, cnOnly, intlOnly
}

func postChat(h http.Handler, model string, stream bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
	return rec
}

func TestResolveModelDetailQoderPrefixes(t *testing.T) {
	for _, tc := range []struct {
		in, realm, bare string
		explicit        bool
	}{
		{"qoder:Qwen3.8-Max", qoderAutoRealm, "Qwen3.8-Max", true},
		{"qoder: Qwen3.8-Max ", qoderAutoRealm, "Qwen3.8-Max", true},
		{"qoder-cn:Kimi-K2", auth.RealmQoderCN, "Kimi-K2", true},
		{"qoder-intl:MiniMax-M3", auth.RealmQoderIntl, "MiniMax-M3", true},
		{"qoder:", "cn", "qoder:", false},
		{"Qoder:x", "cn", "Qoder:x", false}, // 前缀大小写敏感
		{"glm-5.2", "cn", "glm-5.2", false},
		{"global:gpt-5.4", "global", "gpt-5.4", true},
	} {
		realm, bare, explicit := resolveModelDetail(tc.in)
		if realm != tc.realm || bare != tc.bare || explicit != tc.explicit {
			t.Errorf("resolveModelDetail(%q) = (%q,%q,%v) want (%q,%q,%v)",
				tc.in, realm, bare, explicit, tc.realm, tc.bare, tc.explicit)
		}
	}
	// 跨平台永不回退：两个平台各自成对。
	if otherRealm(auth.RealmQoderCN) != auth.RealmQoderIntl || otherRealm(auth.RealmQoderIntl) != auth.RealmQoderCN {
		t.Error("qoder realms must pair with each other")
	}
	if otherRealm("cn") != "global" || otherRealm("global") != "cn" {
		t.Error("workbuddy realms must pair with each other")
	}
}

// 显式区前缀只选该区的 Qoder 账号；WorkBuddy 账号与另一区账号都不参与。
func TestQoderExplicitRealmRouting(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, nil,
		&auth.Auth{UID: "wb1", AccessToken: "at-wb", ExpiresAt: 9999999999},
		qoderAcct("qc1", qoder.RealmCN), qoderAcct("qi1", qoder.RealmIntl))

	rec := postChat(h, "qoder-intl:"+both, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 1 || got[0] != "qi1" {
		t.Fatalf("calls=%v want [qi1]", got)
	}
}

// qoder:X 只在一区有 → 直接去那一区（默认区设成另一区也不影响）。
func TestQoderAutoSingleRealm(t *testing.T) {
	_, cnOnly, intlOnly := qoderCatalogSplit(t)
	model, wantUID, otherDefault := cnOnly, "qc1", auth.RealmQoderIntl
	if model == "" {
		model, wantUID, otherDefault = intlOnly, "qi1", auth.RealmQoderCN
	}
	if model == "" {
		t.Skip("内置目录里没有只在单区存在的模型")
	}
	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, nil, qoderAcct("qc1", qoder.RealmCN), qoderAcct("qi1", qoder.RealmIntl))
	h.cfg.QoderDefaultRealm = otherDefault
	rec := postChat(h, "qoder:"+model, true)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 1 || got[0] != wantUID {
		t.Fatalf("model=%s calls=%v want [%s]", model, got, wantUID)
	}
}

// qoder:X 两区都有 → 默认区优先；默认区没有可用号时回退另一区。
func TestQoderAutoBothRealmsDefaultThenFallback(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	fake := &qoderChatFake{}
	h, p := newQoderHandler(t, fake, nil, qoderAcct("qc1", qoder.RealmCN), qoderAcct("qi1", qoder.RealmIntl))

	if rec := postChat(h, "qoder:"+both, true); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 1 || got[0] != "qc1" {
		t.Fatalf("default realm: calls=%v want [qc1]", got)
	}

	p.Cooldown("qc1", pool.CoolSoft, time.Hour, "test")
	if rec := postChat(h, "qoder:"+both, true); rec.Code != 200 {
		t.Fatalf("fallback: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 2 || got[1] != "qi1" {
		t.Fatalf("fallback: calls=%v want [qc1 qi1]", got)
	}
}

// 跨平台回退关闭（Pool 零值）时：没有 Qoder 账号的 Qoder 模型 503，不会落到 WorkBuddy 账号上。
func TestQoderNeverFallsBackToWorkBuddy(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, nil, &auth.Auth{UID: "wb1", AccessToken: "at-wb", ExpiresAt: 9999999999})
	rec := postChat(h, "qoder:"+both, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 0 {
		t.Fatalf("calls=%v want none", got)
	}
}

// 429 → 冷却该号并换号；会话失效 → 停用该号并换号。
func TestQoderErrorPolicyRotates(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	for _, tc := range []struct {
		name  string
		err   *qoder.Error
		check func(t *testing.T, st pool.Status)
	}{
		{"rate_limit", &qoder.Error{Kind: qoder.KindRateLimit, Status: 429, Msg: "too many"}, func(t *testing.T, st pool.Status) {
			if !st.Cooling {
				t.Errorf("rate-limited account not cooled: %+v", st)
			}
		}},
		{"session_dead", &qoder.Error{Kind: qoder.KindSessionDead, Status: 401, Code: "TOKEN_EXPIRE"}, func(t *testing.T, st pool.Status) {
			if !st.Disabled {
				t.Errorf("session-dead account not disabled: %+v", st)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &qoderChatFake{errOf: map[string]*qoder.Error{"qa": tc.err}}
			a, b := qoderAcct("qa", qoder.RealmCN), qoderAcct("qb", qoder.RealmCN)
			h, p := newQoderHandler(t, fake, nil, a, b)
			p.SetCredits("qa", 2000, 0) // 确定性源 → 先选 qa
			rec := postChat(h, "qoder-cn:"+both, true)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := fake.called(); len(got) != 2 || got[0] != "qa" || got[1] != "qb" {
				t.Fatalf("calls=%v want [qa qb]", got)
			}
			st, _ := p.Status("qa")
			tc.check(t, st)
		})
	}
}

// 内容审核 / 参数错误：请求本身的问题，不罚号、不换号，原样回 400。
func TestQoderBadRequestFailsFast(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	fake := &qoderChatFake{errOf: map[string]*qoder.Error{
		"qa": {Kind: qoder.KindBadRequest, Status: 400, Msg: "invalid tool schema"},
	}}
	h, p := newQoderHandler(t, fake, nil, qoderAcct("qa", qoder.RealmCN), qoderAcct("qb", qoder.RealmCN))
	p.SetCredits("qa", 2000, 0)
	rec := postChat(h, "qoder-cn:"+both, true)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid tool schema") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.called(); len(got) != 1 {
		t.Fatalf("calls=%v want exactly one", got)
	}
	if st, _ := p.Status("qa"); st.Disabled || st.Cooling {
		t.Fatalf("bad request must not penalize account: %+v", st)
	}
}

// 非流式：Qoder 输出的 SSE 聚合成 chat.completion。
func TestQoderNonStreamAggregates(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)
	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, nil, qoderAcct("qc1", qoder.RealmCN))
	rec := postChat(h, "qoder-cn:"+both, false)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "你好" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

// models.hidden："qoder:X" 两区都藏（点名调用 400）；"qoder-cn:X" 只藏国内区，qoder:X 改走国际区。
func TestQoderHiddenModels(t *testing.T) {
	both, _, _ := qoderCatalogSplit(t)

	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, []string{"qoder:" + both}, qoderAcct("qc1", qoder.RealmCN), qoderAcct("qi1", qoder.RealmIntl))
	for _, m := range []string{"qoder:" + both, "qoder-cn:" + both, "qoder-intl:" + both, "qoder:" + strings.ToLower(both)} {
		if rec := postChat(h, m, true); rec.Code != 400 {
			t.Errorf("%s: status=%d want 400", m, rec.Code)
		}
	}
	if got := fake.called(); len(got) != 0 {
		t.Fatalf("hidden model reached upstream: %v", got)
	}

	fake2 := &qoderChatFake{}
	h2, _ := newQoderHandler(t, fake2, []string{"qoder-cn:" + both}, qoderAcct("qc1", qoder.RealmCN), qoderAcct("qi1", qoder.RealmIntl))
	if rec := postChat(h2, "qoder:"+both, true); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake2.called(); len(got) != 1 || got[0] != "qi1" {
		t.Fatalf("calls=%v want [qi1]", got)
	}
}

// /v1/models 只列有 Qoder 账号的区域；同名模型只列一次，id 带 qoder: 前缀。
func TestQoderModelsListing(t *testing.T) {
	both, cnOnly, intlOnly := qoderCatalogSplit(t)
	fake := &qoderChatFake{}
	h, _ := newQoderHandler(t, fake, nil, qoderAcct("qi1", qoder.RealmIntl))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, m := range resp.Data {
		if strings.HasPrefix(m.ID, "qoder:") {
			seen[m.ID]++
			if m.OwnedBy != "qoder" {
				t.Errorf("%s owned_by=%q", m.ID, m.OwnedBy)
			}
		}
	}
	if seen["qoder:"+both] != 1 {
		t.Errorf("qoder:%s listed %d times, want 1", both, seen["qoder:"+both])
	}
	if intlOnly != "" && seen["qoder:"+intlOnly] != 1 {
		t.Errorf("intl-only model %s not listed", intlOnly)
	}
	if cnOnly != "" && seen["qoder:"+cnOnly] != 0 {
		t.Errorf("cn-only model listed without cn accounts: %s", cnOnly)
	}
}
