package qoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// acctTokenServer 模拟三个 token 端点；responders 按路径给出 (status, body)。
type acctTokenServer struct {
	mu         sync.Mutex
	calls      []string
	bodies     map[string]map[string]string
	responders map[string]func(body map[string]string) (int, string)
}

func (s *acctTokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls = append(s.calls, r.URL.Path)
	if s.bodies == nil {
		s.bodies = map[string]map[string]string{}
	}
	s.bodies[r.URL.Path] = body
	fn := s.responders[r.URL.Path]
	s.mu.Unlock()
	if r.Header.Get("User-Agent") == "" || r.Header.Get("Authorization") != "" {
		w.WriteHeader(400) // token 端点只带区域 UA、不带 Bearer
		return
	}
	if fn == nil {
		w.WriteHeader(404)
		return
	}
	st, b := fn(body)
	w.WriteHeader(st)
	_, _ = io.WriteString(w, b)
}

func acctOK200(b string) func(map[string]string) (int, string) {
	return func(map[string]string) (int, string) { return 200, b }
}

func acctFail(st int, b string) func(map[string]string) (int, string) {
	return func(map[string]string) (int, string) { return st, b }
}

func TestRefreshRouting(t *testing.T) {
	acctFastRetry(t)
	ctx := context.Background()
	type tc struct {
		name      string
		cred      Cred
		resp      map[string]func(map[string]string) (int, string)
		wantCalls []string
		wantTok   string
		wantRT    string
		wantKind  ErrKind
		wantCode  string
	}
	cases := []tc{
		{
			name: "drt 走 deviceToken/refresh，refresh token 轮换",
			cred: Cred{UID: "u", RefreshToken: "drt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathDeviceRefresh: acctOK200(`{"token":"dt-new","refresh_token":"drt-new","expires_in":2592000000}`),
			},
			wantCalls: []string{oaPathDeviceRefresh}, wantTok: "dt-new", wantRT: "drt-new",
		},
		{
			name: "drt 会话死亡：不回落 PAT",
			cred: Cred{UID: "u", RefreshToken: "drt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathDeviceRefresh: acctFail(401, `{"code":"TOKEN_EXPIRE","message":"Offline user session not found"}`),
				oaPathJobExchange:   acctOK200(`{"token":"jt-should-not"}`),
			},
			wantCalls: []string{oaPathDeviceRefresh}, wantKind: KindSessionDead,
		},
		{
			name: "drt 响应缺 refresh_token 时沿用旧值；device_token 字段也认",
			cred: Cred{UID: "u", RefreshToken: "drt-old"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathDeviceRefresh: acctOK200(`{"device_token":"dt-2"}`),
			},
			wantCalls: []string{oaPathDeviceRefresh}, wantTok: "dt-2", wantRT: "drt-old",
		},
		{
			name: "jrt 走 jobToken/refresh",
			cred: Cred{UID: "u", RefreshToken: "jrt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobRefresh: acctOK200(`{"token":"jt-new","refresh_token":"jrt-new","expires_in":86400000}`),
			},
			wantCalls: []string{oaPathJobRefresh}, wantTok: "jt-new", wantRT: "jrt-new",
		},
		{
			name: "jrt 失败回落 PAT 重新交换",
			cred: Cred{UID: "u", RefreshToken: "jrt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobRefresh:  acctFail(401, `{"code":"INVALID_REFRESH_TOKEN"}`),
				oaPathJobExchange: acctOK200(`{"token":"jt-pat","refresh_token":"jrt-pat"}`),
			},
			wantCalls: []string{oaPathJobRefresh, oaPathJobExchange}, wantTok: "jt-pat", wantRT: "jrt-pat",
		},
		{
			name: "只有 PAT",
			cred: Cred{UID: "u", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobExchange: acctOK200(`{"token":"jt-pat"}`),
			},
			wantCalls: []string{oaPathJobExchange}, wantTok: "jt-pat", wantRT: "",
		},
		{
			name: "jrt 会话死亡 + PAT 鉴权失败 → 会话死亡",
			cred: Cred{UID: "u", RefreshToken: "jrt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobRefresh:  acctFail(403, `{"code":12153}`),
				oaPathJobExchange: acctFail(401, `{"code":"INVALID_PAT"}`),
			},
			wantCalls: []string{oaPathJobRefresh, oaPathJobExchange}, wantKind: KindSessionDead,
		},
		{
			name: "jrt 会话死亡 + PAT 暂时失败（5xx）→ 返回暂时性错误，别把可救的账号判死",
			cred: Cred{UID: "u", RefreshToken: "jrt-old", PAT: "pt-x"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobRefresh:  acctFail(401, `TOKEN_EXPIRE`),
				oaPathJobExchange: acctFail(503, `busy`),
			},
			wantCalls: []string{oaPathJobRefresh, oaPathJobExchange, oaPathJobExchange, oaPathJobExchange}, wantKind: KindServer,
		},
		{
			name: "2xx 但没给 token，体里带会话死亡标记",
			cred: Cred{UID: "u", RefreshToken: "jrt-old"},
			resp: map[string]func(map[string]string) (int, string){
				oaPathJobRefresh: acctOK200(`{"success":false,"code":"TOKEN_EXPIRE"}`),
			},
			wantCalls: []string{oaPathJobRefresh}, wantKind: KindSessionDead,
		},
		{
			name: "什么都没有",
			cred: Cred{UID: "u"}, wantKind: KindBadRequest, wantCode: "NO_REFRESH_TOKEN",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := &acctTokenServer{responders: c.resp}
			cl := acctNewClient(t, srv)
			ts, err := cl.Refresh(ctx, c.cred)
			if len(srv.calls) != len(c.wantCalls) {
				t.Fatalf("调用序列 %v want %v", srv.calls, c.wantCalls)
			}
			for i := range c.wantCalls {
				if srv.calls[i] != c.wantCalls[i] {
					t.Fatalf("调用序列 %v want %v", srv.calls, c.wantCalls)
				}
			}
			if c.wantKind != KindNone {
				if err == nil || err.Kind != c.wantKind {
					t.Fatalf("err=%v want kind %v", err, c.wantKind)
				}
				if c.wantCode != "" && err.Code != c.wantCode {
					t.Fatalf("code=%q want %q", err.Code, c.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if ts.AccessToken != c.wantTok || ts.RefreshToken != c.wantRT || ts.ExpiresAt <= time.Now().Unix() {
				t.Fatalf("ts=%+v", ts)
			}
		})
	}
}

func TestRefreshPayloadAndExpiry(t *testing.T) {
	srv := &acctTokenServer{responders: map[string]func(map[string]string) (int, string){
		oaPathJobRefresh: acctOK200(`{"token":"jt-new"}`),
	}}
	cl := acctNewClient(t, srv)
	ts, err := cl.Refresh(context.Background(), Cred{UID: "u", Realm: "intl", RefreshToken: "jrt-x"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.bodies[oaPathJobRefresh]["refresh_token"] != "jrt-x" {
		t.Errorf("payload=%v", srv.bodies[oaPathJobRefresh])
	}
	// 缺 expires_in：按 jt- 标称 24 小时（原实现沿用旧值，见 refresh.go 注释）。
	if d := ts.ExpiresAt - time.Now().Unix(); d < 86400-5 || d > 86400+5 {
		t.Errorf("缺省寿命 %d", d)
	}
	if ts.RefreshToken != "jrt-x" {
		t.Errorf("缺 refresh_token 时应沿用旧值：%q", ts.RefreshToken)
	}
}

func TestTokenFamily(t *testing.T) {
	cases := map[string]Cred{
		"device":  {RefreshToken: "drt-1"},
		"job":     {RefreshToken: "jrt-1"},
		"pat":     {AccessToken: "pt-1"},
		"unknown": {AccessToken: "dt-1"},
	}
	for want, c := range cases {
		if got := TokenFamily(c); got != want {
			t.Errorf("TokenFamily(%+v)=%s want %s", c, got, want)
		}
	}
}
