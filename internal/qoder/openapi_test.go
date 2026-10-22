package qoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 账号类测试的公共夹具（前缀 acct，避免与同包其它测试的辅助函数重名）
// ---------------------------------------------------------------------------

// acctRewrite 把所有出站请求改写到 httptest 服务器，原 host 放进 X-Orig-Host 供处理器区分区域。
type acctRewrite struct {
	target *url.URL
	base   http.RoundTripper
}

func (t acctRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("X-Orig-Host", r.URL.Host)
	r2.URL.Scheme, r2.URL.Host, r2.Host = t.target.Scheme, t.target.Host, t.target.Host
	return t.base.RoundTrip(r2)
}

// acctNewClient 构造指向 h 的客户端。机器身份缺省「找不到组件」（不会碰到本机真实的
// runtime-info.exe，也不会删本机种子）；需要身份的测试用 acctWithIdentity 注入。
func acctNewClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c := NewClient(Options{Logf: func(f string, a ...any) { t.Logf(f, a...) }})
	c.httpc = &http.Client{Transport: acctRewrite{target: u, base: http.DefaultTransport}, Timeout: 10 * time.Second}
	c.opts.RuntimeInfoPath = filepath.Join(t.TempDir(), "absent-runtime-info")
	c.identity.installDirs = func() []string { return nil }
	seed := filepath.Join(t.TempDir(), ".locale_cfg")
	c.identity.seedPath = func() string { return seed }
	return c
}

// acctWithIdentity 注入一个「可用」的假组件：返回固定身份，统计调用次数。
func acctWithIdentity(t *testing.T, c *Client, token string) *int {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "runtime-info.exe")
	if err := os.WriteFile(exe, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.opts.RuntimeInfoPath = exe
	calls := new(int)
	var mu sync.Mutex
	c.identity.run = func(ctx context.Context, exe string, stdin []byte) ([]byte, error) {
		mu.Lock()
		*calls++
		mu.Unlock()
		return []byte(`{"machineToken":"` + token + `","machineType":"T-` + token + `","machineCode":"C-` + token +
			`","vmInfo":{"isVm":false,"brand":"","vmTypeCode":0,"percentage":0}}`), nil
	}
	return calls
}

// acctFastRetry 把重试退避缩到 1ms（测试结束恢复）。
func acctFastRetry(t *testing.T) {
	old := oaRetryBackoff
	oaRetryBackoff = time.Millisecond
	t.Cleanup(func() { oaRetryBackoff = old })
}

// acctVectors 原实现生成的向量（.wb-test/qd_account_vectors.py）。
type acctVectors struct {
	PKCE    [][3]string `json:"pkce"`
	AuthURL []struct {
		Realm     string `json:"realm"`
		UUID      string `json:"uuid"`
		MachineID string `json:"machine_id"`
		RawHex    string `json:"raw_hex"`
		AuthURL   string `json:"auth_url"`
		Verifier  string `json:"verifier"`
		Nonce     string `json:"nonce"`
	} `json:"auth_url"`
	Credits []struct {
		Name    string          `json:"name"`
		Body    json.RawMessage `json:"body"`
		OK      bool            `json:"ok"`
		Credits map[string]any  `json:"credits"`
		Reason  string          `json:"reason"`
	} `json:"credits"`
	Headers struct {
		UID            string            `json:"uid"`
		Plain          map[string]string `json:"plain"`
		DesktopOmitted map[string]string `json:"desktop_omitted"`
		DesktopNative  map[string]string `json:"desktop_native"`
		DesktopVersion string            `json:"desktop_version"`
	} `json:"headers"`
	Checkin []struct {
		Name      string                       `json:"name"`
		OnlyDaily bool                         `json:"only_daily"`
		Account   map[string]map[string]string `json:"account"`
		Routes    map[string][2]any            `json:"routes"`
		OK        bool                         `json:"ok"`
		Earned    float64                      `json:"earned"`
		Message   *string                      `json:"message"`
		Claimed   []string                     `json:"claimed"`
		Logs      []string                     `json:"logs"`
		Stamped   bool                         `json:"stamped"`
		Codes     map[string]string            `json:"codes"`
		Blocked   []string                     `json:"blocked"`
	} `json:"checkin"`
	Pro []struct {
		Name    string            `json:"name"`
		Routes  map[string][2]any `json:"routes"`
		OK      bool              `json:"ok"`
		Earned  float64           `json:"earned"`
		Already bool              `json:"already"`
		State   string            `json:"state"`
		Msg     string            `json:"msg"`
		Logs    []string          `json:"logs"`
	} `json:"pro"`
	NextWindow [][3]any `json:"next_window"`
	Vote       []struct {
		Name        string           `json:"name"`
		Samples     []map[string]any `json:"samples"`
		Calls       int              `json:"calls"`
		PickedToken string           `json:"picked_token"`
		PickedPct   *float64         `json:"picked_pct"`
	} `json:"vote"`
}

