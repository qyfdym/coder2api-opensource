package panel

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

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const (
	pTenant = "11111111-2222-3333-4444-555555555555"
	pAppID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	pIssuer = "https://login.microsoftonline.com/" + pTenant + "/v2.0"
	pArnA   = "arn:aws:codewhisperer:us-east-1:123456789012:profile/AAA"
	pArnB   = "arn:aws:codewhisperer:us-east-1:123456789012:profile/BBB"
)

func pJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func newKiroPanel2(t *testing.T, rt *kiroFakeRT) (*Panel, *pool.Pool, *kiroacct.Service) {
	t.Helper()
	pl := pool.New("")
	svc := &kiroacct.Service{
		Client: kiro.NewClient(kiro.Options{Transport: rt, Logf: t.Logf,
			Sleep: func(context.Context, time.Duration) error { return nil }}),
		AuthDir: t.TempDir(), LocalCacheDir: t.TempDir(),
	}
	return New(Config{Version: "test", APIKey: "test-key", Pool: pl, Kiro: svc}), pl, svc
}

func profilesJSON(arns ...string) string {
	var ps []map[string]string
	for _, a := range arns {
		ps = append(ps, map[string]string{"arn": a, "profileName": "p"})
	}
	b, _ := json.Marshal(map[string]any{"profiles": ps})
	return string(b)
}

// 网页登录（社交）：start → 粘回错误地址（可重试）→ 粘回正确地址 → 建档入池。
func TestKiroPortalSocialEndToEnd(t *testing.T) {
	rt := &kiroFakeRT{}
	var exch map[string]string
	rt.on("prod.us-east-1.auth.desktop.kiro.dev/oauth/token", func(_ *http.Request, body []byte) (int, string) {
		_ = json.Unmarshal(body, &exch)
		return 200, `{"accessToken":"at-w","refreshToken":"rt-w","expiresIn":3600,"profileArn":"` + pArnA + `"}`
	})
	rt.on(kiroUsageRoute, func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer at-w" {
			return 403, `{"message":"bad"}`
		}
		return 200, kiroUsageBody("user-web", "web@example.com", 2, 50)
	})
	p, pl, _ := newKiroPanel2(t, rt)

	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/start", `{}`)
	if code != 200 || out["stage"] != "kiro" {
		t.Fatalf("start: %d %v", code, out)
	}
	state, _ := out["state"].(string)
	loginURL, _ := out["url"].(string)
	u, _ := url.Parse(loginURL)
	if u.Host != "app.kiro.dev" || u.Query().Get("redirect_uri") != "http://localhost:3128" {
		t.Fatalf("login url: %s", loginURL)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "code_verifier") {
		t.Fatal("the PKCE verifier must never be sent to the browser")
	}

	// 地址不对：回 200 + retry=true，会话仍可用；不会向上游换 token。
	post := func(cb string) map[string]any {
		b, _ := json.Marshal(map[string]string{"state": state, "callback_url": cb})
		_, o := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/complete", string(b))
		return o
	}
	if o := post("http://evil.example:3128/?code=c&state=" + u.Query().Get("state")); o["status"] != "error" || o["retry"] != true {
		t.Fatalf("foreign callback: %v", o)
	}
	if o := post("http://localhost:3128/?code=c&state=WRONG"); o["status"] != "error" || o["retry"] != true {
		t.Fatalf("wrong state: %v", o)
	}
	if exch != nil {
		t.Fatal("rejected callbacks must not reach the token endpoint")
	}

	o := post("http://localhost:3128/signin/callback?code=the-code&state=" + u.Query().Get("state"))
	if o["status"] != "ok" {
		t.Fatalf("complete: %v", o)
	}
	acc, _ := o["account"].(map[string]any)
	uid, _ := acc["uid"].(string)
	if acc["nickname"] != "web@example.com" || acc["provider"] != "Social" {
		t.Fatalf("account: %v", acc)
	}
	if exch["code"] != "the-code" || exch["code_verifier"] == "" {
		t.Fatalf("exchange: %v", exch)
	}
	if st, ok := pl.Status(uid); !ok || st.Platform != "kiro" || st.Credits != 48 {
		t.Fatalf("pool: ok=%v %+v", ok, st)
	}
	// 会话用完即删：再粘一次是「已过期」，且不会再换一次 token。
	if o := post("http://localhost:3128/?code=the-code&state=" + u.Query().Get("state")); o["status"] != "error" || o["retry"] != false {
		t.Fatalf("finished session: %v", o)
	}
}

