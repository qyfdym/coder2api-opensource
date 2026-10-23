package kiro

// 代理列表文本解析、面板托管成员的热更新、连通性测试。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
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

// 连接还没建立就超时（代理收下 TCP 却一直没接通隧道）算代理的错；调用方主动取消不算。
func TestProxyHealthCountsConnectTimeoutButNotCancel(t *testing.T) {
	if !isProxyFault(errors.New("net/http: request canceled while waiting for connection")) {
		t.Errorf("a connect-phase timeout must count as a proxy fault")
	}
	if isProxyFault(errors.New("net/http: timeout awaiting response headers")) {
		t.Errorf("a slow upstream is not a proxy fault")
	}

	fail := errRT{errors.New("net/http: request canceled while waiting for connection")}
	h := newProxyHealth()
	rt := healthRT{base: fail, key: "http://p.example:1", h: h}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(canceled, http.MethodGet, "https://q.example/", nil)
	for i := 0; i < proxyFailThreshold+1; i++ {
		_, _ = rt.RoundTrip(req)
	}
	if !h.healthy("http://p.example:1") {
		t.Errorf("a caller-side cancel must not mark the proxy down")
	}

	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	req, _ = http.NewRequestWithContext(expired, http.MethodGet, "https://q.example/", nil)
	for i := 0; i < proxyFailThreshold; i++ {
		_, _ = rt.RoundTrip(req)
	}
	if h.healthy("http://p.example:1") {
		t.Errorf("repeated connect timeouts must mark the proxy down")
	}
}

// 传输失败的错误文本要写明走的是哪个出口（并且不带代理密码）。
func TestTransportErrorNamesTheEgress(t *testing.T) {
	// 连不上的代理：127.0.0.1:1 立即拒绝连接。
	c := NewClient(Options{ProxyPool: []string{"http://alice:hunter2@127.0.0.1:1"}, Logf: func(string, ...any) {}})
	_, _, err := c.postJSON(context.Background(), "https://oidc.us-east-1.amazonaws.com/client/register", map[string]string{})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := transportError(err).Error()
	if !strings.Contains(msg, "出口：代理 http://127.0.0.1:1") || !strings.Contains(msg, "proxyconnect") {
		t.Errorf("the egress and the cause must be in the message: %q", msg)
	}
	if strings.Contains(msg, "hunter2") || strings.Contains(msg, "alice") {
		t.Errorf("credentials leaked: %q", msg)
	}
	// 拨号失败是代理的错：两次之后这个代理被标记为暂时不可用。
	_, _, _ = c.postJSON(context.Background(), "https://oidc.us-east-1.amazonaws.com/client/register", map[string]string{})
	if c.health.healthy("http://alice:hunter2@127.0.0.1:1") {
		t.Errorf("dial failures must mark the proxy down")
	}

	if n := (&Client{}).egressNote(""); !strings.Contains(n, "服务器直连") {
		t.Errorf("no-proxy note: %q", n)
	}
	if n := (&Client{opts: Options{ProxyURL: "http://u:p@d.example:1"}}).egressNote(""); !strings.Contains(n, "默认代理 http://d.example:1") || strings.Contains(n, "u:p") {
		t.Errorf("default-proxy note: %q", n)
	}
	if n := (&Client{}).egressNote(ProxyDirect); !strings.Contains(n, "不走代理") {
		t.Errorf("direct note: %q", n)
	}
}

func TestTestDirect(t *testing.T) {
	f := newFake()
	f.on(hostQ+"/", func(*http.Request, []byte) (int, http.Header, []byte) { return 403, nil, nil })
	f.on("ipinfo.io/json", func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, nil, []byte(`{"ip":"9.9.9.9","country":"CN","org":"AS9808 China Mobile"}`)
	})
	res := testClient(f).TestDirect(context.Background())
	if !res.OK || res.Country != "CN" || res.IP != "9.9.9.9" {
		t.Errorf("direct test = %+v", res)
	}
	bad := NewClient(Options{Transport: errRT{errors.New("dial tcp 1.2.3.4:443: i/o timeout")}, Logf: func(string, ...any) {}})
	if res := bad.TestDirect(context.Background()); res.OK || !strings.Contains(res.Error, "i/o timeout") {
		t.Errorf("failed direct test = %+v", res)
	}
}

