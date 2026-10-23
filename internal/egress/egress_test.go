package egress

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestProxyFuncFollowsResolver(t *testing.T) {
	t.Cleanup(func() { Set(nil) })
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	wb, qd := ProxyFunc(WorkBuddy), ProxyFunc(Qoder)

	if u, err := wb(req); u != nil || err != nil {
		t.Fatalf("no resolver: %v %v", u, err)
	}

	proxy, _ := url.Parse("http://127.0.0.1:17890")
	Set(func(platform string, _ *http.Request) (*url.URL, error) {
		switch platform {
		case WorkBuddy:
			return proxy, nil
		case Qoder:
			return nil, errors.New("代理池里没有可用的代理")
		}
		return nil, nil
	})
	if u, err := wb(req); err != nil || u.String() != proxy.String() {
		t.Errorf("workbuddy: %v %v", u, err)
	}
	if u, err := qd(req); err == nil || u != nil {
		t.Errorf("an enabled but unavailable egress must fail, not fall back: %v %v", u, err)
	}
	if u, err := ProxyFunc("other")(req); u != nil || err != nil {
		t.Errorf("a platform without a setting follows the environment: %v %v", u, err)
	}

	Set(nil)
	if u, err := wb(req); u != nil || err != nil {
		t.Errorf("after clearing: %v %v", u, err)
	}
}