func acctLoadVectors(t *testing.T) acctVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/account_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v acctVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.PKCE) == 0 || len(v.Checkin) == 0 {
		t.Fatal("向量为空：用 .wb-test/qd_account_vectors.py 重新生成")
	}
	return v
}

// acctReplay 按「METHOD path」→ [status, 原文] 回放路由；未知路由回 404（与原实现打桩一致）。
type acctReplay struct {
	mu     sync.Mutex
	routes map[string][2]any
	subst  func(string) string
	calls  []string
	hdrs   map[string]http.Header
}

func (rp *acctReplay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	_, _ = io.Copy(io.Discard, r.Body)
	rp.mu.Lock()
	rp.calls = append(rp.calls, key)
	if rp.hdrs == nil {
		rp.hdrs = map[string]http.Header{}
	}
	rp.hdrs[key] = r.Header.Clone()
	route, ok := rp.routes[key]
	rp.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
		return
	}
	status := int(route[0].(float64))
	body := route[1].(string)
	if rp.subst != nil {
		body = rp.subst(body)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestOAClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		hdr    http.Header
		want   ErrKind
	}{
		{401, `{"code":"UNAUTHORIZED"}`, nil, KindAuth},
		{403, `forbidden`, nil, KindAuth},
		{401, `{"code":"TOKEN_EXPIRE"}`, nil, KindSessionDead},
		{403, `{"code":12153,"message":"x"}`, nil, KindSessionDead},
		{400, `Offline user session not found`, nil, KindSessionDead},
		{403, `{"traceId":"9912153001"}`, nil, KindAuth}, // 12153 必须是独立数字
		{429, `slow down`, http.Header{"Retry-After": []string{"5"}}, KindRateLimit},
		{500, `boom`, nil, KindServer},
		{502, ``, nil, KindServer},
		{404, `nope`, nil, KindBadRequest},
		{409, `{"errorCode":"ALREADY_CLAIMED"}`, nil, KindBadRequest},
	}
	for _, c := range cases {
		e := oaClassify(c.status, []byte(c.body), c.hdr)
		if e.Kind != c.want {
			t.Errorf("status=%d body=%q kind=%v want %v", c.status, c.body, e.Kind, c.want)
		}
		if e.Status != c.status || !strings.HasPrefix(e.Msg, "HTTP ") {
			t.Errorf("status=%d msg=%q", c.status, e.Msg)
		}
	}
	if e := oaClassify(429, nil, http.Header{"Retry-After": []string{"5"}}); e.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter=%v", e.RetryAfter)
	}
	if e := oaClassify(409, []byte(`{"errorCode":"ALREADY_CLAIMED"}`), nil); e.Code != "ALREADY_CLAIMED" {
		t.Errorf("Code=%q", e.Code)
	}
	// 原实现文案口径：body 按字符截 160。
	long := strings.Repeat("中", 300)
	if e := oaClassify(500, []byte(long), nil); e.Msg != "HTTP 500 "+strings.Repeat("中", 160) {
		t.Errorf("截断口径不对：%d 字符", len([]rune(e.Msg)))
	}
}

func TestOARetriesTransientOnly(t *testing.T) {
	acctFastRetry(t)
	var mu sync.Mutex
	hits := map[string]int{}
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		n := hits[r.URL.Path]
		mu.Unlock()
		switch r.URL.Path {
		case "/flaky":
			if n < 3 {
				w.WriteHeader(503)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/bad":
			w.WriteHeader(400)
		}
	}))
	resp, err := c.oaDo(context.Background(), oaRequest{url: "https://openapi.qoder.com.cn/flaky", attempts: 3})
	if err != nil || resp.status != 200 || hits["/flaky"] != 3 {
		t.Fatalf("5xx 应重试到成功：err=%v hits=%d", err, hits["/flaky"])
	}
	_, err = c.oaDo(context.Background(), oaRequest{url: "https://openapi.qoder.com.cn/bad", attempts: 3})
	if err == nil || err.Kind != KindBadRequest || hits["/bad"] != 1 {
		t.Fatalf("4xx 不应重试：err=%v hits=%d", err, hits["/bad"])
	}
}

