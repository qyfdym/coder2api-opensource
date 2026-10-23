package kiroacct

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

const (
	tTenant = "11111111-2222-3333-4444-555555555555"
	tAppID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tIssuer = "https://login.microsoftonline.com/" + tTenant + "/v2.0"

	routeMSToken  = "login.microsoftonline.com/" + tTenant + "/oauth2/v2.0/token"
	routeProfiles = "codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles"
	routeSetPref  = "q.us-east-1.amazonaws.com/setUserPreference"
	otherArn      = "arn:aws:codewhisperer:us-east-1:123456789012:profile/OTHER"
)

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func newSvc(t *testing.T, f *fakeRT) *Service {
	t.Helper()
	return &Service{
		Client: kiro.NewClient(kiro.Options{Transport: f, Logf: func(string, ...any) {},
			Sleep: func(context.Context, time.Duration) error { return nil }}),
		AuthDir: t.TempDir(),
	}
}

func profilesBody(arns ...string) string {
	type pr struct {
		ARN  string `json:"arn"`
		Name string `json:"profileName"`
	}
	var ps []pr
	for _, a := range arns {
		ps = append(ps, pr{a, "p"})
	}
	b, _ := json.Marshal(map[string]any{"profiles": ps})
	return string(b)
}

// 企业 SSO 记录（Kiro Account Manager 形态，缺端点）→ 补全 → 刷新 → 补 profile → 落盘；
// 推理 / 额度请求带 TokenType: EXTERNAL_IDP；账号文件里没有 clientSecret。
func TestImportExternalIdpJSON(t *testing.T) {
	f := newFakeRT()
	access := jwt(map[string]any{"iss": tIssuer, "oid": "oid-7", "preferred_username": "bob@corp.example"})
	// 第一次刷新（导入）用导出文件里的 token；第二次（重载后）必须用上一次轮换并落盘的新 token。
	var refreshTokensSeen []string
	f.on(routeMSToken, func(_ *http.Request, body []byte) (int, string) {
		form, _ := url.ParseQuery(string(body))
		refreshTokensSeen = append(refreshTokensSeen, form.Get("refresh_token"))
		want := []string{"rt-kam", "rt-rotated"}
		if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != tAppID ||
			len(refreshTokensSeen) > len(want) || form.Get("refresh_token") != want[len(refreshTokensSeen)-1] {
			return 400, `{"error":"invalid_grant"}`
		}
		b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "rt-rotated", "expires_in": 3600})
		return 200, string(b)
	})
	var tokenTypes []string
	f.on(routeProfiles, func(r *http.Request, _ []byte) (int, string) {
		tokenTypes = append(tokenTypes, r.Header.Get("TokenType"))
		return 200, profilesBody(testArn)
	})
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		tokenTypes = append(tokenTypes, r.Header.Get("TokenType"))
		if r.Header.Get("Authorization") != "Bearer "+access {
			return 403, `{"message":"bad token"}`
		}
		return 200, usageJSON("", "", 10, 100) // 企业号没有 userInfo：身份来自 JWT
	})
	s := newSvc(t, f)

	items, skipped := kiro.ParseImport(`{"credentials":{"refreshToken":"rt-kam","clientId":"` + tAppID + `"},
		"userId":"` + tIssuer + `.oid-7","authMethod":"external_idp","clientSecret":"must-be-dropped"}`)
	if len(items) != 1 || len(skipped) != 0 {
		t.Fatalf("parse: %+v %v", items, skipped)
	}
	res := s.Import(context.Background(), items)
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("import: %+v", res)
	}
	a := res[0].Account
	m := a.KiroMeta()
	if m.AuthMethod != "external_idp" || m.Provider != "AzureAD" || m.ClientID != tAppID || m.ClientSecret != "" ||
		m.IssuerURL != tIssuer || !strings.HasSuffix(m.TokenEndpoint, "/oauth2/v2.0/token") || m.ProfileArn != testArn {
		t.Fatalf("meta: %+v", m)
	}
	if a.Nickname != "bob@corp.example" || a.RefreshTokenValue() != "rt-rotated" {
		t.Fatalf("identity/tokens: nick=%q rt=%q", a.Nickname, a.RefreshTokenValue())
	}
	if a.UID != kiro.StableUID(tIssuer+".oid-7") {
		t.Errorf("uid should derive from the stable JWT identity, got %s", a.UID)
	}
	for _, tt := range tokenTypes {
		if tt != "EXTERNAL_IDP" {
			t.Errorf("TokenType header = %q, want EXTERNAL_IDP", tt)
		}
	}
	if len(tokenTypes) == 0 {
		t.Fatal("no REST call observed")
	}
	raw, _ := os.ReadFile(filepath.Join(s.AuthDir, auth.KiroFileName(a.UID)))
	if strings.Contains(string(raw), "must-be-dropped") {
		t.Fatal("client secret from the export leaked into the account file")
	}
	back := loadOne(t, s.AuthDir)
	if c := Cred(back); c.TokenEndpoint == "" || c.IssuerURL != tIssuer || c.Scopes == "" || !c.IsExternalIDP() {
		t.Fatalf("reloaded cred lost the external idp fields: %+v", c)
	}
	// 重新载入的账号还能刷新（刷新用的就是落盘的端点 / scope）。
	if kerr := s.Refresh(context.Background(), back); kerr != nil {
		t.Fatalf("refresh reloaded account: %v", kerr)
	}
	if len(refreshTokensSeen) != 2 || refreshTokensSeen[1] != "rt-rotated" {
		t.Fatalf("second refresh must use the persisted rotated token, saw %v", refreshTokensSeen)
	}
}

