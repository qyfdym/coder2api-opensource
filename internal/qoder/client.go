package qoder

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Options Qoder 客户端装配参数（来自网关 config.qoder；零值均有合理缺省）。
type Options struct {
	// DataDir 本包的磁盘缓存目录（模型目录缓存、机器身份缓存等）。空 = 不落盘。
	DataDir string
	// IntlGateway 覆盖国际版推理主机（空 = 官方候选 api3 → api2 → api1）。
	IntlGateway string
	// RequestTimeout 非流式业务请求（账号、额度、签到、模型目录）的总超时，缺省 30s。
	RequestTimeout time.Duration
	// HeaderTimeout 推理请求等待上游响应头的上限，缺省 120s（高思考档 + 长上下文首包可达 40–70s）。
	HeaderTimeout time.Duration
	// QueueMaxWait 上游排队（10605 / isQueued）时单次请求累计等待上限，缺省 10min。
	QueueMaxWait time.Duration
	// QueueMaxRounds 上游排队重试轮数上限，缺省 6。
	QueueMaxRounds int
	// RuntimeInfoPath 官方 UMID 组件 runtime-info 可执行文件路径（签到 / 活动需要真实机器身份）。
	// 空 = 自动探测（Windows 桌面客户端安装目录、DataDir/umid/）；探测不到则签到按无身份降级。
	RuntimeInfoPath string
	// DesktopVersion 活动 / 签到请求头里的桌面客户端版本，缺省 "0.4.3"。
	DesktopVersion string
	// Logf 日志输出（缺省 log.Printf）。
	Logf func(format string, args ...any)
	// Transport 出站传输覆盖（测试注入离线传输用）；nil = 内置 HTTP/1.1 传输。
	Transport http.RoundTripper
}

func (o Options) withDefaults() Options {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.HeaderTimeout <= 0 {
		o.HeaderTimeout = 120 * time.Second
	}
	if o.QueueMaxWait <= 0 {
		o.QueueMaxWait = 10 * time.Minute
	}
	if o.QueueMaxRounds <= 0 {
		o.QueueMaxRounds = 6
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if strings.TrimSpace(o.DesktopVersion) == "" {
		o.DesktopVersion = "0.4.3"
	}
	return o
}

// Client Qoder 上游客户端（并发安全，进程内单例即可）。
//
// 方法按职责分布在多个文件：推理与模型目录（chat*.go / catalog*.go / body*.go），
// 账号生命周期、额度、签到（account*.go / oauth*.go / credits*.go / tasks*.go / identity*.go）。
type Client struct {
	opts Options
	// httpc 非流式业务请求（有总超时）。
	httpc *http.Client
	// streamc 推理 SSE（无总超时，只限响应头；断流由调用方 ctx 取消兜底）。
	streamc *http.Client
	// Sessions COSY 会话缓存（按 UID + access token）。
	Sessions SessionCache

	// catalog 模型目录状态（定义见 catalog_state.go）。
	catalog *catalogState
	// identity 机器身份状态（定义见 identity_state.go）。
	identity *identityState
}

// NewClient 构建客户端。
//
// 传输层：显式 HTTP/1.1（与原实现 urllib 一致的已验证路径），拨号 / TLS 握手 / 响应头
// 各有上限，keep-alive 复用连接；代理遵循环境变量（HTTPS_PROXY / NO_PROXY）。
func NewClient(o Options) *Client {
	o = o.withDefaults()
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tlsConn) http.RoundTripper{},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: o.HeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		DisableCompression:    true, // 推理头声明 accept-encoding: identity
	}
	var rt http.RoundTripper = base
	if o.Transport != nil {
		rt = o.Transport
	}
	c := &Client{
		opts:     o,
		httpc:    &http.Client{Transport: rt, Timeout: o.RequestTimeout},
		streamc:  &http.Client{Transport: rt, Timeout: 0},
		catalog:  newCatalogState(),
		identity: newIdentityState(),
	}
	return c
}

// logf 统一日志出口（前缀 [qoder]）。
func (c *Client) logf(format string, args ...any) {
	c.opts.Logf("[qoder] "+format, args...)
}

// Gateways 返回某区域的推理主机候选（主机在前，备用依次在后）。
func (c *Client) Gateways(realm string) []string {
	cfg := ConfigFor(realm)
	if NormalizeRealm(realm) == RealmIntl && strings.TrimSpace(c.opts.IntlGateway) != "" {
		return []string{strings.TrimRight(strings.TrimSpace(c.opts.IntlGateway), "/")}
	}
	out := []string{cfg.Gateway}
	out = append(out, cfg.GatewayFallbacks...)
	return out
}
