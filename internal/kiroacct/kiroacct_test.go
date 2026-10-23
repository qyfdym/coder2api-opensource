package kiroacct

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

// fakeRT 按「主机 + 路径」路由的假上游（经 kiro.Options.Transport 注入，不开端口）。
type fakeRT struct {
	mu       sync.Mutex
	handlers map[string]func(r *http.Request, body []byte) (int, string)
	hits     map[string]int
}

func newFakeRT() *fakeRT {
	return &fakeRT{handlers: map[string]func(*http.Request, []byte) (int, string){}, hits: map[string]int{}}
}

func (f *fakeRT) on(route string, h func(r *http.Request, body []byte) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

func (f *fakeRT) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[route]
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	route := r.URL.Host + r.URL.Path
	f.mu.Lock()
	f.hits[route]++
	h := f.handlers[route]
	f.mu.Unlock()
	status, out := 404, `{"message":"no route"}`
	if h != nil {
		status, out = h(r, body)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(out)), Header: http.Header{}, Request: r}, nil
}

const (
	routeUsage  = "codewhisperer.us-east-1.amazonaws.com/getUsageLimits"
	routeSocial = "prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	routeOIDC   = "oidc.us-east-1.amazonaws.com/token"
	testArn     = "arn:aws:codewhisperer:us-east-1:123456789012:profile/TEST"
)

func usageJSON(userID, email string, current, limit float64) string {
	b, _ := json.Marshal(map[string]any{
		"nextDateReset":    1893456000,
		"subscriptionInfo": map[string]any{"subscriptionTitle": "KIRO PRO", "subscriptionType": "Q_DEVELOPER_STANDALONE_PRO"},
		"userInfo":         map[string]any{"email": email, "userId": userID},
		"usageBreakdownList": []any{map[string]any{
			"resourceType": "AGENTIC_REQUEST", "currentUsageWithPrecision": current, "usageLimitWithPrecision": limit,
			"currentUsage": current, "usageLimit": limit,
		}},
	})
	return string(b)
}

func newService(t *testing.T, f *fakeRT) *Service {
	t.Helper()
	return &Service{
		Client:  kiro.NewClient(kiro.Options{Transport: f, Logf: func(string, ...any) {}}),
		AuthDir: t.TempDir(),
	}
}

func loadOne(t *testing.T, dir string) *auth.Auth {
	t.Helper()
	list, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("auth dir has %d accounts, want 1", len(list))
	}
	return list[0]
}

// API Key：不刷新、查一次额度确认有效；uid 由 userId 决定，重复导入覆盖同一个文件。
func TestImportAPIKey(t *testing.T) {
	f := newFakeRT()
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer ksk_test123" {
			return 403, `{"message":"bad key"}`
		}
		return 200, usageJSON("user-api", "api@example.com", 10, 1000)
	})
	s := newService(t, f)
	items, skipped := kiro.ParseImport("ksk_test123")
	if len(items) != 1 || len(skipped) != 0 {
		t.Fatalf("parse: %+v %v", items, skipped)
	}
	res := s.Import(context.Background(), items)
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("import: %+v", res)
	}
	a := res[0].Account
	if !a.IsKiro() || !IsAPIKey(a) || a.UID != kiro.StableUID("user-api") || a.Nickname != "api@example.com" {
		t.Fatalf("account: kiro=%v apikey=%v uid=%s nick=%s", a.IsKiro(), IsAPIKey(a), a.UID, a.Nickname)
	}
	if f.count(routeSocial)+f.count(routeOIDC) != 0 {
		t.Error("API key import must not refresh")
	}
	m := a.KiroMeta()
	if m.Plan != "PRO" || m.Source != "api_key" || m.MachineID == "" || m.APIKey != "ksk_test123" {
		t.Errorf("meta: %+v", m)
	}
	// 重导入：同一个 uid、同一个文件。
	if res2 := s.Import(context.Background(), items); len(res2) != 1 || !res2[0].OK || res2[0].UID != a.UID {
		t.Fatalf("re-import: %+v", res2)
	}
	back := loadOne(t, s.AuthDir)
	if back.UID != a.UID || !back.IsKiro() || back.AccessTokenValue() != "ksk_test123" {
		t.Fatalf("reload: uid=%s kiro=%v", back.UID, back.IsKiro())
	}
	// API Key 永不刷新。
	if NeedsRefresh(back, time.Hour) {
		t.Error("API key account must never need refresh")
	}
	if kerr := s.EnsureFresh(context.Background(), back, time.Hour); kerr != nil {
		t.Fatalf("EnsureFresh(api key): %v", kerr)
	}
}

// 行格式（邮箱----密码----refreshToken----clientId----clientSecret）：走 OIDC 刷新，
// refresh token 轮换后落盘；密码列不出现在账号文件里。
func TestImportLineFormatIdC(t *testing.T) {
	f := newFakeRT()
	f.on(routeOIDC, func(_ *http.Request, body []byte) (int, string) {
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		if m["refreshToken"] != "rt-line-1234567890" || m["clientId"] != "cid-1" || m["clientSecret"] != "cs-1" {
			return 400, `{"error":"invalid_grant"}`
		}
		return 200, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":3600}`
	})
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer at-new" {
			return 403, `{"message":"bad token"}`
		}
		return 200, usageJSON("user-idc", "line@example.com", 50, 50)
	})
	s := newService(t, f)
	const secretPW = "Sup3r-Secret-PW"
	items, _ := kiro.ParseImport("line@example.com----" + secretPW + "----rt-line-1234567890----cid-1----cs-1")
	res := s.Import(context.Background(), items)
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("import: %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(s.AuthDir, auth.KiroFileName(res[0].UID)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secretPW)) {
		t.Fatal("password column leaked into the account file")
	}
	back := loadOne(t, s.AuthDir)
	if back.RefreshTokenValue() != "rt-rotated" || back.AccessTokenValue() != "at-new" {
		t.Errorf("rotated tokens not saved")
	}
	if m := back.KiroMeta(); m.AuthMethod != "idc" || m.ClientID != "cid-1" || m.Email != "line@example.com" {
		t.Errorf("meta: %+v", m)
	}
}