// 导入时声明的 profileArn 必须在该凭证自己能列出的 profile 里；列不出来也不放行。
func TestImportExternalIdpProfileMustBeOffered(t *testing.T) {
	access := jwt(map[string]any{"iss": tIssuer, "oid": "o"})
	setup := func(profiles func() (int, string)) *Service {
		f := newFakeRT()
		f.on(routeMSToken, func(*http.Request, []byte) (int, string) {
			b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "rt", "expires_in": 3600})
			return 200, string(b)
		})
		f.on(routeProfiles, func(*http.Request, []byte) (int, string) { return profiles() })
		f.on(routeUsage, func(*http.Request, []byte) (int, string) { return 200, usageJSON("u", "e@corp.example", 1, 10) })
		return newSvc(t, f)
	}
	text := `{"refreshToken":"rt0","clientId":"` + tAppID + `","authMethod":"external_idp","issuerUrl":"` + tIssuer + `","profileArn":"` + testArn + `"}`
	items, _ := kiro.ParseImport(text)
	if len(items) != 1 {
		t.Fatal("parse")
	}
	if res := setup(func() (int, string) { return 200, profilesBody(testArn) }).Import(context.Background(), items); !res[0].OK {
		t.Fatalf("offered profile must be accepted: %+v", res[0])
	}
	if res := setup(func() (int, string) { return 200, profilesBody(otherArn) }).Import(context.Background(), items); res[0].OK || !strings.Contains(res[0].Error, "profile") {
		t.Fatalf("a profile that is not offered must be rejected: %+v", res[0])
	}
	if res := setup(func() (int, string) { return 500, `{"message":"boom"}` }).Import(context.Background(), items); res[0].OK {
		t.Fatalf("unverifiable profile must be rejected: %+v", res[0])
	}
}

// 篡改过的企业 SSO 配置在入库前被拒绝，不会向任何地方发请求。
func TestImportExternalIdpRejectsForeignConfig(t *testing.T) {
	f := newFakeRT()
	s := newSvc(t, f)
	res := s.importWith(context.Background(), []kiro.ImportItem{{
		RefreshToken: "rt", ClientID: tAppID, AuthMethod: kiro.AuthMethodExternalIDP, Provider: kiro.ProviderAzureAD,
		TokenEndpoint: "https://evil.example/" + tTenant + "/oauth2/v2.0/token", IssuerURL: "https://evil.example/" + tTenant + "/v2.0",
		Scopes: "api://" + tAppID + "/codewhisperer:conversations offline_access",
	}}, "import", true)
	if res[0].OK || !strings.Contains(res[0].Error, "企业 SSO 配置无效") {
		t.Fatalf("foreign config must be rejected: %+v", res[0])
	}
	if n := len(f.hits); n != 0 {
		t.Fatalf("rejected config reached the network: %v", f.hits)
	}
	if list, _ := auth.LoadDir(s.AuthDir); len(list) != 0 {
		t.Fatal("rejected config must not be saved")
	}
}