// 线上的实际故障：代理收下了 TCP 连接，却一直不把隧道接通。登录 / 刷新要等满整个超时，报
// 「request canceled while waiting for connection」；这个代理必须因此被记为不可用，账号才能顺延到池里的下一个。
func TestHungProxyIsSkippedAfterConnectTimeouts(t *testing.T) {
	old := restTimeout
	restTimeout = 300 * time.Millisecond
	t.Cleanup(func() { restTimeout = old })

	other := "http://127.0.0.1:1"
	var hung string
	for i := 0; i < 40 && hung == ""; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() { // 收下连接，什么都不回
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				t.Cleanup(func() { conn.Close() })
			}
		}()
		cand := "http://" + ln.Addr().String()
		if poolOrder([]string{cand, other}, "")[0] == cand {
			hung = cand // 要让挂起的代理排在登录（无账号）偏好顺序的第一位
		}
	}
	if hung == "" {
		t.Fatal("could not rank the hanging proxy first")
	}

	c := NewClient(Options{ProxyPool: []string{hung, other}, Logf: func(string, ...any) {}})
	var msg string
	for i := 0; i < proxyFailThreshold; i++ {
		_, _, err := c.postJSON(context.Background(), "https://oidc.us-east-1.amazonaws.com/client/register", map[string]string{})
		if err == nil {
			t.Fatal("a hanging proxy must fail the request")
		}
		msg = transportError(err).Error()
		t.Logf("attempt %d: %s", i+1, msg)
	}
	// net/http 对同一种故障有两种写法（见 healthRT），哪种取决于两个取消机制谁先触发。
	if !strings.Contains(msg, "出口：代理 "+hung) ||
		!(strings.Contains(msg, "while waiting for connection") || strings.Contains(msg, "context deadline exceeded")) {
		t.Errorf("the message must name the hanging proxy and say the connection never came up: %q", msg)
	}
	if got := c.proxyFor(Cred{}); got != other {
		t.Errorf("after repeated connect timeouts the account must move on to the next proxy, got %q", got)
	}
}

type connRT struct {
	err      error
	gotConn  bool // 返回错误之前是否已经拿到连接
	sawTrace bool
}

func (c *connRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if tr := httptrace.ContextClientTrace(r.Context()); tr != nil {
		c.sawTrace = true
		if c.gotConn && tr.GotConn != nil {
			tr.GotConn(httptrace.GotConnInfo{})
		}
	}
	return nil, c.err
}

// context deadline exceeded 本身分不清是代理还是上游慢：没拿到连接就超时算代理的错，
// 拿到连接之后才超时是上游慢，不算。
func TestProxyHealthDeadlineNeedsNoConnection(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://q.example/", nil)

	noConn := &connRT{err: context.DeadlineExceeded}
	h := newProxyHealth()
	rt := healthRT{base: noConn, key: "http://p.example:1", h: h}
	for i := 0; i < proxyFailThreshold; i++ {
		_, _ = rt.RoundTrip(req)
	}
	if !noConn.sawTrace {
		t.Fatal("healthRT must install a connection trace")
	}
	if h.healthy("http://p.example:1") {
		t.Errorf("a deadline before any connection must mark the proxy down")
	}

	slow := &connRT{err: context.DeadlineExceeded, gotConn: true}
	h = newProxyHealth()
	rt = healthRT{base: slow, key: "http://p.example:1", h: h}
	for i := 0; i < proxyFailThreshold+2; i++ {
		_, _ = rt.RoundTrip(req)
	}
	if !h.healthy("http://p.example:1") {
		t.Errorf("a deadline after the connection was made is the upstream being slow, not the proxy")
	}
}
