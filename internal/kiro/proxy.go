package kiro

// 出站代理：账号级覆盖、代理池、平台默认。
//
// Kiro 按出口 IP 的地区决定放出哪些模型（真机：同一个账号，国内 IP 只有 9 个非 Claude 模型，
// 经美国代理是 21 个含 Claude / auto），所以出口 IP 必须可控。选择顺序：
//
//  1. 账号自己设了代理 → 用它（"direct" = 这个账号不走代理，即使环境变量里配了）；
//  2. 配了代理池 → 按账号稳定排序（rendezvous hash），取第一个健康的成员。同一个账号
//     总是落在同一个代理上，池里增删成员只影响落在该成员上的账号；该代理连不上时顺延到
//     排序里的下一个，恢复后自动回来。池里全挂时仍用排第一的（请求直接失败），
//     绝不悄悄改走直连——直连出去的是另一个地区的 IP；
//  3. 否则用平台默认（kiro.proxy_url，空 = 环境变量 HTTPS_PROXY）。
//
// 账号的所有出站请求（推理、刷新、额度、模型目录、profile）共用同一个选择结果。

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyDirect 账号级代理的特殊值：这个账号不走任何代理。
const ProxyDirect = "direct"

const (
	// proxyFailThreshold 连续这么多次连不上代理就暂时标记为不可用。
	proxyFailThreshold = 2
	// proxyDownFor 不可用的持续时间；到期后放一个请求试探，成功即恢复。
	proxyDownFor = time.Minute
)

// NormalizeProxyURL 校验并规范化代理地址。"" = 未设置；"direct" = 不走代理；
// 否则必须是 http / https / socks5 / socks5h 地址（可带 user:pass@）。
func NormalizeProxyURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.EqualFold(s, ProxyDirect) {
		return ProxyDirect, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("代理地址格式不对，应为 http://host:port 或 socks5://host:port")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("代理协议 %q 不支持，只支持 http / https / socks5", u.Scheme)
	}
	if u.Port() == "" {
		return "", errors.New("代理地址缺少端口")
	}
	return s, nil
}

// MaskProxyURL 去掉用户名与密码，只留 scheme://host:port（面板 / 日志显示用）。
func MaskProxyURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, ProxyDirect) {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "（无效地址）"
	}
	return u.Scheme + "://" + u.Host
}

// ProxyID 代理地址的短标识（面板里增删改查、测试用；同一个地址永远得到同一个 ID）。
func ProxyID(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	return hex.EncodeToString(sum[:5])
}

// ProxyInfo 账号当前实际使用的出口，供面板展示。
type ProxyInfo struct {
	// Kind account（账号自己设的）/ pool（代理池分配）/ default（kiro.proxy_url）/
	// direct（显式直连）/ none（没配，跟随环境变量）。
	Kind string
	// Label 脱敏后的代理地址；direct / none 为空。
	Label string
	// Down 该代理最近连不上（池内会自动顺延到别的成员，账号级的不会）。
	Down bool
}

// ---------------------------------------------------------------------------
// 健康状态
// ---------------------------------------------------------------------------

type proxyState struct {
	fails     int
	downUntil time.Time
}

type proxyHealth struct {
	mu  sync.Mutex
	st  map[string]*proxyState
	now func() time.Time
}

func newProxyHealth() *proxyHealth {
	return &proxyHealth{st: map[string]*proxyState{}, now: time.Now}
}

// healthy 代理当前是否可用（不可用期满后视为可用，放请求去试探）。
func (h *proxyHealth) healthy(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.st[key]
	return s == nil || !h.now().Before(s.downUntil)
}

func (h *proxyHealth) failure(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.st[key]
	if s == nil {
		s = &proxyState{}
		h.st[key] = s
	}
	s.fails++
	if s.fails >= proxyFailThreshold {
		s.downUntil = h.now().Add(proxyDownFor)
	}
}

func (h *proxyHealth) success(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.st[key]; s != nil {
		s.fails, s.downUntil = 0, time.Time{}
	}
}

// isProxyFault 错误是否出在「连代理」这一段（拨号 / 隧道建立失败），而不是上游本身慢或报错。
// net/http 把连接代理阶段的所有错误（含 CONNECT 被拒）包成 Op=proxyconnect 的 *net.OpError。
func isProxyFault(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && (oe.Op == "proxyconnect" || oe.Op == "dial") {
		return true
	}
	// 连接还没建立就超时：代理收下了 TCP 连接，却一直没把隧道接通（节点挂了 / 出不去）。
	// 这条文本是 net/http 里固定的 errRequestCanceledConn，没有导出的类型可以判断。
	return strings.Contains(err.Error(), "while waiting for connection")
}

// healthRT 记录经某个代理的请求是否成功连上，供代理池顺延使用。
type healthRT struct {
	base http.RoundTripper
	key  string
	h    *proxyHealth
}