// 网页登录（企业 SSO）：两段粘贴 + 多 profile 选择；只接受登录时列出的 profile。
func TestKiroPortalMicrosoftWithProfileSelection(t *testing.T) {
	rt := &kiroFakeRT{}
	access := pJWT(map[string]any{"iss": pIssuer, "oid": "oid-5", "email": "ms@corp.example"})
	rt.on("login.microsoftonline.com/"+pTenant+"/v2.0/.well-known/openid-configuration", func(*http.Request, []byte) (int, string) {
		b, _ := json.Marshal(map[string]any{"issuer": pIssuer,
			"authorization_endpoint": "https://login.microsoftonline.com/" + pTenant + "/oauth2/v2.0/authorize",
			"token_endpoint":         "https://login.microsoftonline.com/" + pTenant + "/oauth2/v2.0/token"})
		return 200, string(b)
	})
	rt.on("login.microsoftonline.com/"+pTenant+"/oauth2/v2.0/token", func(*http.Request, []byte) (int, string) {
		b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "rt-ms", "expires_in": 3600})
		return 200, string(b)
	})
	rt.on("codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles", func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("TokenType") != "EXTERNAL_IDP" {
			return 403, `{"message":"missing token type"}`
		}
		return 200, profilesJSON(pArnA, pArnB)
	})
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) { return 200, kiroUsageBody("", "", 1, 10) })
	p, pl, _ := newKiroPanel2(t, rt)

	_, out := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/start", `{}`)
	state, _ := out["state"].(string)
	loginURL, _ := out["url"].(string)
	portalState := mustParse(t, loginURL).Query().Get("state")
	post := func(cb string) map[string]any {
		b, _ := json.Marshal(map[string]string{"state": state, "callback_url": cb})
		_, o := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/complete", string(b))
		return o
	}

	desc := url.Values{"login_option": {"external_idp"}, "issuer_url": {pIssuer}, "client_id": {pAppID}, "state": {portalState},
		"scopes": {"api://" + pAppID + "/codewhisperer:conversations offline_access"}}
	o := post("http://localhost:3128/signin/callback?" + desc.Encode())
	if o["status"] != "next" || o["stage"] != "microsoft" {
		t.Fatalf("leg 1: %v", o)
	}
	authURL, _ := o["url"].(string)
	au := mustParse(t, authURL)
	if au.Host != "login.microsoftonline.com" || au.Query().Get("code_challenge") == "" {
		t.Fatalf("microsoft authorize url: %s", authURL)
	}

	o = post("http://localhost:3128/oauth/callback?code=ms-code&state=" + au.Query().Get("state"))
	if o["status"] != "select_profile" {
		t.Fatalf("leg 2: %v", o)
	}
	profiles, _ := o["profiles"].([]any)
	if len(profiles) != 2 {
		t.Fatalf("profiles: %v", o["profiles"])
	}
	if len(pl.List()) != 0 {
		t.Fatal("no account may be created before the user picks a profile")
	}
	sel := func(arn string) (int, map[string]any) {
		b, _ := json.Marshal(map[string]string{"state": state, "profile_arn": arn})
		return kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/select", string(b))
	}
	if code, _ := sel("arn:aws:codewhisperer:us-east-1:123456789012:profile/NOTOFFERED"); code != 400 {
		t.Fatalf("a profile that was not offered must be rejected, got %d", code)
	}
	if len(pl.List()) != 0 {
		t.Fatal("rejected selection must not create an account")
	}
	code, o := sel(pArnB)
	if code != 200 || o["status"] != "ok" {
		t.Fatalf("select: %d %v", code, o)
	}
	uid, _ := o["account"].(map[string]any)["uid"].(string)
	a := pl.AuthByUID(uid)
	if a == nil || a.KiroMeta().ProfileArn != pArnB || a.KiroMeta().AuthMethod != "external_idp" || a.Nickname != "ms@corp.example" {
		t.Fatalf("account: %+v", a)
	}
	// 选完即销毁：同一个 state 不能再选一次。
	if code, o := sel(pArnA); code != 200 || o["status"] != "error" {
		t.Fatalf("second select: %d %v", code, o)
	}
	if len(pl.List()) != 1 {
		t.Fatalf("pool size = %d", len(pl.List()))
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 取消会清掉暂存的凭证与会话；对不存在的会话幂等。
func TestKiroPortalCancel(t *testing.T) {
	p, _, _ := newKiroPanel2(t, &kiroFakeRT{})
	_, out := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/start", `{}`)
	state, _ := out["state"].(string)
	if p.krPortalGet(state) == nil {
		t.Fatal("session should be registered")
	}
	if code, o := kiroDo(t, p, "POST", "/panel/api/kiro/login/cancel", `{"state":"`+state+`"}`); code != 200 || o["cancelled"] != true {
		t.Fatalf("cancel: %d %v", code, o)
	}
	if p.krPortalGet(state) != nil {
		t.Fatal("cancelled session must be gone")
	}
	b, _ := json.Marshal(map[string]string{"state": state, "callback_url": "http://localhost:3128/?code=c&state=s"})
	if _, o := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/complete", string(b)); o["status"] != "error" || o["retry"] != false {
		t.Fatalf("complete after cancel: %v", o)
	}
	if code, o := kiroDo(t, p, "POST", "/panel/api/kiro/login/cancel", `{"state":"never-existed"}`); code != 200 || o["cancelled"] != false {
		t.Fatalf("cancel unknown: %d %v", code, o)
	}
	// 参数缺失 → 400。
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/login/portal/complete", `{"state":"x"}`); code != 400 {
		t.Fatalf("missing callback_url: %d", code)
	}
}

