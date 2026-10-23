package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// kiroFakeRT 按「主机 + 路径」路由的假 Kiro 上游（不开端口）。
type kiroFakeRT struct {
	mu       sync.Mutex
	handlers map[string]func(r *http.Request, body []byte) (int, string)
}

func (f *kiroFakeRT) on(route string, h func(*http.Request, []byte) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers == nil {
		f.handlers = map[string]func(*http.Request, []byte) (int, string){}
	}
	f.handlers[route] = h
}

func (f *kiroFakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	h := f.handlers[r.URL.Host+r.URL.Path]
	f.mu.Unlock()
	status, out := 404, `{"message":"no route"}`
	if h != nil {
		status, out = h(r, body)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(out)), Header: http.Header{}, Request: r}, nil
}

const kiroUsageRoute = "codewhisperer.us-east-1.amazonaws.com/getUsageLimits"

func kiroUsageBody(userID, email string, current, limit float64) string {
	b, _ := json.Marshal(map[string]any{
		"nextDateReset":    1893456000,
		"subscriptionInfo": map[string]any{"subscriptionTitle": "KIRO FREE", "subscriptionType": "Q_DEVELOPER_STANDALONE_FREE"},
		"userInfo":         map[string]any{"email": email, "userId": userID},
		"usageBreakdownList": []any{map[string]any{
			"resourceType": "AGENTIC_REQUEST", "currentUsage": current, "usageLimit": limit,
			"currentUsageWithPrecision": current, "usageLimitWithPrecision": limit,
		}},
	})
	return string(b)
}

func newKiroPanel(t *testing.T, rt *kiroFakeRT) (*Panel, *pool.Pool) {
	t.Helper()
	pl := pool.New("")
	svc := &kiroacct.Service{
		Client:  kiro.NewClient(kiro.Options{Transport: rt, Logf: t.Logf}),
		AuthDir: t.TempDir(),
	}
	return New(Config{Version: "test", APIKey: "test-key", Pool: pl, Kiro: svc}), pl
}

func kiroDo(t *testing.T, p *Panel, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer test-key")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// 凭证导入：成功的入池并带上额度；格式不全的进 skipped；重复导入不重复建号。
func TestKiroImportEndpoint(t *testing.T) {
	rt := &kiroFakeRT{}
	rt.on(kiroUsageRoute, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer ksk_good" {
			return 403, `{"message":"The bearer token included in the request is invalid."}`
		}
		return 200, kiroUsageBody("user-1", "k@example.com", 20, 50)
	})
	p, pl := newKiroPanel(t, rt)

	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"ksk_good\nksk_bad\nnot a credential"}`)
	if code != 200 || out["imported"] != float64(1) {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if failed, _ := out["failed"].([]any); len(failed) != 1 {
		t.Errorf("failed=%v", out["failed"])
	}
	if skipped, _ := out["skipped"].([]any); len(skipped) != 1 {
		t.Errorf("skipped=%v", out["skipped"])
	}
	accs, _ := out["accounts"].([]any)
	acc, _ := accs[0].(map[string]any)
	uid, _ := acc["uid"].(string)
	if acc["platform"] != auth.PlatformKiro || acc["provider"] != "APIKey" || acc["nickname"] != "k@example.com" {
		t.Errorf("account brief: %v", acc)
	}
	st, ok := pl.Status(uid)
	if !ok || st.Platform != auth.PlatformKiro || st.Realm != auth.RealmKiro || st.Credits != 30 || st.CreditsTotal != 50 {
		t.Fatalf("pool status: ok=%v %+v", ok, st)
	}
	if st.Plan != "FREE" || st.AuthKind != "APIKey" {
		t.Errorf("plan=%q auth_kind=%q", st.Plan, st.AuthKind)
	}
	// 重复导入：同一个 uid，池里仍只有一个号。
	if code, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"ksk_good"}`); code != 200 || out["imported"] != float64(1) {
		t.Fatalf("re-import: code=%d out=%v", code, out)
	}
	if n := len(pl.List()); n != 1 {
		t.Fatalf("pool size=%d want 1", n)
	}
	// 「额度」按钮：Kiro 账号走 Kiro 额度接口。
	if code, out := kiroDo(t, p, "POST", "/panel/api/accounts/"+uid+"/balance", ""); code != 200 || out["credits"] != float64(30) {
		t.Fatalf("balance: code=%d out=%v", code, out)
	}
	// 什么都解析不出来：400 + 原因。
	code, out = kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"short"}`)
	if code != 400 || !strings.Contains(out["error"].(string), "没有可导入的账号") {
		t.Fatalf("empty import: code=%d out=%v", code, out)
	}
}

// 额度耗尽的号导入后立即硬冷却到下次重置，不参与选号。
func TestKiroImportExhaustedCools(t *testing.T) {
	rt := &kiroFakeRT{}
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		return 200, kiroUsageBody("user-x", "x@example.com", 50, 50)
	})
	p, pl := newKiroPanel(t, rt)
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"ksk_empty"}`)
	if code != 200 || out["imported"] != float64(1) {
		t.Fatalf("code=%d out=%v", code, out)
	}
	uid := out["accounts"].([]any)[0].(map[string]any)["uid"].(string)
	if st, _ := pl.Status(uid); !st.Cooling || st.Credits != 0 {
		t.Fatalf("exhausted account must cool: %+v", st)
	}
}

