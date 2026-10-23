package proxycore

// 真内核集成测试（默认跳过）：
//
//	PROXYCORE_REAL_MIHOMO=<mihomo 可执行文件> PROXYCORE_REAL_NODE=<host:port 一个能用的 http 代理>
//	go test ./internal/proxycore -run RealMihomo -v
//
// 把那个 http 代理当作订阅里的节点，验证网关生成的配置在真 mihomo 上的行为：勾选的节点能出去、
// 代理口必须带认证、节点从订阅里消失后代理口故障闭合（不走直连）。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealMihomo(t *testing.T) {
	bin, node := os.Getenv("PROXYCORE_REAL_MIHOMO"), os.Getenv("PROXYCORE_REAL_NODE")
	if bin == "" || node == "" {
		t.Skip("set PROXYCORE_REAL_MIHOMO and PROXYCORE_REAL_NODE to run")
	}
	host, port, err := net.SplitHostPort(node)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(bin)
	m, err := New(Options{Dir: t.TempDir(), BinPath: abs, BasePort: 37890, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	ctx := context.Background()
	sub := fmt.Sprintf("proxies:\n  - {\"name\":\"🇺🇸 美国 [测试]|01 (x1.0)\",\"type\":\"http\",\"server\":%q,\"port\":%s}\n"+
		"  - {\"name\":\"Dead\",\"type\":\"http\",\"server\":\"127.0.0.1\",\"port\":1}\n", host, port)
	v, err := m.AddSub(ctx, AddSubInput{Name: "real", Content: sub})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Status().Running {
		t.Fatalf("core not running: %+v %v", m.Status(), m.Logs(10))
	}
	nodes, err := m.Nodes(ctx, v.ID)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	if err := m.Select(ctx, v.ID, []string{nodes[0].Name, "Dead"}); err != nil {
		t.Fatal(err)
	}
	urls := m.NodeURLs()
	get := func(proxy string) error {
		u, _ := url.Parse(proxy)
		hc := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
		resp, err := hc.Get("https://q.us-east-1.amazonaws.com/")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := get(urls[0]); err != nil {
		t.Fatalf("the selected node must work: %v", err)
	}
	if err := get(urls[1]); err == nil {
		t.Errorf("a dead node must fail")
	}
	noAuth, _ := url.Parse(urls[0])
	noAuth.User = nil
	if err := get(noAuth.String()); err == nil {
		t.Errorf("the node listener must require the credentials")
	}
	if d, err := m.Delay(ctx, v.ID, []string{nodes[0].Name}); err != nil || d[nodes[0].Name] <= 0 {
		t.Errorf("delay: %v %v", d, err)
	}

	// 节点从订阅里消失：同一个代理口必须失败，而不是改走直连。
	gone := "proxies:\n  - {\"name\":\"Dead\",\"type\":\"http\",\"server\":\"127.0.0.1\",\"port\":1}\n"
	if err := m.SetContent(ctx, v.ID, gone); err != nil {
		t.Fatal(err)
	}
	if err := get(urls[0]); err == nil {
		t.Errorf("a node that left the subscription must fail closed")
	}
	nodes, _ = m.Nodes(ctx, v.ID)
	missing := false
	for _, n := range nodes {
		missing = missing || n.Missing
	}
	if !missing {
		t.Errorf("the vanished node must be reported as missing: %+v", nodes)
	}
}
