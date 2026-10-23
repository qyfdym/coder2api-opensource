package kiroacct

// 代理池的成员来源，以及 WorkBuddy / Qoder 怎么用这个池。
//
// 池里的成员有三个来源：config.json 的 kiro.proxy_pool（只读）、面板手动添加的代理（ProxyStore）、
// 机场订阅里勾选的节点（代理内核为每个节点开的本机监听口，由 NodeURLs 提供）。Kiro 按账号分配；
// WorkBuddy / Qoder 在面板里打开出口代理后按平台选一个（见 EgressResolver）。

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

// ManagedProxyURLs 面板托管的池成员：手动添加且启用的代理 + 订阅节点（内核在运行时）。
func (s *Service) ManagedProxyURLs() []string {
	var out []string
	if s.Proxies != nil {
		out = append(out, s.Proxies.EnabledURLs()...)
	}
	if s.NodeURLs != nil {
		out = append(out, s.NodeURLs()...)
	}
	return out
}

// SyncManagedProxies 把当前的托管成员热更新进 Kiro 客户端。
func (s *Service) SyncManagedProxies() {
	if s != nil && s.Client != nil {
		s.Client.SetManagedProxies(s.ManagedProxyURLs())
	}
}

// PlatformName 平台的中文展示名。
func PlatformName(p string) string {
	switch p {
	case egress.WorkBuddy:
		return "WorkBuddy"
	case egress.Qoder:
		return "Qoder"
	}
	return p
}

// IsCNHost 国内版上游的域名：*.cn、*.tencent.com、*.qq.com（WorkBuddy 国内版 copilot.tencent.com /
// codebuddy.cn / workbuddy.cn，Qoder 国内版 *.qoder.com.cn）。
func IsCNHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "tencent.com" || h == "qq.com" {
		return true
	}
	for _, suf := range []string{".cn", ".tencent.com", ".qq.com"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// PlatformProxy 某个平台对目标主机现在该走的代理（完整地址，可能带密码；只在服务端使用）。
// "" + nil = 不走代理（没开，或国内域名直连）；"" + err = 开了但没有可用的代理。
func (s *Service) PlatformProxy(platform, host string) (string, error) {
	if s == nil || s.Proxies == nil || s.Client == nil {
		return "", nil
	}
	pe := s.Proxies.Platform(platform)
	if pe.Mode == EgressOff {
		return "", nil
	}
	if pe.DirectCN && host != "" && IsCNHost(host) {
		return "", nil
	}
	switch pe.Mode {
	case EgressAuto:
		if p := s.Client.PickProxy("platform:" + platform); p != "" {
			return p, nil
		}
		return "", errors.New("[出口：" + PlatformName(platform) + " 代理池] 代理池是空的，没有可用的代理（在面板「代理池」页添加代理或勾选订阅节点，或关闭该平台的出口代理）")
	case EgressPin:
		for _, m := range s.Client.PoolMembers() {
			if kiro.ProxyID(m.URL) == pe.ProxyID {
				return m.URL, nil
			}
		}
		return "", errors.New("[出口：" + PlatformName(platform) + "] 指定的代理已不在代理池里（在面板「代理池」页重新选择）")
	}
	return "", nil
}

// EgressResolver 给 egress 包用的解析函数（装配期 egress.Set 注入）。
func (s *Service) EgressResolver() egress.Resolver {
	return func(platform string, req *http.Request) (*url.URL, error) {
		host := ""
		if req != nil && req.URL != nil {
			host = req.URL.Hostname()
		}
		raw, err := s.PlatformProxy(platform, host)
		if err != nil || raw == "" {
			return nil, err
		}
		return url.Parse(raw)
	}
}
