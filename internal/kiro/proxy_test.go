package kiro

// 出站代理：账号级覆盖、代理池（稳定分配 + 健康顺延）、传输层不静默回落。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

func TestNormalizeProxyURL(t *testing.T) {
	ok := map[string]string{
		"":                              "",
		"  ":                            "",
		"direct":                        ProxyDirect,
		" DIRECT ":                      ProxyDirect,
		"http://127.0.0.1:7890":         "http://127.0.0.1:7890",
		"https://proxy.example.com:443": "https://proxy.example.com:443",
		"socks5://u:p@10.0.0.1:1080":    "socks5://u:p@10.0.0.1:1080",
		"socks5h://10.0.0.1:1080":       "socks5h://10.0.0.1:1080",
	}
	for in, want := range ok {
		if got, err := NormalizeProxyURL(in); err != nil || got != want {
			t.Errorf("NormalizeProxyURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"127.0.0.1:7890", "ftp://h:21", "http://h", "http://:80", "http://", "vmess://abc", "http://bad host:1"} {
		if got, err := NormalizeProxyURL(in); err == nil {
			t.Errorf("NormalizeProxyURL(%q) = %q, want error", in, got)
		}
	}
}

func TestMaskProxyURLDropsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"socks5://user:secret@10.0.0.1:1080": "socks5://10.0.0.1:1080",
		"http://127.0.0.1:7890":              "http://127.0.0.1:7890",
		"direct":                             "direct",
		"":                                   "",
		"::bad::":                            "（无效地址）",
	} {
		got := MaskProxyURL(in)
		if got != want {
			t.Errorf("MaskProxyURL(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("credentials leaked: %q", got)
		}
	}
}

func poolClient(pool ...string) *Client {
	return NewClient(Options{Transport: newFake(), ProxyPool: pool, Logf: func(string, ...any) {}})
}

var threeProxies = []string{"http://p1.example:8080", "http://p2.example:8080", "http://p3.example:8080"}

func machineCred(i int) Cred {
	return Cred{UID: fmt.Sprintf("u%d", i), MachineID: fmt.Sprintf("machine-%d", i)}
}

// 同一个账号总是落在同一个代理上；账号大致均匀分布；池里去掉一个成员只影响落在它身上的账号。
func TestProxyPoolStableAssignment(t *testing.T) {
	c := poolClient(threeProxies...)
	counts := map[string]int{}
	first := map[int]string{}
	for i := 0; i < 300; i++ {
		p := c.proxyFor(machineCred(i))
		if p != c.proxyFor(machineCred(i)) {
			t.Fatalf("assignment for account %d is not stable", i)
		}
		counts[p]++
		first[i] = p
	}
	for _, p := range threeProxies {
		if counts[p] < 60 {
			t.Errorf("proxy %s got only %d of 300 accounts: %v", p, counts[p], counts)
		}
	}
	shrunk := poolClient(threeProxies[0], threeProxies[2]) // 去掉 p2
	for i := 0; i < 300; i++ {
		if first[i] != threeProxies[1] && shrunk.proxyFor(machineCred(i)) != first[i] {
			t.Errorf("account %d moved from %s although its proxy is still in the pool", i, first[i])
		}
	}
}

// 代理连不上时顺延到该账号偏好顺序里的下一个，恢复后自动回来；全挂时仍用排第一的，绝不回落直连。
func TestProxyPoolFailoverAndRecovery(t *testing.T) {
	c := poolClient(threeProxies...)
	now := time.Unix(1_000_000, 0)
	c.health.now = func() time.Time { return now }
	cred := machineCred(7)
	order := poolOrder(c.pool, credKey(cred))
	if got := c.proxyFor(cred); got != order[0] {
		t.Fatalf("healthy pool must pick the first preference, got %s want %s", got, order[0])
	}
	c.health.failure(order[0])
	if got := c.proxyFor(cred); got != order[0] {
		t.Errorf("a single failure must not demote the proxy, got %s", got)
	}
	c.health.failure(order[0])
	if got := c.proxyFor(cred); got != order[1] {
		t.Errorf("after repeated failures the next preference must be used, got %s want %s", got, order[1])
	}
	if info := c.ProxyInfo(cred); info.Kind != "pool" || info.Label != MaskProxyURL(order[1]) || info.Down {
		t.Errorf("info while failing over: %+v", info)
	}
	// 全部不可用：仍选排第一的（请求会直接失败），不是 "" 也不是 direct。
	for _, p := range order {
		c.health.failure(p)
		c.health.failure(p)
	}
	if got := c.proxyFor(cred); got != order[0] {
		t.Errorf("all down must still pick %s, got %q", order[0], got)
	}
	if info := c.ProxyInfo(cred); !info.Down {
		t.Errorf("the chosen proxy must be reported as down: %+v", info)
	}
	// 不可用期满后放请求去试探，成功即恢复。
	now = now.Add(proxyDownFor + time.Second)
	if got := c.proxyFor(cred); got != order[0] {
		t.Errorf("after the down window the first preference is tried again, got %s", got)
	}
	c.health.failure(order[0]) // 试探仍失败：立刻再次标记不可用
	if c.health.healthy(order[0]) {
		t.Errorf("a failed probe must mark the proxy down again")
	}
	now = now.Add(proxyDownFor + time.Second)
	c.health.success(order[0])
	c.health.failure(order[0])
	if !c.health.healthy(order[0]) {
		t.Errorf("success must reset the failure count")
	}
}

func TestAccountProxyOverridesPool(t *testing.T) {
	c := poolClient(threeProxies...)
	cred := machineCred(1)

	cred.ProxyURL = "socks5://u:secret@10.9.9.9:1080"
	if got := c.proxyFor(cred); got != cred.ProxyURL {
		t.Errorf("account proxy must win over the pool, got %s", got)
	}
	info := c.ProxyInfo(cred)
	if info.Kind != "account" || info.Label != "socks5://10.9.9.9:1080" {
		t.Errorf("account info: %+v", info)
	}
	// 账号自己指定的代理不参与池的顺延。
	c.health.failure(cred.ProxyURL)
	c.health.failure(cred.ProxyURL)
	if got := c.proxyFor(cred); got != cred.ProxyURL {
		t.Errorf("account proxy must not fail over, got %s", got)
	}

	cred.ProxyURL = ProxyDirect
	if got := c.proxyFor(cred); got != ProxyDirect {
		t.Errorf("direct must win over the pool, got %s", got)
	}
	if info := c.ProxyInfo(cred); info.Kind != "direct" || info.Label != "" {
		t.Errorf("direct info: %+v", info)
	}
}

func TestProxyInfoWithoutPool(t *testing.T) {
	none := NewClient(Options{Transport: newFake()})
	if got := none.proxyFor(machineCred(1)); got != "" {
		t.Errorf("no pool: platform default expected, got %q", got)
	}
	if info := none.ProxyInfo(machineCred(1)); info.Kind != "none" {
		t.Errorf("none info: %+v", info)
	}
	def := NewClient(Options{Transport: newFake(), ProxyURL: "http://user:pw@127.0.0.1:7890"})
	if info := def.ProxyInfo(machineCred(1)); info.Kind != "default" || info.Label != "http://127.0.0.1:7890" {
		t.Errorf("default info: %+v", info)
	}
	// 池里的无效成员 / direct / 重复项被忽略。
	c := poolClient("not a proxy", "direct", threeProxies[0], threeProxies[0])
	if len(c.pool) != 1 || c.pool[0] != threeProxies[0] {
		t.Errorf("pool = %v", c.pool)
	}
}

// 传输层：direct 强制不走代理；指定代理就用它；地址解析不了时请求报错，而不是悄悄走别的出口。
func TestTransportForProxies(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://q.us-east-1.amazonaws.com/x", nil)

	if tr := transportFor("http://default.example:1", ProxyDirect); tr.Proxy != nil {
		t.Errorf("direct must disable the proxy even when a default proxy is configured")
	}
	tr := transportFor("", "http://127.0.0.1:7890")
	if u, err := tr.Proxy(req); err != nil || u == nil || u.Host != "127.0.0.1:7890" {
		t.Errorf("explicit proxy: %v %v", u, err)
	}
	if tr.ForceAttemptHTTP2 {
		t.Errorf("proxied transports pin HTTP/1.1")
	}
	tr = transportFor("http://default.example:1", "")
	if u, err := tr.Proxy(req); err != nil || u == nil || u.Host != "default.example:1" {
		t.Errorf("platform default proxy: %v %v", u, err)
	}
	tr = transportFor("", "http://bad host:1")
	if u, err := tr.Proxy(req); err == nil {
		t.Errorf("an unparsable proxy must fail the request, got %v", u)
	}
}

type fixedRT struct {
	err  error
	resp *http.Response
	// connected 返回之前先报告「拿到了连接」（模拟连上之后上游才出问题）。
	connected bool
}

func (f fixedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if tr := httptrace.ContextClientTrace(r.Context()); f.connected && tr != nil && tr.GotConn != nil {
		tr.GotConn(httptrace.GotConnInfo{})
	}
	return f.resp, f.err
}

// 只有「连代理」这一段的错误才计入代理健康；上游慢 / 请求被取消不算。
func TestHealthRoundTripperCountsOnlyProxyFaults(t *testing.T) {
	h := newProxyHealth()
	now := time.Unix(1_000_000, 0)
	h.now = func() time.Time { return now }
	const key = "http://p1.example:8080"
	proxyErr := &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("connection refused")}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)

	slow := healthRT{base: fixedRT{err: errors.New("net/http: timeout awaiting response headers"), connected: true}, key: key, h: h}
	for i := 0; i < 5; i++ {
		_, _ = slow.RoundTrip(req)
	}
	if !h.healthy(key) {
		t.Fatalf("upstream slowness must not mark the proxy down")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := req.Clone(ctx)
	bad := healthRT{base: fixedRT{err: proxyErr}, key: key, h: h}
	for i := 0; i < 5; i++ {
		_, _ = bad.RoundTrip(cancelled)
	}
	if !h.healthy(key) {
		t.Fatalf("a cancelled request must not mark the proxy down")
	}

	_, _ = bad.RoundTrip(req)
	_, _ = bad.RoundTrip(req)
	if h.healthy(key) {
		t.Fatalf("two proxy connect failures must mark the proxy down")
	}
	now = now.Add(proxyDownFor + time.Second)
	good := healthRT{base: fixedRT{resp: &http.Response{StatusCode: 200}}, key: key, h: h}
	if _, err := good.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	_, _ = bad.RoundTrip(req)
	if !h.healthy(key) {
		t.Fatalf("a success must reset the failure streak")
	}
}

// 账号的所有出站请求共用同一个出口：推理流 / 查询 / 登录三类客户端对同一账号取到的是同一个出口的客户端集合。
func TestClientSetsAreCachedPerEgress(t *testing.T) {
	c := poolClient(threeProxies...)
	a, b := machineCred(1), machineCred(2)
	if c.streamFor(a) == nil || c.restFor(a) == nil || c.authFor(a) == nil {
		t.Fatal("nil client")
	}
	if c.setFor(c.proxyFor(a)) != c.setFor(c.proxyFor(a)) {
		t.Errorf("client set must be cached per egress")
	}
	direct := a
	direct.ProxyURL = ProxyDirect
	if c.setFor(c.proxyFor(direct)) == c.setFor(c.proxyFor(a)) {
		t.Errorf("direct and pooled egress must not share clients")
	}
	_ = b
}