func (t healthRT) RoundTrip(r *http.Request) (*http.Response, error) {
	// 记录这次请求有没有拿到连接（拨号 + 代理隧道 + TLS 都完成）。连接都没拿到就超时，是出口的问题；
	// 拿到连接之后才慢，是上游的问题，不能怪代理。
	var connected atomic.Bool
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))
	resp, err := t.base.RoundTrip(r)
	switch {
	case err == nil:
		t.h.success(t.key)
	// 调用方主动取消（用户关了页面 / 客户端断开）不算代理的错。超时要算：Client.Timeout 到期时
	// net/http 有时报 request canceled while waiting for connection，有时只有 context deadline exceeded，
	// 两种写法都是「代理收下了连接却一直没把隧道接通」。
	case errors.Is(r.Context().Err(), context.Canceled):
	case isProxyFault(err), !connected.Load() && errors.Is(err, context.DeadlineExceeded):
		t.h.failure(t.key)
	}
	return resp, err
}

// egressRT 传输失败时在错误文本末尾标明这次请求走的是哪个出口。登录 / 刷新 / 目录查询失败时，
// 「服务器直连不通」「默认代理不通」「代理池里的某个代理不通」光看 net/http 的报错分不出来。
type egressRT struct {
	base http.RoundTripper
	note string
}

func (t egressRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		// 放在原因前面：错误文本进信封时会被截断，出口信息不能落在被截掉的尾巴上。
		err = fmt.Errorf("[出口：%s] %w", t.note, err)
	}
	return resp, err
}

// egressNote 出口 key 的说明文字（不含代理账号密码）。
func (c *Client) egressNote(key string) string {
	switch {
	case strings.EqualFold(key, ProxyDirect):
		return "不走代理，服务器直连"
	case key == "":
		if p := strings.TrimSpace(c.opts.ProxyURL); p != "" {
			return "默认代理 " + MaskProxyURL(p)
		}
		return "没配代理，服务器直连（或跟随环境变量 HTTPS_PROXY）"
	}
	return "代理 " + MaskProxyURL(key)
}

// ---------------------------------------------------------------------------
// 选择
// ---------------------------------------------------------------------------

func rendezvousScore(key, proxy string) uint64 {
	sum := sha256.Sum256([]byte(key + "\x00" + proxy))
	return binary.BigEndian.Uint64(sum[:8])
}

// poolOrder 代理池按 key 的偏好顺序（得分高的在前）：同一个 key 总是同一个顺序。
func poolOrder(pool []string, key string) []string {
	out := append([]string(nil), pool...)
	sort.SliceStable(out, func(i, j int) bool {
		return rendezvousScore(key, out[i]) > rendezvousScore(key, out[j])
	})
	return out
}

func credKey(cred Cred) string {
	if id := strings.TrimSpace(cred.MachineID); id != "" {
		return id
	}
	return strings.TrimSpace(cred.UID)
}

// proxyFor 这个账号当前该走哪个出口："" = 平台默认，"direct" = 不走代理，其余是代理地址。
func (c *Client) proxyFor(cred Cred) string {
	if p := strings.TrimSpace(cred.ProxyURL); p != "" {
		return p
	}
	members := c.proxyMembers()
	if len(members) == 0 {
		return ""
	}
	order := poolOrder(members, credKey(cred))
	for _, p := range order {
		if c.health.healthy(p) {
			return p
		}
	}
	return order[0]
}

// proxyMembers 当前生效的代理池成员（配置 + 面板托管）。返回的切片只读：更新时整体替换，不原地修改。
func (c *Client) proxyMembers() []string {
	c.poolMu.RLock()
	defer c.poolMu.RUnlock()
	return c.all
}

// SetManagedProxies 设置面板托管的代理池成员，与配置文件里的 proxy_pool 合并去重（配置里的在前）。
// 无效地址与 direct 被忽略。热生效：之后的请求按新池选出口，已在途的请求不受影响。
func (c *Client) SetManagedProxies(urls []string) {
	merged := append([]string(nil), c.pool...)
	seen := make(map[string]bool, len(merged)+len(urls))
	for _, p := range merged {
		seen[p] = true
	}
	for _, raw := range urls {
		n, err := NormalizeProxyURL(raw)
		if err != nil || n == "" || strings.EqualFold(n, ProxyDirect) || seen[n] {
			continue
		}
		seen[n] = true
		merged = append(merged, n)
	}
	c.poolMu.Lock()
	c.all = merged
	c.poolMu.Unlock()
}

// PoolMember 代理池里的一个成员，供面板展示。
type PoolMember struct {
	// URL 完整地址（可能带账号密码，只在服务端内部使用，不得下发给前端）。
	URL string
	// Label 脱敏地址。
	Label string
	// Config 来自配置文件（面板里只读）；false = 面板托管。
	Config bool
	// Down 最近连不上，暂时被跳过。
	Down bool
}

// PoolMembers 当前生效的代理池成员（顺序：配置在前，其后是面板托管的）。
func (c *Client) PoolMembers() []PoolMember {
	fromConfig := make(map[string]bool, len(c.pool))
	for _, p := range c.pool {
		fromConfig[p] = true
	}
	members := c.proxyMembers()
	out := make([]PoolMember, 0, len(members))
	for _, p := range members {
		out = append(out, PoolMember{URL: p, Label: MaskProxyURL(p), Config: fromConfig[p], Down: !c.health.healthy(p)})
	}
	return out
}