// 网页登录（社交）：用刚换到的 token 建档，不再多刷新一次（授权码刚换出的 token 是新的）。
func TestNewAccountFromPortalSocial(t *testing.T) {
	f := newFakeRT()
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer at-portal" || r.Header.Get("TokenType") != "" {
			return 403, `{"message":"bad"}`
		}
		return 200, usageJSON("user-portal", "p@example.com", 5, 50)
	})
	s := newSvc(t, f)
	res := &kiro.PortalResult{Kind: "social", Tokens: kiro.TokenSet{AccessToken: "at-portal", RefreshToken: "rt-portal", ExpiresAt: time.Now().Unix() + 3600, ProfileArn: testArn}}
	a, err := s.NewAccountFromPortal(context.Background(), res, "")
	if err != nil {
		t.Fatal(err)
	}
	m := a.KiroMeta()
	if m.AuthMethod != "social" || m.Provider != "Social" || m.ProfileArn != testArn || m.Source != "portal_social" || a.Nickname != "p@example.com" {
		t.Fatalf("meta: %+v nick=%q", m, a.Nickname)
	}
	if f.count(routeSocial) != 0 {
		t.Error("a freshly exchanged token must not be refreshed again")
	}
	if _, err := s.NewAccountFromPortal(context.Background(), nil, ""); err == nil {
		t.Error("nil result must be rejected")
	}
}

// 网页登录（企业 SSO）：多 profile 时用户的选择写进账号；列 profile 用的是刚登录的 token。
func TestPortalMicrosoftProfilesAndAccount(t *testing.T) {
	f := newFakeRT()
	access := jwt(map[string]any{"iss": tIssuer, "oid": "oid-1", "email": "m@corp.example"})
	f.on(routeProfiles, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer "+access || r.Header.Get("TokenType") != "EXTERNAL_IDP" {
			return 403, `{"message":"bad"}`
		}
		return 200, profilesBody(testArn, otherArn)
	})
	f.on(routeUsage, func(*http.Request, []byte) (int, string) { return 200, usageJSON("", "", 1, 10) })
	s := newSvc(t, f)
	res := &kiro.PortalResult{
		Kind: kiro.AuthMethodExternalIDP, Tokens: kiro.TokenSet{AccessToken: access, RefreshToken: "rt", ExpiresAt: time.Now().Unix() + 3600},
		ClientID: tAppID, TokenEndpoint: "https://login.microsoftonline.com/" + tTenant + "/oauth2/v2.0/token", IssuerURL: tIssuer,
		Scopes: "api://" + tAppID + "/codewhisperer:conversations api://" + tAppID + "/codewhisperer:completions offline_access",
		Email:  "m@corp.example", UserID: tIssuer + ".oid-1",
	}
	ps, kerr := s.PortalProfiles(context.Background(), res)
	if kerr != nil || len(ps) != 2 {
		t.Fatalf("profiles: %v %v", ps, kerr)
	}
	a, err := s.NewAccountFromPortal(context.Background(), res, otherArn)
	if err != nil {
		t.Fatal(err)
	}
	if m := a.KiroMeta(); m.ProfileArn != otherArn || m.AuthMethod != "external_idp" || m.Source != "portal_microsoft" || m.ClientSecret != "" {
		t.Fatalf("meta: %+v", m)
	}
	// 非企业 SSO 的结果不需要选 profile。
	if ps, kerr := s.PortalProfiles(context.Background(), &kiro.PortalResult{Kind: "social"}); ps != nil || kerr != nil {
		t.Errorf("social result has no profile selection: %v %v", ps, kerr)
	}
}