// 设备授权：start 只给浏览器 state / 验证码 / 链接（不含 deviceCode 与客户端密钥）；
// poll 先 pending、再 ok 建档入池；会话用完即删；cancel 幂等。
func TestKiroDeviceLoginEndpoints(t *testing.T) {
	rt := &kiroFakeRT{}
	rt.on("oidc.us-east-1.amazonaws.com/client/register", func(*http.Request, []byte) (int, string) {
		return 200, `{"clientId":"cid-secret-id","clientSecret":"cs-very-secret"}`
	})
	rt.on("oidc.us-east-1.amazonaws.com/device_authorization", func(*http.Request, []byte) (int, string) {
		return 200, `{"deviceCode":"dc-hidden","userCode":"WXYZ-1234","verificationUri":"https://device.sso.us-east-1.amazonaws.com/",` +
			`"verificationUriComplete":"https://device.sso.us-east-1.amazonaws.com/?user_code=WXYZ-1234","interval":1,"expiresIn":600}`
	})
	var polls int
	rt.on("oidc.us-east-1.amazonaws.com/token", func(_ *http.Request, body []byte) (int, string) {
		if !strings.Contains(string(body), "dc-hidden") {
			return 400, `{"error":"invalid_request"}`
		}
		polls++
		if polls == 1 {
			return 400, `{"error":"authorization_pending"}`
		}
		return 200, `{"accessToken":"at-login","refreshToken":"rt-login","expiresIn":3600}`
	})
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		return 200, kiroUsageBody("user-login", "login@example.com", 1, 50)
	})
	p, pl := newKiroPanel(t, rt)

	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/login/start", `{}`)
	if code != 200 || out["user_code"] != "WXYZ-1234" || out["provider"] != "BuilderId" {
		t.Fatalf("start: code=%d out=%v", code, out)
	}
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"dc-hidden", "cs-very-secret", "cid-secret-id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("start response leaks %q: %s", secret, raw)
		}
	}
	state, _ := out["state"].(string)
	if code, out := kiroDo(t, p, "GET", "/panel/api/kiro/login/poll?state="+state, ""); code != 200 || out["status"] != "pending" {
		t.Fatalf("poll 1: code=%d out=%v", code, out)
	}
	code, out = kiroDo(t, p, "GET", "/panel/api/kiro/login/poll?state="+state, "")
	if code != 200 || out["status"] != "ok" {
		t.Fatalf("poll 2: code=%d out=%v", code, out)
	}
	acc, _ := out["account"].(map[string]any)
	uid, _ := acc["uid"].(string)
	if acc["nickname"] != "login@example.com" || acc["auth_method"] != "idc" {
		t.Errorf("account: %v", acc)
	}
	if a := pl.AuthByUID(uid); a == nil || !a.IsKiro() || a.KiroMeta().ClientSecret != "cs-very-secret" || a.RefreshTokenValue() != "rt-login" {
		t.Fatalf("login account not in pool with its OIDC client: %v", a)
	}
	// 会话已消费：再 poll 是 expired。
	if _, out := kiroDo(t, p, "GET", "/panel/api/kiro/login/poll?state="+state, ""); out["status"] != "expired" {
		t.Errorf("consumed session poll: %v", out)
	}
	// IdC：start_url 必须是 https。
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/login/start", `{"start_url":"http://insecure.example/start"}`); code != 400 {
		t.Errorf("insecure start_url: code=%d want 400", code)
	}
	if code, out := kiroDo(t, p, "POST", "/panel/api/kiro/login/cancel", `{"state":"nope"}`); code != 200 || out["cancelled"] != false {
		t.Errorf("cancel unknown: code=%d out=%v", code, out)
	}
}

// 未装配 Kiro：入口一律 501；overview 告诉前端隐藏 Kiro tab。
func TestKiroDisabled(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/panel/api/kiro/import", `{"text":"ksk_x"}`},
		{"POST", "/panel/api/kiro/login/start", `{}`},
		{"GET", "/panel/api/kiro/login/poll?state=x", ""},
	} {
		if code, _ := kiroDo(t, p, c.method, c.path, c.body); code != http.StatusNotImplemented {
			t.Errorf("%s %s: code=%d want 501", c.method, c.path, code)
		}
	}
	if _, out := kiroDo(t, p, "GET", "/panel/api/overview", ""); out["kiro_enabled"] != false {
		t.Errorf("overview kiro_enabled=%v", out["kiro_enabled"])
	}
}