// SSO token 模式：逐个换取；失败的只报序号。
func TestKiroImportSSOTokenMode(t *testing.T) {
	const good = "GOOD-SSO-TOKEN"
	rt := &kiroFakeRT{}
	oidc := "oidc.us-east-1.amazonaws.com"
	rt.on(oidc+"/client/register", func(*http.Request, []byte) (int, string) { return 200, `{"clientId":"cid","clientSecret":"cs"}` })
	rt.on(oidc+"/device_authorization", func(*http.Request, []byte) (int, string) {
		return 200, `{"deviceCode":"dc","userCode":"UC","interval":1}`
	})
	rt.on("portal.sso.us-east-1.amazonaws.com/token/whoAmI", func(r *http.Request, _ []byte) (int, string) {
		if r.Header.Get("Authorization") != "Bearer "+good {
			return 401, `{}`
		}
		return 200, `{}`
	})
	rt.on("portal.sso.us-east-1.amazonaws.com/session/device", func(*http.Request, []byte) (int, string) { return 200, `{"token":"ds"}` })
	rt.on(oidc+"/device_authorization/accept_user_code", func(*http.Request, []byte) (int, string) { return 200, `{}` })
	rt.on(oidc+"/token", func(*http.Request, []byte) (int, string) {
		return 200, `{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`
	})
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		return 200, kiroUsageBody("user-sso", "sso@example.com", 1, 50)
	})
	p, pl, _ := newKiroPanel2(t, rt)

	body, _ := json.Marshal(map[string]string{"mode": "sso_token", "text": "# 注释\nBAD-TOKEN\n\n" + good + "\n"})
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", string(body))
	if code != 200 || out["imported"] != float64(1) {
		t.Fatalf("import: %d %v", code, out)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 || strings.Contains(failed[0].(string), "BAD-TOKEN") || !strings.Contains(failed[0].(string), "#1") {
		t.Fatalf("failure text must not echo the token: %v", failed)
	}
	if len(pl.List()) != 1 {
		t.Fatalf("pool size %d", len(pl.List()))
	}
	// 空输入 / 过多：400。
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"mode":"sso_token","text":"  \n# x"}`); code != 400 {
		t.Errorf("empty: %d", code)
	}
	many, _ := json.Marshal(map[string]string{"mode": "sso_token", "text": strings.Repeat("t\n", kiroSSOTokenMax+1)})
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/import", string(many)); code != 400 {
		t.Errorf("too many: %d", code)
	}
}

// 本机缓存导入：读 LocalCacheDir 下的文件；没有缓存时给 400 和原因。
func TestKiroImportLocalEndpoint(t *testing.T) {
	rt := &kiroFakeRT{}
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		return 200, kiroUsageBody("user-local", "local@example.com", 4, 50)
	})
	p, pl, svc := newKiroPanel2(t, rt)

	if code, o := kiroDo(t, p, "POST", "/panel/api/kiro/import/local", `{}`); code != 400 || !strings.Contains(o["error"].(string), "kiro-auth-token.json") {
		t.Fatalf("no cache: %d %v", code, o)
	}
	b, _ := json.Marshal(map[string]any{"accessToken": "at-l", "refreshToken": "rt-l", "authMethod": "social", "provider": "Google",
		"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(svc.LocalCacheDir, "kiro-auth-token.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, o := kiroDo(t, p, "POST", "/panel/api/kiro/import/local", `{}`)
	if code != 200 || o["imported"] != float64(1) || len(pl.List()) != 1 {
		t.Fatalf("import local: %d %v", code, o)
	}
}

// 超额开关：读 / 设；设置必须显式带 enabled；打开后因「额度用完」冷却的账号立即解冻。
func TestKiroOverageEndpoints(t *testing.T) {
	rt := &kiroFakeRT{}
	status := "DISABLED"
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		b, _ := json.Marshal(map[string]any{
			"overageConfiguration": map[string]any{"overageStatus": status},
			"subscriptionInfo":     map[string]any{"subscriptionTitle": "KIRO PRO", "subscriptionType": "Q_DEVELOPER_STANDALONE_PRO", "overageCapability": "OVERAGE_CAPABLE"},
			"userInfo":             map[string]any{"email": "o@example.com", "userId": "user-over"},
			"usageBreakdownList": []any{map[string]any{"resourceType": "AGENTIC_REQUEST", "currentUsage": 50, "usageLimit": 50,
				"overageCap": 100.0, "overageRate": 0.04, "currentOverages": 0}},
		})
		return 200, string(b)
	})
	rt.on("q.us-east-1.amazonaws.com/setUserPreference", func(_ *http.Request, body []byte) (int, string) {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if oc, _ := m["overageConfiguration"].(map[string]any); oc != nil {
			status, _ = oc["overageStatus"].(string)
		}
		return 200, `{}`
	})
	p, pl, _ := newKiroPanel2(t, rt)

	// 导入一个额度已用完的号 → 硬冷却。
	_, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"ksk_over"}`)
	uid := out["accounts"].([]any)[0].(map[string]any)["uid"].(string)
	if st, _ := pl.Status(uid); !st.Cooling || st.CoolKind != "hard_credit" {
		t.Fatalf("exhausted account must start cooled: %+v", st)
	}

	code, o := kiroDo(t, p, "GET", "/panel/api/kiro/accounts/"+uid+"/overage", "")
	if code != 200 || o["enabled"] != false || o["capable"] != true || o["cap_usd"] != float64(100) {
		t.Fatalf("get: %d %v", code, o)
	}
	// 不带 enabled → 400，什么都不改（打开超额可能产生费用，必须显式）。
	for _, body := range []string{`{}`, `{"enabled":"yes"}`, ``} {
		if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/overage", body); code != 400 {
			t.Errorf("body %q: want 400, got %d", body, code)
		}
	}
	if status != "DISABLED" {
		t.Fatal("rejected requests must not change the upstream preference")
	}

	code, o = kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/overage", `{"enabled":true}`)
	if code != 200 || o["enabled"] != true {
		t.Fatalf("enable: %d %v", code, o)
	}
	if st, _ := pl.Status(uid); st.Cooling {
		t.Fatalf("enabling overage must free an account that was cooled for exhausted credits: %+v", st)
	}
	// 关闭：额度仍是 0 → 重新进入冷却。
	code, o = kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/overage", `{"enabled":false}`)
	if code != 200 || o["enabled"] != false {
		t.Fatalf("disable: %d %v", code, o)
	}
	if st, _ := pl.Status(uid); !st.Cooling {
		t.Fatalf("disabling overage on an exhausted account must cool it again: %+v", st)
	}
	// 不存在 / 不是 Kiro 账号 → 404。
	if code, _ := kiroDo(t, p, "GET", "/panel/api/kiro/accounts/nope/overage", ""); code != 404 {
		t.Errorf("unknown account: %d", code)
	}
}

// 未装配 Kiro：新增的接口一律 501。
func TestKiroPhase2Disabled(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/panel/api/kiro/login/portal/start", `{}`},
		{"POST", "/panel/api/kiro/login/portal/complete", `{"state":"s","callback_url":"u"}`},
		{"POST", "/panel/api/kiro/login/portal/select", `{"state":"s","profile_arn":"a"}`},
		{"POST", "/panel/api/kiro/import/local", `{}`},
		{"GET", "/panel/api/kiro/accounts/u/overage", ""},
		{"POST", "/panel/api/kiro/accounts/u/overage", `{"enabled":true}`},
	} {
		if code, _ := kiroDo(t, p, c.method, c.path, c.body); code != http.StatusNotImplemented {
			t.Errorf("%s %s: code=%d want 501", c.method, c.path, code)
		}
	}
}