// SSO token 批量导入：每个 token 独立成败；失败原因里不出现 token 本身。
func TestImportSSOTokens(t *testing.T) {
	const good, bad = "GOOD-SSO-TOKEN-VALUE", "EXPIRED-SSO-TOKEN-VALUE"
	f := newFakeRT()
	oidc := "oidc.us-east-1.amazonaws.com"
	f.on(oidc+"/client/register", func(*http.Request, []byte) (int, string) { return 200, `{"clientId":"cid","clientSecret":"csec"}` })
	f.on(oidc+"/device_authorization", func(*http.Request, []byte) (int, string) {
		return 200, `{"deviceCode":"dc","userCode":"UC","interval":1}`
	})
	f.on("portal.sso.us-east-1.amazonaws.com/token/whoAmI", func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer "+good {
			return 401, `{"message":"Unauthorized"}`
		}
		return 200, `{}`
	})
	f.on("portal.sso.us-east-1.amazonaws.com/session/device", func(*http.Request, []byte) (int, string) { return 200, `{"token":"ds"}` })
	f.on(oidc+"/device_authorization/accept_user_code", func(*http.Request, []byte) (int, string) { return 200, `{}` })
	f.on(oidc+routeTokenPath, func(*http.Request, []byte) (int, string) {
		return 200, `{"accessToken":"at-sso","refreshToken":"rt-sso","expiresIn":3600}`
	})
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer at-sso" {
			return 403, `{"message":"bad"}`
		}
		return 200, usageJSON("user-sso", "sso@example.com", 3, 50)
	})
	s := newSvc(t, f)
	res := s.ImportSSOTokens(context.Background(), []string{bad, good}, "")
	if len(res) != 2 || res[0].OK || !res[1].OK {
		t.Fatalf("results: %+v", res)
	}
	if strings.Contains(res[0].Error, bad) || strings.Contains(res[0].Nickname, bad) || !strings.Contains(res[0].Nickname, "#1") {
		t.Fatalf("failure must identify the entry by index only: %+v", res[0])
	}
	m := res[1].Account.KiroMeta()
	if m.AuthMethod != "idc" || m.Provider != "BuilderId" || m.ClientID != "cid" || m.ClientSecret != "csec" || m.Source != "sso_token" || m.StartURL != kiro.BuilderIDStartURL {
		t.Fatalf("meta: %+v", m)
	}
	raw, _ := os.ReadFile(filepath.Join(s.AuthDir, auth.KiroFileName(res[1].UID)))
	if strings.Contains(string(raw), good) {
		t.Fatal("the SSO token (a live portal session) must never be written to disk")
	}
}

const routeTokenPath = "/token"

// 本机 IDE 缓存导入：access token 还有效就不刷新（不轮换 IDE 手里的 refresh token）；已过期才刷新。
func TestImportLocalCacheDefersRotation(t *testing.T) {
	dir := t.TempDir()
	write := func(expires string) {
		b, _ := json.Marshal(map[string]any{"accessToken": "at-cache", "refreshToken": "rt-cache", "authMethod": "social", "provider": "Github", "expiresAt": expires})
		if err := os.WriteFile(filepath.Join(dir, "kiro-auth-token.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeRT()
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, string) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
			return 403, `{"message":"bad"}`
		}
		return 200, usageJSON("user-cache", "c@example.com", 1, 50)
	})
	f.on(routeSocial, func(*http.Request, []byte) (int, string) {
		return 200, `{"accessToken":"at-refreshed","refreshToken":"rt-rotated","expiresIn":3600}`
	})
	s := newSvc(t, f)
	s.LocalCacheDir = dir

	write(time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339))
	res, skipped := s.ImportLocalCache(context.Background())
	if len(res) != 1 || !res[0].OK || len(skipped) != 0 {
		t.Fatalf("fresh cache: %+v %v", res, skipped)
	}
	if f.count(routeSocial) != 0 {
		t.Fatal("a still-valid cached token must not trigger a refresh (it would rotate the IDE's refresh token)")
	}
	if a := res[0].Account; a.RefreshTokenValue() != "rt-cache" || a.KiroMeta().Source != "local_cache" {
		t.Fatalf("account: rt=%q source=%q", a.RefreshTokenValue(), a.KiroMeta().Source)
	}

	write(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	res, _ = s.ImportLocalCache(context.Background())
	if len(res) != 1 || !res[0].OK || f.count(routeSocial) != 1 || res[0].Account.RefreshTokenValue() != "rt-rotated" {
		t.Fatalf("expired cache must be refreshed once: %+v refreshes=%d", res, f.count(routeSocial))
	}

	// 目录里没有缓存：说明原因，不返回结果。
	s.LocalCacheDir = t.TempDir()
	if res, skipped := s.ImportLocalCache(context.Background()); len(res) != 0 || len(skipped) != 1 {
		t.Fatalf("missing cache: %+v %v", res, skipped)
	}
}

