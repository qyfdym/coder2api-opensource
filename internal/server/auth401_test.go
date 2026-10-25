package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 聊天拿到网关层 401 页（access token 被拒）：下一次选中这个号时先刷新 token；refresh 返回 12153
// 就禁用。此前 60 天有效期内永远不刷新，号会一直被选中、一直 401，状态却显示正常。
func TestChat401ForcesRefreshThenDisablesDeadSession(t *testing.T) {
	var refreshCalls, chatCalls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp := func(status int, ct, body string) *http.Response {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}
			}
			if strings.HasSuffix(r.URL.Path, "/token/refresh") {
				refreshCalls.Add(1)
				return resp(401, "application/json", `{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: Session doesn't have required client"}`), nil
			}
			chatCalls.Add(1)
			return resp(401, "text/html", "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n<body>openresty</body></html>"), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	a := &auth.Auth{UID: "u1", AccessToken: "revoked", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	post := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		return rec.Code
	}

	if code := post(); code < 400 {
		t.Fatalf("first request: code=%d, want an error", code)
	}
	if refreshCalls.Load() != 0 || chatCalls.Load() != 1 {
		t.Fatalf("first request: refresh=%d chat=%d", refreshCalls.Load(), chatCalls.Load())
	}
	if !a.NeedsRefresh(0) {
		t.Fatal("a 401 must mark the token for refresh")
	}

	post()
	if refreshCalls.Load() != 1 {
		t.Errorf("second request must refresh first (refresh calls = %d)", refreshCalls.Load())
	}
	if chatCalls.Load() != 1 {
		t.Errorf("a dead session must not be sent to chat again (chat calls = %d)", chatCalls.Load())
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Errorf("refresh 12153 after a 401 must disable the account: %+v", st)
	}
}
