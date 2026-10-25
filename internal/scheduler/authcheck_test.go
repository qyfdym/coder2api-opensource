package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// apisix401 真机的网关层 401 页（access token 被拒，没有业务信封）。
const apisix401 = "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n<body>\r\n" +
	"<center><h1>401 Authorization Required</h1></center>\r\n<hr><center>openresty</center>\r\n" +
	"<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n</html>\r\n"

// 真机的 refresh 失败原文（登录会话已被上游作废）。
const refresh12153 = `{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: Session doesn't have required client","requestId":"r1"}`

// authUpstream access token 为 good 时 billing 正常，其余一律 401 页；refresh 按 refreshOK 决定成败。
type authUpstream struct {
	refreshStatus int    // 0 = 成功（换成 good）
	refreshBody   string // refreshStatus != 0 时的响应体
	refreshCalls  atomic.Int32
	billingCalls  atomic.Int32
	checkinCalls  atomic.Int32
}

func (f *authUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			f.refreshCalls.Add(1)
			if f.refreshStatus != 0 {
				w.WriteHeader(f.refreshStatus)
				w.Write([]byte(f.refreshBody))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"accessToken":"good","refreshToken":"rt2","expiresIn":5184000}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			f.checkinCalls.Add(1)
		} else {
			f.billingCalls.Add(1)
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(401)
			w.Write([]byte(apisix401))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func authScheduler(t *testing.T, f *authUpstream) (*Scheduler, *pool.Pool, *auth.Auth) {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	// access token 离过期还很远（60 天有效期内）：后台任务不会主动刷新它。
	a := &auth.Auth{UID: "u1", Nickname: "135****0770", AccessToken: "revoked", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p, a
}

// 余额刷新拿到 401 → 刷新 token 返回 12153：登录确实死了，立即禁用并写明要重新登录；下一轮不再打这个号。
func TestBalance401WithDeadSessionDisables(t *testing.T) {
	f := &authUpstream{refreshStatus: 401, refreshBody: refresh12153}
	s, p, _ := authScheduler(t, f)

	s.RunBalanceRefreshNow()
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != loginDeadReason {
		t.Fatalf("account must be disabled as login-dead: %+v", st)
	}
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("refresh calls = %d, want 1", n)
	}

	before := f.billingCalls.Load()
	s.RunBalanceRefreshNow()
	if f.billingCalls.Load() != before || f.refreshCalls.Load() != 1 {
		t.Errorf("a login-dead account must not be polled again (billing %d→%d, refresh %d)",
			before, f.billingCalls.Load(), f.refreshCalls.Load())
	}

	// 即使打开了「禁用账号也执行保号任务」，登录已失效的号也不再刷余额。
	s.SetIncludeDisabledInTasks(true)
	s.RunBalanceRefreshNow()
	if f.billingCalls.Load() != before {
		t.Errorf("include_disabled_in_tasks must not poll a login-dead account")
	}
}

// access token 被单独作废但 refresh token 还活着：刷新成功 → 重试拿到余额，账号保持可用。
func TestBalance401RecoversByRefresh(t *testing.T) {
	f := &authUpstream{}
	s, p, a := authScheduler(t, f)

	s.RunBalanceRefreshNow()
	st, _ := p.Status("u1")
	if st.Disabled || st.Credits != 700 {
		t.Fatalf("account must recover and report credits: %+v", st)
	}
	if a.AccessTokenValue() != "good" || f.refreshCalls.Load() != 1 {
		t.Errorf("token=%q refresh calls=%d", a.AccessTokenValue(), f.refreshCalls.Load())
	}
}

// 刷新验证本身失败（上游 5xx / 网络）：不处置；30 分钟内不再重复刷新（余额每 5 分钟一轮）。
func TestBalance401TransientRefreshFailureIsThrottled(t *testing.T) {
	f := &authUpstream{refreshStatus: 502, refreshBody: `bad gateway`}
	s, p, _ := authScheduler(t, f)

	s.RunBalanceRefreshNow()
	s.RunBalanceRefreshNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("a failed verification must not disable the account: %+v", st)
	}
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("refresh calls = %d, want 1 (throttled)", n)
	}
}

// 签到拿到 401 → 判定登录失效并禁用；同一轮的余额查询不再发（只会再拿一次 401）。
func TestCheckin401WithDeadSessionDisables(t *testing.T) {
	f := &authUpstream{refreshStatus: 401, refreshBody: refresh12153}
	s, p, _ := authScheduler(t, f)

	s.RunCheckinNow()
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != loginDeadReason {
		t.Fatalf("account must be disabled as login-dead: %+v", st)
	}
	if f.checkinCalls.Load() != 1 || f.billingCalls.Load() != 0 {
		t.Errorf("checkin calls=%d billing calls=%d, want 1 and 0", f.checkinCalls.Load(), f.billingCalls.Load())
	}
}

// 签到 401 + 刷新成功：重试签到成功，余额照常更新。
func TestCheckin401RecoversByRefresh(t *testing.T) {
	f := &authUpstream{}
	s, p, _ := authScheduler(t, f)

	s.RunCheckinNow()
	st, _ := p.Status("u1")
	if st.Disabled || st.Credits != 700 {
		t.Fatalf("account must recover: %+v", st)
	}
	if f.checkinCalls.Load() != 2 || f.refreshCalls.Load() != 1 {
		t.Errorf("checkin calls=%d (want 2: original + retry) refresh calls=%d", f.checkinCalls.Load(), f.refreshCalls.Load())
	}
}

func TestLoginDeadReasons(t *testing.T) {
	for _, r := range []string{loginDeadReason, "12153 session dead", "refresh session dead"} {
		if !loginDead(r) {
			t.Errorf("%q should count as login-dead", r)
		}
	}
	for _, r := range []string{"", "余额不足", "account restricted by upstream (11140 request illegal)", "manual"} {
		if loginDead(r) {
			t.Errorf("%q must not count as login-dead", r)
		}
	}
}
