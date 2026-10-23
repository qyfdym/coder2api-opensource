package kiroacct

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

func egressService(t *testing.T) *Service {
	t.Helper()
	store, err := OpenProxyStore(filepath.Join(t.TempDir(), "kiro-proxies.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Client: kiro.NewClient(kiro.Options{Transport: newFakeRT(), Logf: func(string, ...any) {}}), Proxies: store}
}

func TestPlatformEgress(t *testing.T) {
	s := egressService(t)
	// 缺省：关闭（跟随环境变量），国内域名直连默认打开。
	if pe := s.Proxies.Platform(egress.WorkBuddy); pe.Mode != EgressOff || !pe.DirectCN {
		t.Fatalf("default: %+v", pe)
	}
	if p, err := s.PlatformProxy(egress.WorkBuddy, "www.workbuddy.ai"); p != "" || err != nil {
		t.Fatalf("off: %q %v", p, err)
	}

	// 自动但池是空的：失败，不悄悄直连。
	if err := s.Proxies.SetPlatform(egress.WorkBuddy, PlatformEgress{Mode: EgressAuto, DirectCN: true}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.PlatformProxy(egress.WorkBuddy, "www.workbuddy.ai"); p != "" || err == nil || !strings.Contains(err.Error(), "代理池是空的") {
		t.Fatalf("auto with an empty pool: %q %v", p, err)
	}
	// 订阅节点（NodeURLs）与手动添加的代理一起组成池。
	s.NodeURLs = func() []string { return []string{"http://u:p@127.0.0.1:17890"} }
	if _, _, err := s.Proxies.Add([]kiro.ProxyItem{{URL: "http://p1.example:1"}}); err != nil {
		t.Fatal(err)
	}
	s.SyncManagedProxies()
	if got := s.ManagedProxyURLs(); len(got) != 2 {
		t.Fatalf("managed: %v", got)
	}
	p, err := s.PlatformProxy(egress.WorkBuddy, "www.workbuddy.ai")
	if err != nil || (p != "http://p1.example:1" && p != "http://u:p@127.0.0.1:17890") {
		t.Fatalf("auto: %q %v", p, err)
	}
	if again, _ := s.PlatformProxy(egress.WorkBuddy, "www.workbuddy.ai"); again != p {
		t.Errorf("auto must be stable: %q then %q", p, again)
	}
	// 国内域名直连。
	for _, host := range []string{"copilot.tencent.com", "www.codebuddy.cn", "gateway.qoder.com.cn", "x.qq.com"} {
		if p, err := s.PlatformProxy(egress.WorkBuddy, host); p != "" || err != nil {
			t.Errorf("%s must go direct: %q %v", host, p, err)
		}
	}
	if err := s.Proxies.SetPlatform(egress.WorkBuddy, PlatformEgress{Mode: EgressAuto, DirectCN: false}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PlatformProxy(egress.WorkBuddy, "copilot.tencent.com"); p == "" {
		t.Errorf("with direct_cn off, CN hosts use the proxy too")
	}

	// 固定一个；它离开池后报错。
	pin := kiro.ProxyID("http://u:p@127.0.0.1:17890")
	if err := s.Proxies.SetPlatform(egress.Qoder, PlatformEgress{Mode: EgressPin, ProxyID: pin}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.PlatformProxy(egress.Qoder, "api3.qoder.sh"); err != nil || p != "http://u:p@127.0.0.1:17890" {
		t.Fatalf("pin: %q %v", p, err)
	}
	s.NodeURLs = nil
	s.SyncManagedProxies()
	if _, err := s.PlatformProxy(egress.Qoder, "api3.qoder.sh"); err == nil || !strings.Contains(err.Error(), "不在代理池") {
		t.Errorf("a pinned proxy that left the pool must fail: %v", err)
	}

	// 解析函数（给 http.Transport.Proxy 用）。
	req, _ := http.NewRequest(http.MethodGet, "https://api3.qoder.sh/x", nil)
	if u, err := s.EgressResolver()(egress.Qoder, req); u != nil || err == nil {
		t.Errorf("resolver must surface the error: %v %v", u, err)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://www.workbuddy.ai/x", nil)
	if u, err := s.EgressResolver()(egress.WorkBuddy, req); err != nil || u == nil || u.Host != "p1.example:1" {
		t.Errorf("resolver: %v %v", u, err)
	}

	// 设置落盘；非法值拒绝。
	re, err := OpenProxyStore(s.Proxies.path)
	if err != nil {
		t.Fatal(err)
	}
	if pe := re.Platform(egress.Qoder); pe.Mode != EgressPin || pe.ProxyID != pin || pe.DirectCN {
		t.Errorf("reloaded: %+v", pe)
	}
	if pe := re.Platform(egress.WorkBuddy); pe.Mode != EgressAuto || pe.DirectCN {
		t.Errorf("reloaded wb: %+v", pe)
	}
	for _, bad := range []struct {
		plat string
		pe   PlatformEgress
	}{
		{"kiro", PlatformEgress{Mode: EgressAuto}},
		{egress.Qoder, PlatformEgress{Mode: "sometimes"}},
		{egress.Qoder, PlatformEgress{Mode: EgressPin}},
	} {
		if err := s.Proxies.SetPlatform(bad.plat, bad.pe); err == nil {
			t.Errorf("SetPlatform(%s, %+v) must fail", bad.plat, bad.pe)
		}
	}
}

func TestIsCNHost(t *testing.T) {
	for host, want := range map[string]bool{
		"copilot.tencent.com": true, "www.codebuddy.cn": true, "gateway.qoder.com.cn": true, "CN": false,
		"www.workbuddy.ai": false, "api3.qoder.sh": false, "tencent.com": true, "evilcn.com": false, "qq.com.": true,
	} {
		if got := IsCNHost(host); got != want {
			t.Errorf("IsCNHost(%q) = %v, want %v", host, got, want)
		}
	}
}
