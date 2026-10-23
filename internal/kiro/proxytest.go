package kiro

// 代理连通性测试：面板「代理池」页的「测试」按钮。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// proxyProbeURL 连通性探测目标：Kiro 推理入口所在的主机。不带凭证请求，上游会回 403 之类，
	// 只要拿到 HTTP 响应就说明「到 Kiro 的这条路是通的」。
	proxyProbeURL = "https://q.us-east-1.amazonaws.com/"
	// proxyGeoURL 查询出口 IP 的所在地区（尽力而为，查不到不影响连通结论）。
	proxyGeoURL = "https://ipinfo.io/json"

	proxyProbeTimeout = 15 * time.Second
	proxyGeoTimeout   = 8 * time.Second
)

// ProxyTestResult 一次代理测试的结果。
type ProxyTestResult struct {
	OK bool `json:"ok"`
	// LatencyMS 到 Kiro 入口拿到响应头的耗时（含代理握手与 TLS）。
	LatencyMS int64 `json:"latency_ms,omitempty"`
	// IP / Country / Org 出口 IP 及其所在国家（ISO 两位代码）与运营商，查不到时为空。
	IP      string `json:"ip,omitempty"`
	Country string `json:"country,omitempty"`
	Org     string `json:"org,omitempty"`
	// Error 失败原因（已去掉代理账号密码）。
	Error string `json:"error,omitempty"`
	// At 测试时刻（Unix 秒）。
	At int64 `json:"at"`
}

// TestProxy 经指定代理探测一次：能不能连到 Kiro 入口，出口 IP 在哪个国家。
// 成功会顺带清除该代理的「最近连不上」标记，让它立刻重新参与选择。
func (c *Client) TestProxy(ctx context.Context, raw string) ProxyTestResult {
	n, err := NormalizeProxyURL(raw)
	if err != nil || n == "" || strings.EqualFold(n, ProxyDirect) {
		return ProxyTestResult{At: time.Now().Unix(), Error: "代理地址无效"}
	}
	return c.probe(ctx, n)
}

// ProbePool 把代理池里的每个成员探测一遍（只测能不能连到 Kiro 入口，不查地区），结果计入连通状态。
// 只靠真实请求更新连通状态的话，只有 WorkBuddy / Qoder 在用、或者长时间没人用的代理挂了也没人知道；
// 后台定时跑一遍，选出口时就能绕开它。并发 4 个。
func (c *Client) ProbePool(ctx context.Context) {
	if !c.probing.CompareAndSwap(false, true) { // 上一轮还没跑完：不叠加
		return
	}
	defer c.probing.Store(false)
	members := c.proxyMembers()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, m := range members {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(p string) {
			defer func() { <-sem; wg.Done() }()
			c.probeOnce(ctx, p, false)
		}(m)
	}
	wg.Wait()
}

// RunPoolProbe 每隔 every 探测一遍代理池，直到 ctx 结束。
func (c *Client) RunPoolProbe(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.ProbePool(ctx)
		}
	}
}

// TestDirect 不经任何代理，测试服务器自己的出口：能不能连到 Kiro 入口、出口 IP 在哪个国家。
// 登录 / 导入账号失败时用它判断是不是服务器本身连不上 AWS。
func (c *Client) TestDirect(ctx context.Context) ProxyTestResult { return c.probe(ctx, ProxyDirect) }

// probe 经指定出口（规范化后的代理地址，或 ProxyDirect）探测一次，并查出口地区。
func (c *Client) probe(ctx context.Context, key string) ProxyTestResult {
	return c.probeOnce(ctx, key, true)
}

// probeOnce 经指定出口探测一次；geo=false 时不查出口地区。代理（非直连）的结果计入连通状态。
func (c *Client) probeOnce(ctx context.Context, key string, geo bool) ProxyTestResult {
	res := ProxyTestResult{At: time.Now().Unix()}
	n := key
	rt := c.opts.Transport
	if rt == nil {
		t := transportFor("", n)
		t.ResponseHeaderTimeout = proxyProbeTimeout
		defer t.CloseIdleConnections()
		rt = t
	}
	hc := &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	pctx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, proxyProbeURL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// 与真实请求（healthRT）同一个口径：连接都没拿到就失败才算代理的错；连上之后上游慢不算。
	var connected atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))
	start := time.Now()
	resp, err := hc.Do(req)
	isProxy := !strings.EqualFold(n, ProxyDirect)
	if err != nil {
		res.Error = proxyErrText(err, n)
		if isProxy && !connected.Load() && !errors.Is(ctx.Err(), context.Canceled) {
			c.health.failure(n)
		}
		return res
	}
	_ = resp.Body.Close()
	res.OK, res.LatencyMS = true, time.Since(start).Milliseconds()
	if isProxy {
		c.health.success(n)
	}
	if !geo {
		return res
	}

	gctx, gcancel := context.WithTimeout(ctx, proxyGeoTimeout)
	defer gcancel()
	greq, err := http.NewRequestWithContext(gctx, http.MethodGet, proxyGeoURL, nil)
	if err != nil {
		return res
	}
	greq.Header.Set("Accept", "application/json")
	gresp, err := hc.Do(greq)
	if err != nil {
		return res
	}
	defer gresp.Body.Close()
	if gresp.StatusCode == http.StatusOK {
		var g struct {
			IP      string `json:"ip"`
			Country string `json:"country"`
			Org     string `json:"org"`
		}
		if json.NewDecoder(io.LimitReader(gresp.Body, 16<<10)).Decode(&g) == nil {
			res.IP, res.Country, res.Org = g.IP, g.Country, g.Org
		}
	}
	return res
}

// proxyErrText 把传输错误整理成给人看的一句话，并抹掉代理的账号密码（有的代理会在报错里回显）。
func proxyErrText(err error, proxyURL string) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "超时（" + proxyProbeTimeout.String() + " 内没有连通）"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	msg := err.Error()
	if u, e := url.Parse(proxyURL); e == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			msg = strings.ReplaceAll(msg, p, "***")
		}
		if name := u.User.Username(); len(name) >= 3 {
			msg = strings.ReplaceAll(msg, name, "***")
		}
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return msg
}