// EgressKey 账号当前实际使用的出口原值（"" = 平台默认，"direct" = 直连，其余是代理地址，可能带密码）。
// 只用来比较「出口有没有变」，不得展示或记日志。
func (c *Client) EgressKey(cred Cred) string { return c.proxyFor(cred) }

// ProxyInfo 账号当前实际使用的出口（脱敏），供面板展示。
func (c *Client) ProxyInfo(cred Cred) ProxyInfo {
	key := c.proxyFor(cred)
	switch {
	case strings.EqualFold(key, ProxyDirect):
		return ProxyInfo{Kind: "direct"}
	case key == "":
		if p := strings.TrimSpace(c.opts.ProxyURL); p != "" {
			return ProxyInfo{Kind: "default", Label: MaskProxyURL(p)}
		}
		return ProxyInfo{Kind: "none"}
	case strings.TrimSpace(cred.ProxyURL) != "":
		return ProxyInfo{Kind: "account", Label: MaskProxyURL(key), Down: !c.health.healthy(key)}
	default:
		return ProxyInfo{Kind: "pool", Label: MaskProxyURL(key), Down: !c.health.healthy(key)}
	}
}

// ---------------------------------------------------------------------------
// 按出口缓存的 HTTP 客户端
// ---------------------------------------------------------------------------

// restTimeout 额度 / 模型 / 刷新 / 登录这类短请求的整体超时（变量只为让测试能缩短它）。
var restTimeout = 30 * time.Second

// httpSet 一个出口对应的三个客户端。
type httpSet struct {
	stream *http.Client // 推理流：无整体超时（靠 ctx + 响应头超时 + 网关空闲看门狗）
	rest   *http.Client // 额度 / 模型 / profile 查询
	auth   *http.Client // 刷新 / 登录
}

// setFor 取（或建）某个出口的客户端集合。
func (c *Client) setFor(key string) *httpSet {
	c.setsMu.Lock()
	defer c.setsMu.Unlock()
	if s, ok := c.sets[key]; ok {
		return s
	}
	s := c.buildSet(key)
	if c.sets == nil {
		c.sets = map[string]*httpSet{}
	}
	c.sets[key] = s
	return s
}

func (c *Client) buildSet(key string) *httpSet {
	var streamRT, restRT http.RoundTripper
	if c.opts.Transport != nil {
		streamRT, restRT = c.opts.Transport, c.opts.Transport
	} else {
		st, rt := transportFor(c.opts.ProxyURL, key), transportFor(c.opts.ProxyURL, key)
		st.ResponseHeaderTimeout = c.opts.HeaderTimeout
		streamRT, restRT = st, rt
	}
	if key != "" && !strings.EqualFold(key, ProxyDirect) {
		streamRT = healthRT{base: streamRT, key: key, h: c.health}
		restRT = healthRT{base: restRT, key: key, h: c.health}
	}
	note := c.egressNote(key)
	streamRT, restRT = egressRT{base: streamRT, note: note}, egressRT{base: restRT, note: note}
	return &httpSet{
		stream: &http.Client{Transport: streamRT},
		rest:   &http.Client{Transport: restRT, Timeout: restTimeout},
		auth:   &http.Client{Transport: restRT, Timeout: restTimeout},
	}
}

// transportFor 按出口 key 构建传输层：key 为空 = 平台默认（defaultProxy，空则环境变量），
// "direct" = 强制不走代理，其余 = 指定代理。代理地址解析失败时请求直接报错，而不是悄悄走别的出口。
func transportFor(defaultProxy, key string) *http.Transport {
	switch {
	case key == "":
		return buildTransport(defaultProxy)
	case strings.EqualFold(key, ProxyDirect):
		t := buildTransport("")
		t.Proxy = nil
		return t
	}
	t := buildTransport("")
	u, err := url.Parse(key)
	if err != nil || u.Hostname() == "" {
		t.Proxy = func(*http.Request) (*url.URL, error) {
			return nil, fmt.Errorf("无效的代理地址 %s", MaskProxyURL(key))
		}
		return t
	}
	t.Proxy = http.ProxyURL(u)
	// 部分 HTTP 代理对 h2 CONNECT 支持不好，走代理时固定 HTTP/1.1。
	t.ForceAttemptHTTP2 = false
	return t
}

// streamFor / restFor / authFor 账号的推理流 / 查询 / 登录客户端（出口按 proxyFor 选择）。
func (c *Client) streamFor(cred Cred) *http.Client { return c.setFor(c.proxyFor(cred)).stream }
func (c *Client) restFor(cred Cred) *http.Client   { return c.setFor(c.proxyFor(cred)).rest }
func (c *Client) authFor(cred Cred) *http.Client   { return c.setFor(c.proxyFor(cred)).auth }