func TestOATransportError(t *testing.T) {
	c := NewClient(Options{Logf: func(string, ...any) {}})
	c.httpc = &http.Client{Timeout: time.Second}
	_, err := c.oaDo(context.Background(), oaRequest{url: "http://127.0.0.1:1/x", attempts: 1})
	if err == nil || err.Kind != KindTransport {
		t.Fatalf("连接失败应为 KindTransport：%v", err)
	}
}

func TestOALooseJSON(t *testing.T) {
	m, ok := oaObject([]byte(`{"a":"12.5","b":true,"c":null,"d":"false","e":0,"f":"x","g":1795000000000,"h":[1]}`))
	if !ok {
		t.Fatal("parse")
	}
	if oaNum(m["a"]) != 12.5 || oaNum(m["b"]) != 1 || oaNum(m["c"]) != 0 || oaNum(m["f"]) != 0 || oaNum(m["h"]) != 0 {
		t.Error("oaNum 口径不对")
	}
	if !oaTruthy(m["d"]) || oaTruthyField(m["d"], true) || !oaTruthyField(m["c"], true) || oaTruthy(m["e"]) {
		t.Error("布尔口径不对（Python bool 与 _truthy 两种）")
	}
	if normalizeEpoch(m["g"]) != 1795000000 || normalizeEpoch("1795000000") != 1795000000 || normalizeEpoch(nil) != 0 {
		t.Error("normalizeEpoch")
	}
	if oaStr(m["g"]) != "1795000000000" || oaStr(m["c"]) != "" || oaStr(m["e"]) != "" {
		t.Error("oaStr 口径不对")
	}
}

// TestHeadersMatchPython 普通头 / 桌面端头（无身份、有身份）的键集合与取值与原实现一致。
func TestHeadersMatchPython(t *testing.T) {
	v := acctLoadVectors(t)
	cred := Cred{UID: v.Headers.UID, Realm: RealmCN, AccessToken: "dt-fake"}
	cmp := func(name string, want map[string]string, got http.Header) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s：头数量 %d，原实现 %d（got=%v）", name, len(got), len(want), got)
		}
		for k, wv := range want {
			if gv := got.Get(k); gv != wv {
				t.Errorf("%s：%s=%q want %q", name, k, gv, wv)
			}
		}
	}
	strip := func(h http.Header) http.Header { h.Del("X-Request-ID"); return h }
	cmp("plain", v.Headers.Plain, strip(oaPlainHeaders(cred)))

	c := acctNewClient(t, http.NotFoundHandler())
	if c.opts.DesktopVersion != v.Headers.DesktopVersion {
		t.Errorf("DesktopVersion=%q want %q", c.opts.DesktopVersion, v.Headers.DesktopVersion)
	}
	h, use := c.oaDesktopHeaders(context.Background(), cred)
	if use.headers != MachineHeadersOmitted || use.source != IdentitySourceDerived || use.err == nil {
		t.Errorf("无组件时应 omitted：%+v", use)
	}
	cmp("desktop_omitted", v.Headers.DesktopOmitted, strip(h))

	c2 := acctNewClient(t, http.NotFoundHandler())
	c2.identity.run = func(ctx context.Context, exe string, stdin []byte) ([]byte, error) {
		return []byte(`{"machineToken":"tok-native","machineType":"type-native","machineCode":"code-native"}`), nil
	}
	exe := filepath.Join(t.TempDir(), "runtime-info.exe")
	_ = os.WriteFile(exe, []byte("x"), 0o755)
	c2.opts.RuntimeInfoPath = exe
	h2, use2 := c2.oaDesktopHeaders(context.Background(), cred)
	if use2.headers != MachineHeadersNative || use2.source != IdentitySourceNative {
		t.Errorf("有组件时应 native：%+v", use2)
	}
	cmp("desktop_native", v.Headers.DesktopNative, strip(h2))
}