// 凭证被拒（刷新 invalid_grant / 额度接口 403 封禁）：不建档、给出原因。
func TestImportRejectsDeadCredentials(t *testing.T) {
	f := newFakeRT()
	f.on(routeSocial, func(*http.Request, []byte) (int, string) { return 400, `{"error":"invalid_grant"}` })
	f.on(routeUsage, func(*http.Request, []byte) (int, string) {
		return 403, `{"reason":"TEMPORARILY_SUSPENDED","message":"suspended"}`
	})
	s := newService(t, f)
	items, _ := kiro.ParseImport(strings.Repeat("r", 40) + "\nksk_suspended")
	res := s.Import(context.Background(), items)
	if len(res) != 2 || res[0].OK || res[1].OK || res[0].Error == "" || res[1].Error == "" {
		t.Fatalf("results: %+v", res)
	}
	if list, _ := auth.LoadDir(s.AuthDir); len(list) != 0 {
		t.Fatalf("dead credentials must not be saved, got %d files", len(list))
	}
}

// 额度：基础额度换算成池的整数口径，并补全套餐 / 邮箱 / 昵称 / 下次重置时刻；token 将过期时先刷新。
func TestBalanceRefreshesAndFillsProfile(t *testing.T) {
	f := newFakeRT()
	f.on(routeSocial, func(*http.Request, []byte) (int, string) {
		return 200, `{"accessToken":"at-fresh","refreshToken":"rt-2","expiresIn":3600,"profileArn":"` + testArn + `"}`
	})
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer at-fresh" || r.URL.Query().Get("profileArn") != testArn {
			return 403, `{"message":"stale token"}`
		}
		return 200, usageJSON("user-b", "bal@example.com", 12.5, 1000)
	})
	s := newService(t, f)
	a := auth.NewKiro("kbal", "", "at-old", "rt-1", time.Now().Add(time.Minute).Unix(), auth.KiroMeta{
		AuthMethod: "social", Provider: "Github", Region: "us-east-1",
	})
	a.FilePath = filepath.Join(s.AuthDir, auth.KiroFileName(a.UID))
	remain, total, u, kerr := s.Balance(context.Background(), a)
	if kerr != nil {
		t.Fatal(kerr)
	}
	if remain != 987 || total != 1000 || u.Plan != "PRO" {
		t.Fatalf("balance: remain=%d total=%d plan=%s", remain, total, u.Plan)
	}
	if f.count(routeSocial) != 1 {
		t.Errorf("refresh calls=%d want 1", f.count(routeSocial))
	}
	m := a.KiroMeta()
	if m.Plan != "PRO" || m.Email != "bal@example.com" || m.ProfileArn != testArn || m.NextResetAt != 1893456000 || a.Nickname != "bal@example.com" {
		t.Errorf("profile not filled: nick=%q meta=%+v", a.Nickname, m)
	}
	if got := NextReset(a, time.Unix(1800000000, 0)); !got.Equal(time.Unix(1893456000, 0)) {
		t.Errorf("NextReset=%v", got)
	}
	if got := NextReset(a, time.Unix(1900000000, 0)); !got.Equal(time.Unix(1900000000, 0).Add(6 * time.Hour)) {
		t.Errorf("NextReset after reset time=%v", got)
	}
	// 第二次：token 新鲜，不再刷新。
	if _, _, _, kerr := s.Balance(context.Background(), a); kerr != nil || f.count(routeSocial) != 1 {
		t.Fatalf("second balance: err=%v refresh calls=%d", kerr, f.count(routeSocial))
	}
}

// 并发 EnsureFresh 只刷新一次（账号级串行 + 拿锁后复查）。
func TestEnsureFreshSingleFlight(t *testing.T) {
	f := newFakeRT()
	f.on(routeSocial, func(*http.Request, []byte) (int, string) {
		time.Sleep(20 * time.Millisecond)
		return 200, `{"accessToken":"at-x","refreshToken":"rt-x","expiresIn":3600}`
	})
	s := newService(t, f)
	a := auth.NewKiro("kconc", "n", "at-old", "rt-1", time.Now().Unix()-10, auth.KiroMeta{AuthMethod: "social"})
	a.FilePath = filepath.Join(s.AuthDir, auth.KiroFileName(a.UID))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if kerr := s.EnsureFresh(context.Background(), a, time.Minute); kerr != nil {
				t.Error(kerr)
			}
		}()
	}
	wg.Wait()
	if n := f.count(routeSocial); n != 1 {
		t.Fatalf("refresh calls=%d want 1", n)
	}
	if a.AccessTokenValue() != "at-x" {
		t.Fatal("token not updated")
	}
}

func TestModelRateNilSafe(t *testing.T) {
	var s *Service
	if s.ModelRate("claude-sonnet-4.5") != "" {
		t.Fatal("nil service must return empty rate")
	}
}