// 超额开关：状态写回账号；「额度用完是否冷却」只在没开超额时成立。
func TestOverageAndExhaustPolicy(t *testing.T) {
	f := newFakeRT()
	status := "DISABLED"
	var posted map[string]any
	f.on(routeUsage, func(*http.Request, []byte) (int, string) {
		b, _ := json.Marshal(map[string]any{
			"overageConfiguration": map[string]any{"overageStatus": status},
			"subscriptionInfo":     map[string]any{"subscriptionTitle": "KIRO PRO", "subscriptionType": "Q_DEVELOPER_STANDALONE_PRO", "overageCapability": "OVERAGE_CAPABLE"},
			"usageBreakdownList": []any{map[string]any{"resourceType": "AGENTIC_REQUEST", "currentUsage": 100, "usageLimit": 100,
				"overageCap": 50.0, "overageRate": 0.04, "currentOverages": 2.0}},
		})
		return 200, string(b)
	})
	f.on(routeSetPref, func(_ *http.Request, body []byte) (int, string) {
		_ = json.Unmarshal(body, &posted)
		if oc, _ := posted["overageConfiguration"].(map[string]any); oc != nil {
			status, _ = oc["overageStatus"].(string)
		}
		return 200, `{}`
	})
	s := newSvc(t, f)
	a := auth.NewKiro("kover", "n", "at", "rt", time.Now().Add(time.Hour).Unix(), auth.KiroMeta{
		AuthMethod: "social", Provider: "Github", Region: "us-east-1", ProfileArn: testArn,
	})
	a.FilePath = filepath.Join(s.AuthDir, auth.KiroFileName(a.UID))

	info, kerr := s.Overage(context.Background(), a)
	if kerr != nil || info.Status != "DISABLED" || !info.Capable || info.CapUSD != 50 || info.RateUSD != 0.04 || info.CurrentUSD != 2 {
		t.Fatalf("overage: %+v %v", info, kerr)
	}
	if OverageEnabled(a) || !ShouldCoolOnExhausted(a, 0, 100) {
		t.Fatal("overage off + exhausted → must cool")
	}
	if ShouldCoolOnExhausted(a, 0, 0) || ShouldCoolOnExhausted(a, 5, 100) {
		t.Fatal("unknown total or remaining credits must not cool")
	}

	info, kerr = s.SetOverage(context.Background(), a, true)
	if kerr != nil || info.Status != "ENABLED" {
		t.Fatalf("enable: %+v %v", info, kerr)
	}
	if oc, _ := posted["overageConfiguration"].(map[string]any); oc["overageStatus"] != "ENABLED" || posted["profileArn"] != testArn {
		t.Fatalf("posted: %v", posted)
	}
	if !OverageEnabled(a) || ShouldCoolOnExhausted(a, 0, 100) {
		t.Fatal("overage on → exhausted account must keep serving")
	}
	// 状态落盘：重启后仍是打开的。
	if back := loadOne(t, s.AuthDir); !OverageEnabled(back) {
		t.Fatal("overage status must persist")
	}
	// 额度刷新（Balance）也顺带更新状态：上游那边被别处关掉后这里跟着变。
	status = "DISABLED"
	if _, _, _, kerr := s.Balance(context.Background(), a); kerr != nil || OverageEnabled(a) {
		t.Fatalf("balance must refresh overage status: %v enabled=%v", kerr, OverageEnabled(a))
	}
}

// 回归（真机发现）：学生号的上游值是 OVERAGE_INCAPABLE，其中没有 "NOT"，
// 只检查否定词会把「不支持」判成「支持」，前端就会让用户确认开启一个注定被拒绝的付费开关。
func TestOverageCapableRealUpstreamValues(t *testing.T) {
	for capability, want := range map[string]bool{
		"OVERAGE_CAPABLE":     true,
		"overage_capable":     true,
		" OVERAGE_CAPABLE ":   true,
		"OVERAGE_INCAPABLE":   false, // 真机：KIRO STUDENT
		"NOT_OVERAGE_CAPABLE": false,
		"OVERAGE_UNSUPPORTED": false,
		"":                    true, // 上游没给：未知，放行由上游自己决定
	} {
		if got := overageCapable(capability); got != want {
			t.Errorf("overageCapable(%q) = %v, want %v", capability, got, want)
		}
	}
	// 经由 Usage → OverageInfo 的完整路径。
	if info := overageInfo(kiro.Usage{OverageCapability: "OVERAGE_INCAPABLE", OverageStatus: "DISABLED"}); info.Capable {
		t.Error("OVERAGE_INCAPABLE must not be reported as capable")
	}
	if info := overageInfo(kiro.Usage{OverageCapability: "OVERAGE_CAPABLE"}); !info.Capable {
		t.Error("OVERAGE_CAPABLE must be reported as capable")
	}
}
