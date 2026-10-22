package qoder

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPKCEMatchesPython 固定随机字节下的 verifier / challenge 与原实现 _make_pkce 逐字符一致。
func TestPKCEMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	for _, c := range v.PKCE {
		raw, err := hex.DecodeString(c[0])
		if err != nil {
			t.Fatal(err)
		}
		ver, ch := pkceFromRandom(raw)
		if ver != c[1] || ch != c[2] {
			t.Errorf("pkce(%s…)\n got %s %s\nwant %s %s", c[0][:8], ver, ch, c[1], c[2])
		}
		if len(ver) != 64 || strings.ContainsAny(ch, "=+/") {
			t.Errorf("形态不对：verifier=%d challenge=%q", len(ver), ch)
		}
	}
}

// TestAuthURLMatchesPython 两区授权链接（参数顺序、nonce 形态、redirect_uri 有无）与原实现一致。
func TestAuthURLMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	if len(v.AuthURL) != 2 {
		t.Fatalf("向量数 %d", len(v.AuthURL))
	}
	for _, c := range v.AuthURL {
		raw, _ := hex.DecodeString(c.RawHex)
		now := time.Unix(1790000000, 123)
		s := buildLoginSession(c.Realm, raw, c.UUID, c.MachineID, now)
		if s.AuthURL != c.AuthURL {
			t.Errorf("%s 授权链接\n got %s\nwant %s", c.Realm, s.AuthURL, c.AuthURL)
		}
		if s.Verifier != c.Verifier || s.Nonce != c.Nonce {
			t.Errorf("%s verifier/nonce 不一致：%q %q", c.Realm, s.Nonce, c.Nonce)
		}
		if s.Realm != c.Realm || !s.ExpiresAt.Equal(now.Add(LoginTTL)) || s.State != "qd-1790000000000000123" {
			t.Errorf("%s 会话字段：%+v", c.Realm, s)
		}
	}
	cn := buildLoginSession("cn", make([]byte, 64), RandomUUID(), "m", time.Now())
	intl := buildLoginSession("intl", make([]byte, 64), RandomUUID(), "m", time.Now())
	if !strings.Contains(cn.AuthURL, "redirect_uri=qoder-work-cn%3A%2F%2F") || strings.Contains(intl.AuthURL, "redirect_uri") {
		t.Error("redirect_uri：国内版要带、国际版不带")
	}
	if len(cn.Nonce) != 36 || len(intl.Nonce) != 32 {
		t.Errorf("nonce 形态：cn=%q intl=%q", cn.Nonce, intl.Nonce)
	}
	if !strings.HasPrefix(intl.AuthURL, "https://qoder.com/device/selectAccounts?") ||
		!strings.HasPrefix(cn.AuthURL, "https://qoder.com.cn/device/selectAccounts?") {
		t.Error("授权站点不对")
	}
}

func TestStartLoginRandomized(t *testing.T) {
	c := NewClient(Options{Logf: func(string, ...any) {}})
	a, err := c.StartLogin("intl")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.StartLogin("intl")
	if a.Verifier == b.Verifier || a.Nonce == b.Nonce || a.MachineID == "" || a.Realm != RealmIntl {
		t.Errorf("每次会话的随机量必须独立：%+v %+v", a, b)
	}
	if time.Until(a.ExpiresAt) > LoginTTL || time.Until(a.ExpiresAt) < LoginTTL-time.Minute {
		t.Errorf("ExpiresAt=%v", a.ExpiresAt)
	}
}

