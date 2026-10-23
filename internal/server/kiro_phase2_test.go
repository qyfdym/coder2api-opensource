package server

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

const (
	msTenant = "11111111-2222-3333-4444-555555555555"
	msAppID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	msIssuer = "https://login.microsoftonline.com/" + msTenant + "/v2.0"
	msScopes = "api://" + msAppID + "/codewhisperer:conversations api://" + msAppID + "/codewhisperer:completions offline_access"
)

// msAcct 企业 SSO 账号。expired=true → access token 已过期，选中时必须先刷新。
func msAcct(t *testing.T, uid, tokenEndpoint string, expired bool) *auth.Auth {
	t.Helper()
	exp := int64(9999999999)
	if expired {
		exp = 1
	}
	a := auth.NewKiro(uid, uid, "at-old", "rt-old", exp, auth.KiroMeta{
		AuthMethod: "external_idp", Provider: "AzureAD", Region: "us-east-1", ProfileArn: kiroTestArn, MachineID: "m-" + uid,
		ClientID: msAppID, TokenEndpoint: tokenEndpoint, IssuerURL: msIssuer, Scopes: msScopes,
	})
	a.FilePath = filepath.Join(t.TempDir(), auth.KiroFileName(uid))
	return a
}

// msUpstream 同时扮演微软租户端点和 Kiro 推理入口，记录每个出站请求。
type msUpstream struct {
	mu      sync.Mutex
	reqs    []string // host+path
	refresh []url.Values
	gen     []http.Header
}

func (m *msUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	resp := func(status int, b []byte) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}, Request: r}
	}
	route := r.URL.Host + r.URL.Path
	m.mu.Lock()
	m.reqs = append(m.reqs, route)
	m.mu.Unlock()
	switch route {
	case "login.microsoftonline.com/" + msTenant + "/oauth2/v2.0/token":
		form, _ := url.ParseQuery(string(body))
		m.mu.Lock()
		m.refresh = append(m.refresh, form)
		m.mu.Unlock()
		return resp(200, []byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`)), nil
	case "q.us-east-1.amazonaws.com/generateAssistantResponse":
		m.mu.Lock()
		m.gen = append(m.gen, r.Header.Clone())
		m.mu.Unlock()
		return resp(200, bytes.Join([][]byte{
			kiroESFrame("assistantResponseEvent", map[string]any{"content": "hello from ms"}),
			kiroESFrame("metadataEvent", map[string]any{"stopReason": "end_turn"}),
		}, nil)), nil
	}
	return resp(404, []byte(`{"message":"no route"}`)), nil
}

func (m *msUpstream) hit(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.reqs {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

// 企业 SSO 账号 token 过期：先走租户端点（表单、公共客户端）刷新，再带新 token 与
// TokenType: EXTERNAL_IDP 请求推理；轮换后的 refresh token 写回账号。
func TestKiroExternalIdpChatRefreshesViaTenantEndpoint(t *testing.T) {
	up := &msUpstream{}
	a := msAcct(t, "ms1", "https://login.microsoftonline.com/"+msTenant+"/oauth2/v2.0/token", true)
	h, _, _ := newKiroHandler(t, nil, up, nil, a)

	rec := postChat(h, "kiro:claude-sonnet-4.5", false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello from ms") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(up.refresh) != 1 {
		t.Fatalf("tenant endpoint called %d times, want 1", len(up.refresh))
	}
	f := up.refresh[0]
	if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "rt-old" || f.Get("client_id") != msAppID ||
		f.Get("scope") == "" || f.Get("client_secret") != "" {
		t.Fatalf("refresh form: %v", f)
	}
	if len(up.gen) != 1 || up.gen[0].Get("Authorization") != "Bearer at-new" || up.gen[0].Get("TokenType") != "EXTERNAL_IDP" {
		t.Fatalf("inference request headers: %v", up.gen)
	}
	if a.RefreshTokenValue() != "rt-new" || a.AccessTokenValue() != "at-new" {
		t.Fatalf("rotated tokens not stored: rt=%q at=%q", a.RefreshTokenValue(), a.AccessTokenValue())
	}
	// 第二次请求：token 仍新鲜，不再刷新。
	if rec = postChat(h, "kiro:claude-sonnet-4.5", false); rec.Code != 200 || len(up.refresh) != 1 {
		t.Fatalf("second call: status=%d refreshes=%d", rec.Code, len(up.refresh))
	}
}

// 账号文件里的 tokenEndpoint 被改到白名单之外：聊天路径上也不会把 refresh token 发出去，
// 账号按「需要重新登录」停用，不影响同池其它账号。
func TestKiroExternalIdpTamperedEndpointNeverContacted(t *testing.T) {
	up := &msUpstream{}
	bad := msAcct(t, "ms-bad", "https://evil.example/"+msTenant+"/oauth2/v2.0/token", true)
	good := kiroAcct("k-good")
	h, p, _ := newKiroHandler(t, nil, up, nil, bad, good)
	p.SetCredits("ms-bad", 2000, 0) // 确定性源 → 先选被篡改的号

	rec := postChat(h, "kiro:claude-sonnet-4.5", false)
	if rec.Code != 200 {
		t.Fatalf("pool should fall through to the healthy account: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if up.hit("evil.example") != 0 {
		t.Fatalf("a refresh token was sent outside the allow-list: %v", up.reqs)
	}
	if up.hit("login.microsoftonline.com") != 0 {
		t.Fatalf("no tenant call is valid for a tampered config: %v", up.reqs)
	}
	if st, _ := p.Status("ms-bad"); !st.Disabled {
		t.Fatalf("tampered account must be taken out of rotation: %+v", st)
	}
	if st, _ := p.Status("k-good"); st.SuccessCount != 1 {
		t.Fatalf("healthy account should have served the request: %+v", st)
	}
}

// 回归：Kiro 客户端的 external_idp 判定与账号元数据一致（大小写不敏感）。
func TestKiroCredExternalIdpFlag(t *testing.T) {
	for _, m := range []string{"external_idp", "EXTERNAL_IDP", " External_Idp "} {
		if !(kiro.Cred{AuthMethod: m}).IsExternalIDP() {
			t.Errorf("%q must be recognised as external idp", m)
		}
	}
	for _, m := range []string{"", "idc", "social", "api_key", "external"} {
		if (kiro.Cred{AuthMethod: m}).IsExternalIDP() {
			t.Errorf("%q must not be recognised as external idp", m)
		}
	}
}
