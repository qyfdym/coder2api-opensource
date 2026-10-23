package kiro

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Options 客户端参数（装配期确定）。
type Options struct {
	// HeaderTimeout 推理请求等待响应头的上限；<=0 回落 120s（大上下文 + 思考档首包较慢）。
	HeaderTimeout time.Duration
	// ProxyURL 出站代理（http:// / socks5://）；空 = 跟随环境变量 HTTPS_PROXY。
	ProxyURL string
	// Endpoint 推理入口偏好：auto（缺省，Kiro IDE → CodeWhisperer → AmazonQ 依次回退）/
	// kiro / codewhisperer / amazonq（指定者优先，其余作回退）。
	Endpoint string
	// KiroVersion / NodeVersion / SystemVersion 出站 User-Agent 里的客户端版本（空 = 内置默认）。
	KiroVersion   string
	NodeVersion   string
	SystemVersion string
	// ThinkingSuffix 模型名后缀开启思考（缺省 "-thinking"，与 Kiro-Go 一致）。
	ThinkingSuffix string
	// ModelCacheTTL 动态模型目录的缓存时长；<=0 回落 30 分钟。
	ModelCacheTTL time.Duration
	// Transport 覆盖底层 RoundTripper（测试注入假上游用）；nil = 按 ProxyURL 构建。
	Transport http.RoundTripper
	// Sleep 覆盖轮询等待（测试注入空实现，免得单测真等几秒）；nil = 真实等待。
	Sleep func(ctx context.Context, d time.Duration) error
	// Logf 日志输出；nil = log.Printf（统一加 "[kiro] " 前缀）。
	Logf func(format string, args ...any)
}

// Client Kiro 协议客户端（进程内单例，并发安全）。
type Client struct {
	opts   Options
	stream *http.Client // 推理流：无整体超时（靠 ctx + 响应头超时 + 网关空闲看门狗）
	rest   *http.Client // 额度 / 模型 / profile 查询
	auth   *http.Client // 刷新 / 登录

	models modelCache

	// profileSuppress Builder ID 账号的 ListAvailableProfiles 不受支持（403）：记下来 24h 内不再查。
	profileSuppress sync.Map // uid → time.Time

	// sleep 轮询等待（nil = 真实等待；测试注入空实现，免得单测真等几秒）。
	sleep func(ctx context.Context, d time.Duration) error
}

// wait 等待 d 或 ctx 结束。
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	// defaultKiroVersion 当前 Kiro IDE 的版本号（本机安装的 1.3.11）。真机对比（同一账号、同一机器标识）：
	// 0.11.107 调 ListAvailableModels 拿到 21 个模型的目录（含 auto / Claude，这个学生版 Builder ID
	// 号用它们推理会被 INVALID_MODEL_ID 拒绝），1.3.11 拿到的是账号真正可用的 9 个；
	// 1.3.11 同时要求推理与目录请求带 profileArn。
	defaultKiroVersion   = "1.3.11"
	defaultNodeVersion   = "22.22.0"
	defaultHeaderTimeout = 120 * time.Second
	defaultModelCacheTTL = 30 * time.Minute
	defaultRegion        = "us-east-1"
)

// NewClient 构建客户端。
func NewClient(opts Options) *Client {
	if opts.HeaderTimeout <= 0 {
		opts.HeaderTimeout = defaultHeaderTimeout
	}
	if strings.TrimSpace(opts.KiroVersion) == "" {
		opts.KiroVersion = defaultKiroVersion
	}
	if strings.TrimSpace(opts.NodeVersion) == "" {
		opts.NodeVersion = defaultNodeVersion
	}
	if strings.TrimSpace(opts.SystemVersion) == "" {
		opts.SystemVersion = defaultSystemVersion()
	}
	if opts.ThinkingSuffix == "" {
		opts.ThinkingSuffix = "-thinking"
	}
	if opts.ModelCacheTTL <= 0 {
		opts.ModelCacheTTL = defaultModelCacheTTL
	}
	opts.Endpoint = strings.ToLower(strings.TrimSpace(opts.Endpoint))
	if opts.Endpoint == "" {
		opts.Endpoint = "auto"
	}
	c := &Client{opts: opts, sleep: opts.Sleep}
	streamRT := opts.Transport
	restRT := opts.Transport
	if streamRT == nil {
		st := buildTransport(opts.ProxyURL)
		st.ResponseHeaderTimeout = opts.HeaderTimeout
		streamRT = st
		restRT = buildTransport(opts.ProxyURL)
	}
	c.stream = &http.Client{Transport: streamRT}
	c.rest = &http.Client{Transport: restRT, Timeout: 30 * time.Second}
	c.auth = &http.Client{Transport: restRT, Timeout: 30 * time.Second}
	return c
}

func buildTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
		Proxy:               http.ProxyFromEnvironment,
	}
	if s := strings.TrimSpace(proxyURL); s != "" {
		if u, err := url.Parse(s); err == nil {
			t.Proxy = http.ProxyURL(u)
			// 部分 HTTP 代理对 h2 CONNECT 支持不好，走代理时固定 HTTP/1.1。
			t.ForceAttemptHTTP2 = false
		}
	}
	return t
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.opts.Logf != nil {
		c.opts.Logf(format, args...)
		return
	}
	log.Printf("[kiro] "+format, args...)
}

// ThinkingSuffix 模型名开启思考的后缀（/v1/models 列出 thinking 变体用）。
func (c *Client) ThinkingSuffix() string { return c.opts.ThinkingSuffix }

// ---------------------------------------------------------------------------
// 出站请求头（移植自 Kiro-Go proxy/kiro_headers.go）
// ---------------------------------------------------------------------------

const (
	streamingSDKVersion = "1.0.34"
	runtimeSDKVersion   = "1.0.0"
)

// userAgents 拼出 User-Agent / x-amz-user-agent（与 Kiro IDE 内置的 aws-sdk-js 同形态）。
func (c *Client) userAgents(cred Cred, apiName, sdkVersion, mode string) (ua, amzUA string) {
	ua = fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion, c.opts.SystemVersion, c.opts.NodeVersion, apiName, sdkVersion, mode, c.opts.KiroVersion)
	amzUA = fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, c.opts.KiroVersion)
	if id := strings.TrimSpace(cred.MachineID); id != "" {
		ua += "-" + id
		amzUA += "-" + id
	}
	return ua, amzUA
}

// applyBaseHeaders 推理与 REST 共用的头：Bearer、tokentype、UA。
func applyBaseHeaders(req *http.Request, cred Cred, ua, amzUA string) {
	if tok := cred.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Del("TokenType")
	req.Header.Del("tokenType")
	req.Header.Del("tokentype")
	switch {
	case cred.IsAPIKey():
		// 上游两种大小写都认；Kiro CLI 抓包是小写 tokentype。
		req.Header.Set("tokentype", "API_KEY")
	case strings.EqualFold(strings.TrimSpace(cred.AuthMethod), "external_idp"):
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	case strings.EqualFold(strings.TrimSpace(cred.AuthMethod), "idc"):
		// Kiro IDE 1.3.x 对 AWS IAM Identity Center / Builder ID 的 token 都带这个头（社交登录不带）。
		req.Header.Set("TokenType", "SSO_OIDC")
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-amz-user-agent", amzUA)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
}

// setRESTHeaders REST 接口（getUsageLimits / ListAvailableModels / profiles）的头。
func (c *Client) setRESTHeaders(req *http.Request, cred Cred) {
	ua, amzUA := c.userAgents(cred, "codewhispererruntime", runtimeSDKVersion, "m/N,E")
	req.Header.Set("Accept", "application/json")
	applyBaseHeaders(req, cred, ua, amzUA)
}

// ---------------------------------------------------------------------------
// 标识符
// ---------------------------------------------------------------------------

// newUUID 随机 UUID v4。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b[:])
}

// uuidSHA1 RFC 4122 v5（命名空间 URL）：同一会话前缀稳定映射到同一 conversationId。
func uuidSHA1(name string) string {
	ns := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return formatUUID(sum[:16])
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewMachineID 新账号的机器标识（UUID v4，与 Kiro-Go 的 OAuth 账号一致）。
func NewMachineID() string { return newUUID() }

// MachineIDFromAPIKey API Key 账号的机器标识：sha256("KiroAPIKey/<key>")（Kiro CLI 口径）。
func MachineIDFromAPIKey(key string) string {
	sum := sha256.Sum256([]byte("KiroAPIKey/" + key))
	return hex.EncodeToString(sum[:])
}

// StableUID 由账号的稳定身份（userId / 邮箱 / refresh token / API Key）派生网关侧 uid：
// 同一个账号重复导入落到同一个凭证文件，不会在池里出现两份。只含 [0-9a-f]，可直接拼文件名。
func StableUID(identity string) string {
	sum := sha256.Sum256([]byte("kiro-account/" + strings.TrimSpace(identity)))
	return "k" + hex.EncodeToString(sum[:])[:15]
}