func TestPollLoginStates(t *testing.T) {
	var mode string
	var gotQuery url.Values
	var gotUA, uiAuth, uiUA string
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case oaPathDevicePoll:
			gotQuery, gotUA = r.URL.Query(), r.Header.Get("User-Agent")
			if !strings.HasPrefix(r.URL.RawQuery, "nonce=") || !strings.HasSuffix(r.URL.RawQuery, "&challenge_method=S256") {
				t.Errorf("轮询参数顺序不对：%s", r.URL.RawQuery)
			}
			switch mode {
			case "404":
				w.WriteHeader(404)
			case "202":
				w.WriteHeader(202)
			case "empty":
				_, _ = io.WriteString(w, `{}`)
			case "notjson":
				_, _ = io.WriteString(w, `<html>`)
			case "400":
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"message":"bad verifier"}`)
			case "ok", "ok-noui":
				_, _ = io.WriteString(w, `{"token":"dt-new","refresh_token":"drt-new","user_id":"uid-123456789","expires_in":2592000000}`)
			}
		case oaPathUserInfo:
			uiAuth, uiUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
			if mode == "ok-noui" {
				w.WriteHeader(500)
				return
			}
			_, _ = io.WriteString(w, `{"id":"uid-123456789","name":"Alice","email":"a@example.com","user_type":"team_member","organization_id":"org1","organization_name":"Org"}`)
		}
	}))
	s := buildLoginSession("cn", make([]byte, 64), RandomUUID(), "mid", time.Now())
	ctx := context.Background()
	want := map[string]string{"404": "pending", "202": "pending", "empty": "pending", "notjson": "pending", "400": "error"}
	for m, st := range want {
		mode = m
		res, err := c.PollLogin(ctx, s)
		if err != nil || res.Status != st {
			t.Errorf("mode=%s status=%q msg=%q err=%v", m, res.Status, res.Message, err)
		}
	}
	if gotQuery.Get("verifier") != s.Verifier || gotQuery.Get("nonce") != s.Nonce || gotUA != pkcePollUA {
		t.Errorf("轮询参数 / UA 不对：%v ua=%q", gotQuery, gotUA)
	}

	mode = "ok"
	res, err := c.PollLogin(ctx, s)
	if err != nil || res.Status != "ok" {
		t.Fatalf("ok: %+v %v", res, err)
	}
	if res.Tokens.AccessToken != "dt-new" || res.Tokens.RefreshToken != "drt-new" {
		t.Errorf("tokens 不对")
	}
	if d := res.Tokens.ExpiresAt - time.Now().Unix(); d < 30*86400-5 || d > 30*86400+5 {
		t.Errorf("expires_in(ms) 换算不对：%d", d)
	}
	u := res.User
	if u.UID != "uid-123456789" || u.Nickname != "Alice" || u.Email != "a@example.com" || u.UserType != "team_member" ||
		u.OrgID != "org1" || u.OrgName != "Org" || u.Domain != "qoder.com.cn" || u.Realm != RealmCN {
		t.Errorf("userinfo 映射不对：%+v", u)
	}
	if uiAuth != "Bearer dt-new" || uiUA != ConfigFor(RealmCN).UA {
		t.Errorf("userinfo 头不对：ua=%q", uiUA)
	}

	mode = "ok-noui"
	res, _ = c.PollLogin(ctx, s)
	if res.Status != "ok" || res.User.UID != "uid-123456789" || res.User.Nickname != "u23456789" ||
		res.User.UserType != DefaultUserType || !strings.Contains(res.Message, "userinfo") {
		t.Errorf("userinfo 失败时的缺省值不对：%+v", res)
	}

	expired := *s
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if res, _ := c.PollLogin(ctx, &expired); res.Status != "expired" {
		t.Errorf("过期会话应 expired：%+v", res)
	}
	if _, err := c.PollLogin(ctx, nil); err == nil {
		t.Error("nil 会话应返回 error")
	}
}

func TestPollLoginTransportIsPending(t *testing.T) {
	c := NewClient(Options{Logf: func(string, ...any) {}})
	c.httpc = &http.Client{Transport: acctRewrite{target: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, base: http.DefaultTransport},
		Timeout: time.Second}
	s := buildLoginSession("cn", make([]byte, 64), RandomUUID(), "mid", time.Now())
	res, err := c.PollLogin(context.Background(), s)
	if err != nil || res.Status != "pending" || !strings.HasPrefix(res.Message, "poll error: ") {
		t.Errorf("网络错误应按 pending 继续轮询：%+v %v", res, err)
	}
}

func TestExchangePAT(t *testing.T) {
	var body map[string]string
	var ua string
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oaPathJobExchange || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		ua = r.Header.Get("User-Agent")
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["personal_token"] == "pt-empty" {
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"token":"jt-1","refresh_token":"jrt-1","expires_in":86400000}`)
	}))
	if _, err := c.ExchangePAT(context.Background(), "cn", "dt-nope"); err == nil || err.Kind != KindBadRequest {
		t.Errorf("非 pt- 应拒绝：%v", err)
	}
	ts, err := c.ExchangePAT(context.Background(), "intl", " pt-abc ")
	if err != nil || ts.AccessToken != "jt-1" || ts.RefreshToken != "jrt-1" {
		t.Fatalf("exchange: %+v %v", ts, err)
	}
	if body["personal_token"] != "pt-abc" || ua != ConfigFor(RealmIntl).UA {
		t.Errorf("请求体 / UA 不对：%v %q", body, ua)
	}
	if d := ts.ExpiresAt - time.Now().Unix(); d < 86400-5 || d > 86400+5 {
		t.Errorf("expires_in 换算：%d", d)
	}
	if _, err := c.ExchangePAT(context.Background(), "cn", "pt-empty"); err == nil || err.Code != "NO_TOKEN" {
		t.Errorf("无 token 应报 NO_TOKEN：%v", err)
	}
}

func TestDeviceExpiry(t *testing.T) {
	now := time.Unix(1790000000, 0)
	if got := deviceExpiry(map[string]any{"expires_in": json.Number("3600000")}, now); got != 1790003600 {
		t.Errorf("expires_in: %d", got)
	}
	if got := deviceExpiry(map[string]any{"expires_at": "2026-12-01T00:00:00Z"}, now); got != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("expires_at RFC3339: %d", got)
	}
	if got := deviceExpiry(map[string]any{"expires_at": "2026-12-01T00:00:00.123"}, now); got != time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local).Unix() {
		t.Errorf("expires_at 无时区: %d", got)
	}
	if got := deviceExpiry(map[string]any{}, now); got != now.Unix()+30*86400 {
		t.Errorf("缺省 30 天: %d", got)
	}
}
