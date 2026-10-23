package kiro

// 代理列表文本解析、面板托管成员的热更新、连通性测试。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestParseProxyListFormats(t *testing.T) {
	text := strings.Join([]string{
		"# 注释行",
		"",
		"socks5://user:pass@1.2.3.4:1080   美国 01",
		"http://5.6.7.8:8080",
		"9.9.9.9:3128",
		"alice:s3cret@10.0.0.1:8000 带认证",
		"10.0.0.2:8001:bob:p:w",
		"[2001:db8::1]:8080",
		"http://5.6.7.8:8080", // 重复
	}, "\n")
	items, skipped := ParseProxyList(text, "socks5")
	if len(skipped) != 0 {
		t.Fatalf("skipped: %v", skipped)
	}
	want := []ProxyItem{
		{"socks5://user:pass@1.2.3.4:1080", "美国 01"},
		{"http://5.6.7.8:8080", ""},
		{"socks5://9.9.9.9:3128", ""},
		{"socks5://alice:s3cret@10.0.0.1:8000", "带认证"},
		{"socks5://bob:p%3Aw@10.0.0.2:8001", ""}, // 密码里的冒号被转义
		{"socks5://[2001:db8::1]:8080", ""},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
}

func TestParseProxyListRejectsBadLines(t *testing.T) {
	text := "good.example:8080\nnoport.example\nhost:99999\nhost:abc\nftp://h:21\nvmess://abc\ndirect\na:b:c\n"
	items, skipped := ParseProxyList(text, "")
	if len(items) != 1 || items[0].URL != "http://good.example:8080" {
		t.Fatalf("items = %+v", items)
	}
	if len(skipped) != 7 {
		t.Fatalf("skipped = %v", skipped)
	}
	if !strings.HasPrefix(skipped[0], "第 2 行：") {
		t.Errorf("skip reason must carry the line number, got %q", skipped[0])
	}
	// 无效行不影响其它行；默认协议非法时按 http。
	if it, _ := ParseProxyList("h.example:1", "gopher"); len(it) != 1 || !strings.HasPrefix(it[0].URL, "http://") {
		t.Errorf("bad default scheme must fall back to http: %+v", it)
	}
}

func TestProxyIDStable(t *testing.T) {
	a, b := ProxyID("http://a.example:1"), ProxyID("http://b.example:1")
	if a == b || a != ProxyID("http://a.example:1") || len(a) != 10 {
		t.Errorf("ids: %q %q", a, b)
	}
}

// 面板托管的成员与配置里的合并去重，立即参与选择；去掉后账号回到原来的出口。
func TestSetManagedProxiesHotUpdate(t *testing.T) {
	c := poolClient()
	cred := machineCred(1)
	if got := c.proxyFor(cred); got != "" {
		t.Fatalf("empty pool: %q", got)
	}
	c.SetManagedProxies([]string{"http://m1.example:1", "bad", "direct", "http://m1.example:1"})
	if got := c.proxyFor(cred); got != "http://m1.example:1" {
		t.Fatalf("managed member must be used, got %q", got)
	}
	if info := c.ProxyInfo(cred); info.Kind != "pool" {
		t.Errorf("info = %+v", info)
	}
	c.SetManagedProxies(nil)
	if got := c.proxyFor(cred); got != "" {
		t.Errorf("after clearing: %q", got)
	}

	// 配置里的成员保留，且排在前面；与托管成员重复的只算一个。
	c = poolClient("http://cfg.example:1")
	c.SetManagedProxies([]string{"http://cfg.example:1", "http://m2.example:1"})
	got := c.PoolMembers()
	if len(got) != 2 || !got[0].Config || got[1].Config || got[1].URL != "http://m2.example:1" {
		t.Fatalf("members = %+v", got)
	}
	c.SetManagedProxies(nil)
	if len(c.PoolMembers()) != 1 {
		t.Errorf("config members must survive clearing the managed ones")
	}
}

func TestTestProxyReportsReachabilityAndCountry(t *testing.T) {
	f := newFake()
	f.on(hostQ+"/", func(*http.Request, []byte) (int, http.Header, []byte) { return 403, nil, []byte(`{}`) })
	f.on("ipinfo.io/json", func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"ip":"1.2.3.4","country":"US","org":"AS1 Example"}`)
	})
	c := testClient(f)
	// 先把这个代理标成连不上，成功的测试应让它立刻恢复。
	c.health.failure("http://p.example:1")
	c.health.failure("http://p.example:1")
	res := c.TestProxy(context.Background(), "http://p.example:1")
	if !res.OK || res.Country != "US" || res.IP != "1.2.3.4" || res.Org != "AS1 Example" || res.Error != "" || res.At == 0 {
		t.Fatalf("result = %+v", res)
	}
	if !c.health.healthy("http://p.example:1") {
		t.Errorf("a successful test must clear the down mark")
	}

	// 查不到地区不影响连通结论。
	f2 := newFake()
	f2.on(hostQ+"/", func(*http.Request, []byte) (int, http.Header, []byte) { return 403, nil, nil })
	if res := testClient(f2).TestProxy(context.Background(), "socks5://p.example:1"); !res.OK || res.Country != "" {
		t.Errorf("no geo: %+v", res)
	}

	for _, bad := range []string{"", "direct", "nope", "ftp://h:1"} {
		if res := c.TestProxy(context.Background(), bad); res.OK || res.Error == "" {
			t.Errorf("TestProxy(%q) = %+v, want an error", bad, res)
		}
	}
}

// 失败原因不能带出代理的账号密码。
func TestTestProxyErrorHidesCredentials(t *testing.T) {
	c := NewClient(Options{Transport: errRT{errors.New("proxyconnect tcp: auth failed for hunter2 / alice")}, Logf: func(string, ...any) {}})
	res := c.TestProxy(context.Background(), "http://alice:hunter2@p.example:1")
	if res.OK || res.Error == "" {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.Error, "hunter2") || strings.Contains(res.Error, "alice") {
		t.Errorf("credentials leaked: %q", res.Error)
	}
}
