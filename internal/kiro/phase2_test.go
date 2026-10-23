package kiro

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
)

const (
	testTenant = "11111111-2222-3333-4444-555555555555"
	testAppID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testIssuer = "https://login.microsoftonline.com/" + testTenant + "/v2.0"
	hostMS     = "login.microsoftonline.com"
	routeMSTok = hostMS + "/" + testTenant + "/oauth2/v2.0/token"
	routeMSCfg = hostMS + "/" + testTenant + "/v2.0/.well-known/openid-configuration"
	routeSoc   = "prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
)

var testScopes = "api://" + testAppID + "/codewhisperer:conversations api://" + testAppID + "/codewhisperer:completions offline_access"

func noSleepClient(f *fakeUpstream) *Client {
	return NewClient(Options{Transport: f, Logf: func(string, ...any) {}, Sleep: func(context.Context, time.Duration) error { return nil }})
}

// 构造一个带 iss / oid / email 声明的假 JWT（只有 payload 有意义）。
func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func jsonResp(status int, v any) func(*http.Request, []byte) (int, http.Header, []byte) {
	b, _ := json.Marshal(v)
	return errResp(status, string(b))
}

// ── 企业 SSO 的配置校验 ─────────────────────────────────────────────────────

func TestValidateExternalIdpEndpoint(t *testing.T) {
	for _, ok := range []string{
		"https://login.microsoftonline.com/" + testTenant + "/oauth2/v2.0/token",
		"https://login.microsoftonline.us/x", "https://login.partner.microsoftonline.cn/x",
	} {
		if err := ValidateExternalIdpEndpoint(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://login.microsoftonline.com/x",           // 非 https
		"https://login.microsoftonline.com.evil.com/x", // 后缀伪装
		"https://evil.com/login.microsoftonline.com",   // 路径伪装
		"https://user:pw@login.microsoftonline.com/x",  // userinfo
		"https://login.microsoftonline.com:8443/x",     // 非默认端口
		"https://127.0.0.1/x", "https://[::1]/x",       // IP 字面量
		"https://login.microsoftonline.com/x#frag", "", "::x", // fragment / 空 / 非法
	} {
		if err := ValidateExternalIdpEndpoint(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestExternalIdpScopesAndConfig(t *testing.T) {
	got, err := NormalizeExternalIdpScopes(testScopes+" offline_access", testAppID)
	if err != nil || strings.Count(got, "offline_access") != 1 {
		t.Fatalf("normalize: %q %v", got, err)
	}
	for name, sc := range map[string]string{
		"no offline":   "api://" + testAppID + "/codewhisperer:conversations",
		"no kiro":      "openid offline_access",
		"foreign":      testScopes + " https://graph.microsoft.com/.default",
		"wrong app":    "api://ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee/codewhisperer:conversations offline_access",
		"unknown name": "api://" + testAppID + "/admin offline_access",
		"empty":        "  ",
	} {
		if _, err := NormalizeExternalIdpScopes(sc, testAppID); err == nil {
			t.Errorf("%s: scopes must be rejected", name)
		}
	}
	if _, err := NormalizeExternalIdpScopes(testScopes, "not-a-uuid"); err == nil {
		t.Error("non-UUID client id must be rejected")
	}

	ep, iss, sc := ExternalIdpConfigFromIssuer(testIssuer+"/", testAppID)
	if ep != "https://login.microsoftonline.com/"+testTenant+"/oauth2/v2.0/token" || iss != testIssuer || !strings.Contains(sc, "offline_access") {
		t.Fatalf("from issuer: %q %q %q", ep, iss, sc)
	}
	if err := ValidateExternalIdpConfig(testAppID, ep, iss, sc); err != nil {
		t.Fatalf("derived config must validate: %v", err)
	}
	// 端点租户与签发者不一致 / 签发者主机不在白名单：必须拒绝。
	other := "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/oauth2/v2.0/token"
	if ValidateExternalIdpConfig(testAppID, other, iss, sc) == nil {
		t.Error("tenant mismatch must be rejected")
	}
	if ValidateExternalIdpConfig(testAppID, "https://evil.com/"+testTenant+"/oauth2/v2.0/token", "https://evil.com/"+testTenant+"/v2.0", sc) == nil {
		t.Error("foreign host must be rejected")
	}
	if ep2, iss2, _ := ExternalIdpConfigFromTokenEndpoint(ep, testAppID); ep2 != ep || iss2 != iss {
		t.Errorf("from token endpoint: %q %q", ep2, iss2)
	}
	// Kiro Account Manager 的 userId 与 JWT iss 两条补全路径。
	if ep3, iss3, _ := DeriveExternalIdpConfig(testIssuer+".oid-123", testAppID, ""); ep3 != ep || iss3 != iss {
		t.Errorf("derive from KAM userId: %q %q", ep3, iss3)
	}
	jwt := fakeJWT(map[string]any{"iss": testIssuer, "oid": "oid-123", "email": "u@corp.example"})
	if _, iss4, _ := DeriveExternalIdpConfig("", testAppID, jwt); iss4 != iss {
		t.Errorf("derive from JWT iss: %q", iss4)
	}
	if email, uid := ExternalIdpIdentity(jwt); email != "u@corp.example" || uid != testIssuer+".oid-123" {
		t.Errorf("identity: %q %q", email, uid)
	}
}

// ── 企业 SSO 刷新 ──────────────────────────────────────────────────────────

var msCred = Cred{
	UID: "ms1", AuthMethod: AuthMethodExternalIDP, Provider: ProviderAzureAD, RefreshToken: "rt-old",
	ClientID: testAppID, TokenEndpoint: "https://login.microsoftonline.com/" + testTenant + "/oauth2/v2.0/token",
	IssuerURL: testIssuer, Scopes: testScopes,
}

func TestRefreshExternalIdp(t *testing.T) {
	f := newFake()
	f.on(routeMSTok, func(r *http.Request, body []byte) (int, http.Header, []byte) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			return 400, nil, []byte(`{"error":"bad_content_type"}`)
		}
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" ||
			form.Get("client_id") != testAppID || form.Get("scope") == "" || form.Get("client_secret") != "" || form.Get("redirect_uri") != "" {
			return 400, nil, []byte(`{"error":"invalid_request","error_description":"unexpected form"}`)
		}
		return 200, nil, []byte(`{"access_token":"at-new","refresh_token":"rt-rotated","expires_in":3599}`)
	})
	c := testClient(f)
	ts, err := c.Refresh(context.Background(), msCred)
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "at-new" || ts.RefreshToken != "rt-rotated" || ts.ExpiresAt < time.Now().Unix()+3000 {
		t.Fatalf("tokens: %+v", ts)
	}
	// 响应没带 refresh_token：沿用旧的。
	f.on(routeMSTok, jsonResp(200, map[string]any{"access_token": "at-2", "expires_in": 60}))
	if ts, err = c.Refresh(context.Background(), msCred); err != nil || ts.RefreshToken != "rt-old" {
		t.Fatalf("keep old refresh token: %+v %v", ts, err)
	}
	// invalid_grant → 需要重新登录。
	f.on(routeMSTok, jsonResp(400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS70008: expired"}))
	if _, err = c.Refresh(context.Background(), msCred); err == nil || err.Kind != KindSessionDead || !strings.Contains(err.Msg, "AADSTS70008") {
		t.Fatalf("invalid_grant: %v", err)
	}
}

// 持久化的配置被篡改成别的主机 / 别的租户：refresh token 一个字节都不能发出去。
func TestRefreshExternalIdpNeverLeavesAllowList(t *testing.T) {
	f := newFake()
	c := testClient(f)
	for name, mut := range map[string]func(*Cred){
		"foreign host": func(c *Cred) { c.TokenEndpoint = "https://evil.example/" + testTenant + "/oauth2/v2.0/token" },
		"other tenant": func(c *Cred) {
			c.TokenEndpoint = "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/oauth2/v2.0/token"
		},
		"bad scope":   func(c *Cred) { c.Scopes = "offline_access https://graph.microsoft.com/.default" },
		"missing":     func(c *Cred) { c.TokenEndpoint = "" },
		"missing app": func(c *Cred) { c.ClientID = "" },
		"plain http": func(c *Cred) {
			c.TokenEndpoint = "http://login.microsoftonline.com/" + testTenant + "/oauth2/v2.0/token"
		},
		"issuer tamper": func(c *Cred) { c.IssuerURL = "https://evil.example/" + testTenant + "/v2.0" },
	} {
		cred := msCred
		mut(&cred)
		if _, err := c.Refresh(context.Background(), cred); err == nil || err.Kind != KindSessionDead {
			t.Errorf("%s: want session_dead, got %v", name, err)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("%d request(s) leaked to the network", len(f.requests))
	}
}

// ── 网页登录：社交（Google / GitHub） ──────────────────────────────────────

func TestPortalSocialLogin(t *testing.T) {
	f := newFake()
	var gotBody map[string]string
	f.on(routeSoc, func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		_ = json.Unmarshal(body, &gotBody)
		return 200, nil, []byte(`{"accessToken":"at-s","refreshToken":"rt-s","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/P"}`)
	})
	c := testClient(f)
	sess, loginURL := c.StartPortalLogin()
	u, _ := url.Parse(loginURL)
	q := u.Query()
	if u.Host != "app.kiro.dev" || q.Get("state") == "" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://localhost:3128" || q.Get("redirect_from") != "KiroIDE" {
		t.Fatalf("login url: %s", loginURL)
	}
	if sess.Stage() != "kiro" {
		t.Fatalf("stage=%s", sess.Stage())
	}
	ctx := context.Background()

	// 各种不该接受的回调：别的主机 / 端口 / scheme / userinfo / 空 / state 不符 / 没有 code。
	for name, cb := range map[string]string{
		"foreign host": "http://evil.example:3128/?code=c&state=" + q.Get("state"),
		"wrong port":   "http://localhost:3129/?code=c&state=" + q.Get("state"),
		"https":        "https://localhost:3128/?code=c&state=" + q.Get("state"),
		"userinfo":     "http://u:p@localhost:3128/?code=c&state=" + q.Get("state"),
		"empty":        "",
		"bad state":    "http://localhost:3128/?code=c&state=WRONG",
		"no state":     "http://localhost:3128/?code=c",
		"nothing":      "http://localhost:3128/",
		"dup code":     "http://localhost:3128/?code=a&code=b&state=" + q.Get("state"),
		"ms callback":  "http://localhost:3128/oauth/callback?code=c&state=" + q.Get("state"),
		"idc option":   "http://localhost:3128/signin/callback?login_option=awsidc&issuer_url=https%3A%2F%2Fd-1.awsapps.com%2Fstart",
	} {
		if _, err := c.ContinuePortalLogin(ctx, sess, cb); err == nil || err.Kind != KindBadRequest {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}
	if len(f.calls(routeSoc)) != 0 || sess.Expired() {
		t.Fatalf("rejected callbacks must not exchange the code or kill the session (calls=%d expired=%v)", len(f.calls(routeSoc)), sess.Expired())
	}

	prog, err := c.ContinuePortalLogin(ctx, sess, "http://localhost:3128/signin/callback?code=auth-code-1&state="+q.Get("state"))
	if err != nil || prog.Result == nil || prog.Result.Kind != "social" {
		t.Fatalf("exchange: %+v %v", prog, err)
	}
	if prog.Result.Tokens.AccessToken != "at-s" || prog.Result.Tokens.RefreshToken != "rt-s" || prog.Result.Tokens.ProfileArn == "" {
		t.Fatalf("tokens: %+v", prog.Result.Tokens)
	}
	// PKCE：交换时带的 verifier 必须能推出登录 URL 里的 challenge。
	if gotBody["code"] != "auth-code-1" || gotBody["redirect_uri"] != "http://localhost:3128" || pkceChallenge(gotBody["code_verifier"]) != q.Get("code_challenge") {
		t.Fatalf("exchange body: %v", gotBody)
	}
	// 用过即废：会话已清空，再用同一个回调必须失败，且不再请求上游。
	if _, err := c.ContinuePortalLogin(ctx, sess, "http://localhost:3128/?code=auth-code-1&state="+q.Get("state")); err == nil {
		t.Fatal("a finished session must not accept another callback")
	}
	if len(f.calls(routeSoc)) != 1 {
		t.Fatalf("code exchanged %d times", len(f.calls(routeSoc)))
	}
}

func TestPortalSocialExchangeFailureKillsSession(t *testing.T) {
	f := newFake()
	f.on(routeSoc, errResp(400, `{"error":"invalid_grant"}`))
	c := testClient(f)
	sess, loginURL := c.StartPortalLogin()
	state := mustQuery(t, loginURL).Get("state")
	if _, err := c.ContinuePortalLogin(context.Background(), sess, "http://localhost:3128/?code=used&state="+state); err == nil {
		t.Fatal("exchange failure must surface")
	}
	if !sess.Expired() {
		t.Fatal("a rejected (single-use) code must retire the session")
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// ── 网页登录：Microsoft 企业 SSO 两段 ──────────────────────────────────────

func msDiscoveryDoc() map[string]any {
	return map[string]any{
		"issuer":                 testIssuer,
		"authorization_endpoint": "https://login.microsoftonline.com/" + testTenant + "/oauth2/v2.0/authorize",
		"token_endpoint":         "https://login.microsoftonline.com/" + testTenant + "/oauth2/v2.0/token",
	}
}

func TestPortalMicrosoftLogin(t *testing.T) {
	f := newFake()
	f.on(routeMSCfg, jsonResp(200, msDiscoveryDoc()))
	access := fakeJWT(map[string]any{"iss": testIssuer, "oid": "oid-9", "preferred_username": "alice@corp.example"})
	var form url.Values
	f.on(routeMSTok, func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		form, _ = url.ParseQuery(string(body))
		b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "rt-ms", "expires_in": 3600})
		return 200, nil, b
	})
	c := testClient(f)
	ctx := context.Background()
	sess, loginURL := c.StartPortalLogin()
	state := mustQuery(t, loginURL).Get("state")

	desc := url.Values{"login_option": {"external_idp"}, "issuer_url": {testIssuer}, "client_id": {testAppID},
		"scopes": {testScopes}, "login_hint": {"alice@corp.example"}, "state": {state}}
	bad := func(mut func(url.Values)) string {
		v := url.Values{}
		for k, vs := range desc {
			v[k] = vs
		}
		mut(v)
		return "http://localhost:3128/signin/callback?" + v.Encode()
	}
	for name, cb := range map[string]string{
		"foreign issuer": bad(func(v url.Values) { v.Set("issuer_url", "https://evil.example/"+testTenant+"/v2.0") }),
		"non-uuid app":   bad(func(v url.Values) { v.Set("client_id", "not-a-uuid") }),
		"foreign scope":  bad(func(v url.Values) { v.Set("scopes", testScopes+" https://graph.microsoft.com/.default") }),
		"no scopes":      bad(func(v url.Values) { v.Del("scopes") }),
		"wrong state":    bad(func(v url.Values) { v.Set("state", "nope") }),
	} {
		if _, err := c.ContinuePortalLogin(ctx, sess, cb); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("invalid descriptors must not reach the network (%d requests)", len(f.requests))
	}

	// 第 1 段：描述符（state 缺省也允许）→ 微软授权地址。
	prog, err := c.ContinuePortalLogin(ctx, sess, bad(func(v url.Values) { v.Del("state") }))
	if err != nil || prog.AuthorizationURL == "" || sess.Stage() != "microsoft" {
		t.Fatalf("leg 1: %+v %v stage=%s", prog, err, sess.Stage())
	}
	au, _ := url.Parse(prog.AuthorizationURL)
	aq := au.Query()
	if au.Host != hostMS || au.Path != "/"+testTenant+"/oauth2/v2.0/authorize" || aq.Get("client_id") != testAppID ||
		aq.Get("redirect_uri") != "http://localhost:3128/oauth/callback" || aq.Get("code_challenge_method") != "S256" ||
		aq.Get("response_type") != "code" || aq.Get("login_hint") != "alice@corp.example" || aq.Get("state") == "" {
		t.Fatalf("authorize url: %s", prog.AuthorizationURL)
	}
	// 重复投喂第 1 段的地址（stage 已前进）：走第 2 段校验，必须因路径不对被拒。
	if _, err := c.ContinuePortalLogin(ctx, sess, bad(func(url.Values) {})); err == nil {
		t.Error("leg-1 callback must not be accepted in leg 2")
	}
	// 第 2 段：state 不符被拒（会话保留），正确的换 token。
	if _, err := c.ContinuePortalLogin(ctx, sess, "http://localhost:3128/oauth/callback?code=c&state=WRONG"); err == nil || sess.Expired() {
		t.Fatalf("wrong state: err=%v expired=%v", err, sess.Expired())
	}
	prog, err = c.ContinuePortalLogin(ctx, sess, "http://localhost:3128/oauth/callback?code=ms-code&state="+aq.Get("state"))
	if err != nil || prog.Result == nil {
		t.Fatalf("leg 2: %+v %v", prog, err)
	}
	r := prog.Result
	if r.Kind != AuthMethodExternalIDP || r.ClientID != testAppID || r.IssuerURL != testIssuer || r.Tokens.RefreshToken != "rt-ms" ||
		r.Email != "alice@corp.example" || r.UserID != testIssuer+".oid-9" {
		t.Fatalf("result: %+v", r)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "ms-code" || form.Get("client_secret") != "" ||
		form.Get("redirect_uri") != "http://localhost:3128/oauth/callback" || pkceChallenge(form.Get("code_verifier")) != aq.Get("code_challenge") {
		t.Fatalf("token form: %v", form)
	}
	if err := ValidateExternalIdpConfig(r.ClientID, r.TokenEndpoint, r.IssuerURL, r.Scopes); err != nil {
		t.Fatalf("result config must validate: %v", err)
	}
	if !sess.Expired() || sess.Stage() != "done" {
		t.Fatal("session must be retired after success")
	}
}

// 发现文档里的端点被换成别的主机 / 别的租户：拒绝，不进入第 2 段。
func TestPortalMicrosoftDiscoveryTamper(t *testing.T) {
	for name, mut := range map[string]func(map[string]any){
		"foreign authorize": func(d map[string]any) {
			d["authorization_endpoint"] = "https://evil.example/" + testTenant + "/oauth2/v2.0/authorize"
		},
		"other tenant token": func(d map[string]any) {
			d["token_endpoint"] = "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/oauth2/v2.0/token"
		},
		"issuer mismatch": func(d map[string]any) {
			d["issuer"] = "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/v2.0"
		},
	} {
		f := newFake()
		doc := msDiscoveryDoc()
		mut(doc)
		f.on(routeMSCfg, jsonResp(200, doc))
		c := testClient(f)
		sess, loginURL := c.StartPortalLogin()
		state := mustQuery(t, loginURL).Get("state")
		cb := "http://localhost:3128/signin/callback?" + url.Values{"login_option": {"external_idp"}, "issuer_url": {testIssuer},
			"client_id": {testAppID}, "scopes": {testScopes}, "state": {state}}.Encode()
		if _, err := c.ContinuePortalLogin(context.Background(), sess, cb); err == nil || sess.Stage() != "kiro" {
			t.Errorf("%s: want rejection staying in leg 1, got err=%v stage=%s", name, err, sess.Stage())
		}
	}
}

// 微软颁发的 access token 的签发者和登录签发者不一致：拒绝。
func TestPortalMicrosoftIssuerClaimMismatch(t *testing.T) {
	f := newFake()
	f.on(routeMSCfg, jsonResp(200, msDiscoveryDoc()))
	other := fakeJWT(map[string]any{"iss": "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/v2.0", "oid": "x"})
	f.on(routeMSTok, jsonResp(200, map[string]any{"access_token": other, "refresh_token": "rt", "expires_in": 60}))
	c := testClient(f)
	sess, loginURL := c.StartPortalLogin()
	state := mustQuery(t, loginURL).Get("state")
	prog, err := c.ContinuePortalLogin(context.Background(), sess, "http://localhost:3128/signin/callback?"+url.Values{
		"login_option": {"external_idp"}, "issuer_url": {testIssuer}, "client_id": {testAppID}, "scopes": {testScopes}, "state": {state}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	st := mustQuery(t, prog.AuthorizationURL).Get("state")
	if _, err := c.ContinuePortalLogin(context.Background(), sess, "http://localhost:3128/oauth/callback?code=c&state="+st); err == nil ||
		!strings.Contains(err.Msg, "签发者") {
		t.Fatalf("issuer mismatch must be rejected, got %v", err)
	}
}

func TestPortalSessionLifecycle(t *testing.T) {
	c := testClient(newFake())
	sess, _ := c.StartPortalLogin()
	sess.Retire()
	sess.Retire() // 幂等
	if !sess.Expired() {
		t.Fatal("retired session must report expired")
	}
	if _, err := c.ContinuePortalLogin(context.Background(), sess, "http://localhost:3128/?code=c&state=s"); err == nil {
		t.Fatal("retired session must reject callbacks")
	}
	if _, err := c.ContinuePortalLogin(context.Background(), nil, "x"); err == nil {
		t.Fatal("nil session must be rejected")
	}
	old, _ := c.StartPortalLogin()
	old.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := c.ContinuePortalLogin(context.Background(), old, "http://localhost:3128/?code=c&state=s"); err == nil || !old.Expired() {
		t.Fatal("expired session must be rejected and retired")
	}
}

// ── SSO token（x-amz-sso_authn）──────────────────────────────────────────

const (
	oidcHost    = "oidc.us-east-1.amazonaws.com"
	routeReg    = oidcHost + "/client/register"
	routeDevice = oidcHost + "/device_authorization"
	routeAccept = oidcHost + "/device_authorization/accept_user_code"
	routeAssoc  = oidcHost + "/device_authorization/associate_token"
	routeOIDCT  = oidcHost + "/token"
	portalHost  = "portal.sso.us-east-1.amazonaws.com"
	routeWho    = portalHost + "/token/whoAmI"
	routeDevSes = portalHost + "/session/device"
	secretSSO   = "AQoDYXdzEJr-SECRET-SSO-VALUE"
)

func ssoFake(polls *int) *fakeUpstream {
	f := newFake()
	f.on(routeReg, jsonResp(200, map[string]any{"clientId": "cid", "clientSecret": "csecret"}))
	f.on(routeDevice, jsonResp(200, map[string]any{"deviceCode": "dc", "userCode": "UC-1", "interval": 1}))
	f.on(routeWho, func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		if r.Header.Get("Authorization") != "Bearer "+secretSSO {
			return 401, nil, []byte(`{"message":"Unauthorized"}`)
		}
		return 200, nil, []byte(`{}`)
	})
	f.on(routeDevSes, func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		if r.Header.Get("Authorization") != "Bearer "+secretSSO {
			return 401, nil, nil
		}
		return 200, nil, []byte(`{"token":"dev-sess"}`)
	})
	f.on(routeAccept, func(r *http.Request, body []byte) (int, http.Header, []byte) {
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		if m["userCode"] != "UC-1" || m["userSessionId"] != "dev-sess" || r.Header.Get("Referer") != "https://view.awsapps.com/" || r.Header.Get("Authorization") != "" {
			return 400, nil, []byte(`{"error":"bad accept"}`)
		}
		return 200, nil, []byte(`{"deviceContext":{"deviceContextId":"dctx","clientId":"cid","clientType":"public"}}`)
	})
	f.on(routeAssoc, func(r *http.Request, body []byte) (int, http.Header, []byte) {
		if !strings.Contains(string(body), "dctx") || !strings.Contains(string(body), "dev-sess") || r.Header.Get("Authorization") != "" {
			return 400, nil, []byte(`{"error":"bad associate"}`)
		}
		return 200, nil, []byte(`{}`)
	})
	f.on(routeOIDCT, func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		*polls++
		if !strings.Contains(string(body), `"deviceCode":"dc"`) {
			return 400, nil, []byte(`{"error":"invalid_request"}`)
		}
		switch *polls {
		case 1:
			return 400, nil, []byte(`{"error":"authorization_pending"}`)
		case 2:
			return 400, nil, []byte(`{"error":"slow_down"}`)
		}
		return 200, nil, []byte(`{"accessToken":"at-sso","refreshToken":"rt-sso","expiresIn":3600}`)
	})
	return f
}

func TestImportFromSSOToken(t *testing.T) {
	polls := 0
	f := ssoFake(&polls)
	var slept []time.Duration
	c := NewClient(Options{Transport: f, Logf: func(string, ...any) {}, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	res, err := c.ImportFromSSOToken(context.Background(), "  "+secretSSO+" ", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tokens.AccessToken != "at-sso" || res.Tokens.RefreshToken != "rt-sso" || res.ClientID != "cid" || res.ClientSecret != "csecret" || res.Region != "us-east-1" {
		t.Fatalf("result: %+v", res)
	}
	if polls != 3 || len(slept) != 3 || slept[2] != 6*time.Second {
		t.Fatalf("polls=%d slept=%v (slow_down must add 5s)", polls, slept)
	}
	// SSO token 只能发给 AWS 门户，不能出现在 OIDC 请求里。
	for _, r := range f.requests {
		if r.Host == oidcHost && (strings.Contains(r.Header.Get("Authorization"), secretSSO) || strings.Contains(string(r.Body), secretSSO)) {
			t.Fatalf("SSO token leaked to %s%s", r.Host, r.Path)
		}
	}
}

func TestImportFromSSOTokenRejections(t *testing.T) {
	polls := 0
	f := ssoFake(&polls)
	c := noSleepClient(f)
	ctx := context.Background()
	// 无效 / 过期的 token：停在校验步骤，不再继续，错误信息不含 token 本身。
	_, err := c.ImportFromSSOToken(ctx, "expired-token-value", "")
	if err == nil || err.Kind != KindSessionDead || strings.Contains(err.Error(), "expired-token-value") {
		t.Fatalf("invalid token: %v", err)
	}
	if len(f.calls(routeDevSes))+len(f.calls(routeAccept))+len(f.calls(routeOIDCT)) != 0 {
		t.Fatal("must stop after whoAmI rejects the token")
	}
	for _, tok := range []string{"", "has space", "has\"quote", strings.Repeat("x", ssoMaxTokenBytes+1)} {
		if _, err := c.ImportFromSSOToken(ctx, tok, ""); err == nil || err.Kind != KindBadRequest {
			t.Errorf("token %.12q must be rejected as bad_request, got %v", tok, err)
		}
	}
	if _, err := c.ImportFromSSOToken(ctx, secretSSO, "evil.com/x"); err == nil || err.Kind != KindBadRequest {
		t.Errorf("bad region: %v", err)
	}
	// 授权被拒绝（不是 pending）：立即失败，不无限轮询。
	f.on(routeOIDCT, errResp(400, `{"error":"access_denied"}`))
	if _, err := c.ImportFromSSOToken(ctx, secretSSO, ""); err == nil {
		t.Fatal("access_denied must fail")
	}
}

// ── 本机 Kiro IDE 缓存 ─────────────────────────────────────────────────────

func writeJSONFile(t *testing.T, dir, name string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadLocalCache(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, dir, "kiro-auth-token.json", map[string]any{
		"accessToken": "at", "refreshToken": "rt-idc", "expiresAt": "2030-01-02T03:04:05.000Z",
		"authMethod": "IdC", "provider": "BuilderId", "region": "us-east-1", "clientIdHash": "abc123",
	})
	writeJSONFile(t, dir, "abc123.json", map[string]any{"clientId": "cid-1", "clientSecret": "cs-1", "expiresAt": "2030-01-01T00:00:00Z"})
	// 干扰项：另一个客户端文件不能被错配。
	writeJSONFile(t, dir, "zzz999.json", map[string]any{"clientId": "WRONG", "clientSecret": "WRONG"})

	items, skipped := ReadLocalCache(dir)
	if len(items) != 1 || len(skipped) != 0 {
		t.Fatalf("items=%+v skipped=%v", items, skipped)
	}
	if it := items[0]; it.AuthMethod != "idc" || it.ClientID != "cid-1" || it.ClientSecret != "cs-1" || it.RefreshToken != "rt-idc" || it.Provider != "BuilderId" || it.ExpiresAt == 0 {
		t.Fatalf("paired item: %+v", it)
	}

	// 社交登录没有 clientIdHash，不需要配对文件。
	soc := t.TempDir()
	writeJSONFile(t, soc, "kiro-auth-token.json", map[string]any{"accessToken": "at", "refreshToken": "rt-soc", "authMethod": "social", "provider": "Github"})
	if items, skipped := ReadLocalCache(soc); len(items) != 1 || items[0].AuthMethod != "social" || len(skipped) != 0 {
		t.Fatalf("social: %+v %v", items, skipped)
	}
}

func TestReadLocalCacheRejections(t *testing.T) {
	// 目录里没有缓存文件：给出明确说明而不是 panic。
	if items, skipped := ReadLocalCache(t.TempDir()); len(items) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "kiro-auth-token.json") {
		t.Fatalf("empty dir: %+v %v", items, skipped)
	}
	if items, skipped := ReadLocalCache(""); len(items) != 0 || len(skipped) != 1 {
		t.Fatalf("empty path: %+v %v", items, skipped)
	}
	// clientIdHash 带路径穿越：拒绝读取任何配对文件（即使目标文件存在）。
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_ = os.Mkdir(dir, 0o700)
	writeJSONFile(t, root, "outside.json", map[string]any{"clientId": "STOLEN", "clientSecret": "STOLEN"})
	for _, h := range []string{"../outside", "..\\outside", "a/b", "", "x y", strings.Repeat("a", 200)} {
		writeJSONFile(t, dir, "kiro-auth-token.json", map[string]any{"accessToken": "at", "refreshToken": "rt", "authMethod": "idc", "clientIdHash": h})
		items, skipped := ReadLocalCache(dir)
		if len(items) != 0 {
			t.Errorf("hash %q must not produce an item: %+v", h, items)
		}
		for _, it := range items {
			if it.ClientID == "STOLEN" {
				t.Fatalf("path traversal via hash %q", h)
			}
		}
		if len(skipped) == 0 {
			t.Errorf("hash %q: expected a skip reason", h)
		}
	}
	// 配对文件缺失：说明缺哪个文件。
	writeJSONFile(t, dir, "kiro-auth-token.json", map[string]any{"accessToken": "at", "refreshToken": "rt", "authMethod": "idc", "clientIdHash": "nope"})
	if items, skipped := ReadLocalCache(dir); len(items) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "nope.json") {
		t.Fatalf("missing pair: %+v %v", items, skipped)
	}
	// 不是 JSON / 过大。
	_ = os.WriteFile(filepath.Join(dir, "kiro-auth-token.json"), []byte("not json"), 0o600)
	if items, skipped := ReadLocalCache(dir); len(items) != 0 || len(skipped) != 1 {
		t.Fatalf("garbage: %+v %v", items, skipped)
	}
	_ = os.WriteFile(filepath.Join(dir, "kiro-auth-token.json"), []byte(strings.Repeat("a", localCacheMaxSize+1)), 0o600)
	if items, skipped := ReadLocalCache(dir); len(items) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "1MB") {
		t.Fatalf("oversize: %+v %v", items, skipped)
	}
}

// ── 导入：企业 SSO 记录 ────────────────────────────────────────────────────

func TestParseImportExternalIdp(t *testing.T) {
	jwt := fakeJWT(map[string]any{"iss": testIssuer, "oid": "oid-1"})
	items, skipped := ParseImport(`[
		{"refreshToken":"rt-kam","clientId":"` + testAppID + `","userId":"` + testIssuer + `.oid-1","authMethod":"external_idp","email":"k@corp.example"},
		{"refreshToken":"rt-ep","clientId":"` + testAppID + `","accessToken":"` + jwt + `","tokenEndpoint":"https://login.microsoftonline.com/` + testTenant + `/oauth2/v2.0/token"},
		{"refreshToken":"rt-evil","clientId":"` + testAppID + `","authMethod":"external_idp","issuerUrl":"https://evil.example/` + testTenant + `/v2.0"},
		{"refreshToken":"rt-noapp","authMethod":"azuread","issuerUrl":"` + testIssuer + `"},
		{"refreshToken":"rt-idc","clientId":"cid","clientSecret":"cs","authMethod":"idc","tokenEndpoint":"https://x.example/stray"}
	]`)
	if len(items) != 3 || len(skipped) != 2 {
		t.Fatalf("items=%d skipped=%v", len(items), skipped)
	}
	for _, it := range items[:2] {
		if it.AuthMethod != AuthMethodExternalIDP || it.Provider != ProviderAzureAD || it.ClientID != testAppID || it.ClientSecret != "" ||
			it.IssuerURL != testIssuer || !strings.HasSuffix(it.TokenEndpoint, "/oauth2/v2.0/token") || !strings.Contains(it.Scopes, "offline_access") {
			t.Errorf("external idp item: %+v", it)
		}
	}
	if items[0].Email != "k@corp.example" || items[0].UserID == "" {
		t.Errorf("kam identity: %+v", items[0])
	}
	// 显式 idc 的记录里夹带 tokenEndpoint：仍按 idc 处理，不被带去外部身份源路径。
	if items[2].AuthMethod != "idc" || items[2].TokenEndpoint != "" {
		t.Errorf("explicit idc must stay idc: %+v", items[2])
	}
	for _, s := range skipped {
		if strings.Contains(s, "evil.example") && !strings.Contains(s, "白名单") {
			t.Errorf("unhelpful skip reason: %s", s)
		}
	}
}

// ── 超额开关 ───────────────────────────────────────────────────────────────

func TestUsageParsesOverageAndSetOverage(t *testing.T) {
	f := newFake()
	status := "DISABLED"
	var setBody map[string]any
	f.on(routeUsage, func(*http.Request, []byte) (int, http.Header, []byte) {
		b, _ := json.Marshal(map[string]any{
			"overageConfiguration": map[string]any{"overageStatus": status},
			"subscriptionInfo":     map[string]any{"subscriptionTitle": "KIRO PRO", "subscriptionType": "Q_DEVELOPER_STANDALONE_PRO", "overageCapability": "OVERAGE_CAPABLE"},
			"usageBreakdownList": []any{map[string]any{"resourceType": "AGENTIC_REQUEST", "currentUsage": 1000, "usageLimit": 1000,
				"overageCap": 200.0, "overageRate": 0.04, "currentOverages": 1.5}},
		})
		return 200, nil, b
	})
	f.on(hostQ+"/setUserPreference", func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		_ = json.Unmarshal(body, &setBody)
		if oc, _ := setBody["overageConfiguration"].(map[string]any); oc != nil {
			status, _ = oc["overageStatus"].(string)
		}
		return 200, nil, []byte(`{}`)
	})
	c := testClient(f)
	u, err := c.UsageLimits(context.Background(), testCred)
	if err != nil || u.OverageStatus != "DISABLED" || u.OverageCapability != "OVERAGE_CAPABLE" || u.OverageCap != 200 || u.OverageRate != 0.04 || u.CurrentOverages != 1.5 {
		t.Fatalf("usage: %+v %v", u, err)
	}
	u, err = c.SetOverage(context.Background(), testCred, true)
	if err != nil || u.OverageStatus != "ENABLED" {
		t.Fatalf("enable: %+v %v", u, err)
	}
	oc, _ := setBody["overageConfiguration"].(map[string]any)
	if oc["overageStatus"] != "ENABLED" || setBody["profileArn"] != testCred.ProfileArn {
		t.Fatalf("set body: %v", setBody)
	}
	if u, err = c.SetOverage(context.Background(), testCred, false); err != nil || u.OverageStatus != "DISABLED" {
		t.Fatalf("disable: %+v %v", u, err)
	}
	// 设置被拒：错误原样返回，不假装成功。
	f.on(hostQ+"/setUserPreference", errResp(403, `{"message":"not allowed"}`))
	if _, err = c.SetOverage(context.Background(), testCred, true); err == nil {
		t.Fatal("rejected set must surface")
	}
}

// Kiro Account Manager 的 expiresAt 是不带时区的本地时间；套餐名要识别 STUDENT / PRO MAX。
func TestParseExpiresAtFormatsAndPlans(t *testing.T) {
	want := time.Date(2026, 10, 23, 23, 47, 28, 0, time.Local).Unix()
	for _, s := range []string{"2026/10/23 23:47:28", "2026-10-23 23:47:28", " 2026/10/23 23:47:28 "} {
		if got := parseExpiresAt(map[string]any{"expiresAt": s}); got != want {
			t.Errorf("expiresAt %q = %d, want %d", s, got, want)
		}
	}
	if got := parseExpiresAt(map[string]any{"expiresAt": "2030-01-02T03:04:05Z"}); got != time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Unix() {
		t.Errorf("RFC3339 must keep working, got %d", got)
	}
	if got := parseExpiresAt(map[string]any{"expiresAt": "not a date"}); got != 0 {
		t.Errorf("garbage must be 0, got %d", got)
	}
	if got := parseExpiresAt(map[string]any{"credentials": map[string]any{"expiresAt": "2026/10/23 23:47:28"}}); got != want {
		t.Errorf("nested credentials.expiresAt = %d, want %d", got, want)
	}
	for in, plan := range map[string]string{
		"KIRO STUDENT Q_DEVELOPER_STANDALONE_STUDENT": "STUDENT",
		"KIRO PRO MAX Q_DEVELOPER_STANDALONE_PRO_MAX": "PRO_MAX",
		"KIRO PRO+ Q_DEVELOPER_STANDALONE_PRO_PLUS":   "PRO_PLUS",
		"KIRO POWER Q_DEVELOPER_STANDALONE_POWER":     "POWER",
		"KIRO PRO Q_DEVELOPER_STANDALONE_PRO":         "PRO",
		"KIRO FREE Q_DEVELOPER_STANDALONE_FREE":       "FREE",
		"":                                            "FREE",
	} {
		if got := parsePlan(in); got != plan {
			t.Errorf("parsePlan(%q) = %q, want %q", in, got, plan)
		}
	}
}
