package qoder

// 账号类业务接口（RealmConfig.OpenAPI 基址）的公共传输层：请求头口径、重试、错误分类，
// 以及按原实现语义宽松读取上游 JSON 字段的小工具。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：http_json / _retryable / Account.headers / Account.desktop_headers /
// session_dead / normalize_epoch / _truthy。
//
// 与推理链路不同，这里的接口全部是「纯 Bearer」：没有 COSY 签名，鉴权只看 Authorization
// 里的 access token。但请求头口径依然要对齐官方客户端——服务端对活动 / 签到接口会按 UA、
// cosy-clienttype、cosy-version 与机器头决定「返回什么」（不对也不报错，只是静默少给活动），
// 所以这些头的取值不是装饰，是功能的一部分。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 业务端点（全部挂 RealmConfig.OpenAPI；与原实现 PATH_* 常量一致）。
const (
	oaPathDevicePoll       = "/api/v1/deviceToken/poll"
	oaPathDeviceRefresh    = "/api/v1/deviceToken/refresh"
	oaPathJobExchange      = "/api/v1/jobToken/exchange"
	oaPathJobRefresh       = "/api/v1/jobToken/refresh"
	oaPathUserInfo         = "/api/v1/userinfo"
	oaPathQuota            = "/api/v2/quota/usage"
	oaPathPlan             = "/api/v2/user/plan"
	oaPathCheckinStatus    = "/sash/api/v1/me/daily-check-in/status"
	oaPathCheckinClaim     = "/sash/api/v1/me/daily-check-in/claim"
	oaPathProEligibility   = "/sash/api/v1/me/pro-upgrade/eligibility"
	oaPathProClaim         = "/sash/api/v1/me/pro-upgrade/claim"
	oaPathCampaigns        = "/sash/api/v1/me/campaigns"
	oaPathCampaignClaimFmt = "/sash/api/v1/me/campaigns/%s/claim"
)

// 桌面端专用请求头取值（活动平台必需；CLI=5 / QoderWork=6 / 桌面端=10）。
// 机器 OS / 主机名沿用原实现的固定值：它们只在「原生身份可用」时才随六头一起发出。
const (
	oaDesktopClientType = "10"
	oaDesktopUA         = "Qoder"
	oaDesktopMachineOS  = "x86_64_win32"
	oaDesktopHostname   = "DESKTOP-QODER"
	// oaMaxBody 单个账号类响应体读取上限（额度 / 活动列表都只有几 KB，8MB 足够防御异常大包）。
	oaMaxBody = 8 << 20
	// oaBodyKeep Error.Body 保留的上游原文长度（字节）。
	oaBodyKeep = 2000
)

// 机器身份来源 / 机器头状态（与原实现字面量逐字一致，面板与诊断口径沿用）。
//
// 两个维度正交：Source 说「身份从哪来」，Headers 说「这一次到底发没发机器头」。
// 原实现曾把两者混用导致自愈分支永不命中（口径分裂 bug），这里分开定义。
const (
	IdentitySourceNative  = "runtime-info" // 官方 UMID 组件给出的真实身份（含其落盘缓存）
	IdentitySourceDerived = "derived"      // 没有真实身份（本包不会发送派生的假机器头）
	MachineHeadersNative  = "native"       // 本次发送了全套六个 cosy-machine* 头
	MachineHeadersOmitted = "omitted"      // 本次一个机器头都没发
)

// oaRetryBackoff 传输失败 / 5xx 重试的基础退避：第 n 次重试前等待 n×该值（原实现 backoff=1.0s）。
// 包级变量只为单元测试缩短等待；生产路径不修改它。
var oaRetryBackoff = time.Second

// oaRequest 一次 openapi 调用的参数。attempts 对应原实现 http_json 的 retries（总尝试次数），
// timeout 是**单次尝试**的上限（原实现 urlopen 的 timeout 也是逐次生效）。
type oaRequest struct {
	method   string
	url      string
	body     []byte
	header   http.Header
	attempts int
	timeout  time.Duration
}

// oaResponse 原始响应（状态码 + 体 + 头），供需要按状态码细分语义的调用方使用
// （例如设备授权轮询把 404 / 202 视为「尚未授权」，而不是错误）。
type oaResponse struct {
	status int
	body   []byte
	header http.Header
}

// oaDo 发送 openapi 请求：传输失败与 5xx 按原实现 _retryable 的口径重试，其余状态立即返回。
//
// 返回约定：2xx → (resp, nil)；非 2xx → (resp, 已分类的 *Error)；连接层失败 → (nil, KindTransport)。
// 非 2xx 时 resp 依然非 nil，调用方可以读原文做业务判定（409 幂等、errorCode 等）。
func (c *Client) oaDo(ctx context.Context, r oaRequest) (*oaResponse, *Error) {
	attempts := r.attempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; ; attempt++ {
		resp, terr := c.oaOnce(ctx, r)
		var e *Error
		switch {
		case terr != nil:
			e = &Error{Kind: KindTransport, Msg: terr.Error()}
		case resp.status < 200 || resp.status >= 300:
			e = oaClassify(resp.status, resp.body, resp.header)
		default:
			return resp, nil
		}
		retryable := terr != nil || resp.status >= 500
		if !retryable || attempt >= attempts || ctx.Err() != nil {
			return resp, e
		}
		if !oaSleep(ctx, oaRetryBackoff*time.Duration(attempt)) {
			return resp, e
		}
	}
}

// oaOnce 单次 HTTP 往返（c.httpc：有总超时、HTTP/1.1、遵循代理环境变量）。
func (c *Client) oaOnce(ctx context.Context, r oaRequest) (*oaResponse, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, r.url, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range r.header {
		req.Header[k] = append([]string(nil), vs...)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oaMaxBody))
	if err != nil {
		return nil, err
	}
	return &oaResponse{status: resp.StatusCode, body: raw, header: resp.Header}, nil
}

// oaSleep 可被 ctx 打断的等待；返回 false 表示 ctx 已结束。d <= 0 立即返回 true。
func oaSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// oaSessionDead12153 业务码 12153 按「独立数字」匹配：裸子串会误命中 trace id、时间戳里的数字串。
var oaSessionDead12153 = regexp.MustCompile(`(^|[^0-9])12153([^0-9]|$)`)

// oaSessionDead 会话死亡标记（上游主动吊销离线会话：刷新已无意义，必须重新登录）。
// 标记集合与原实现 SESSION_DEAD_MARKERS 一致。
func oaSessionDead(s string) bool {
	return strings.Contains(s, "TOKEN_EXPIRE") ||
		strings.Contains(s, "Offline user session not found") ||
		oaSessionDead12153.MatchString(s)
}

// oaClassify 把非 2xx 响应映射为 ErrKind。
//
// 顺序有讲究：会话死亡标记优先于 401/403——同样是 401，「token 过期可刷新」与「会话被吊销
// 必须重登」对调用方是两种完全不同的处置（前者先刷新再试，后者直接停用账号）。
// Msg 采用原实现的 "HTTP <code> <body 前 160 字符>" 口径，日志 / 面板文案因此与原实现一致。
func oaClassify(status int, body []byte, hdr http.Header) *Error {
	text := string(body)
	e := &Error{
		Status: status,
		Msg:    oaHTTPText(status, body),
		Body:   truncate(text, oaBodyKeep),
	}
	e.Code = oaBusinessCode(body)
	switch {
	case status >= 400 && status < 500 && oaSessionDead(text):
		e.Kind = KindSessionDead
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = KindAuth
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimit
		e.RetryAfter = oaRetryAfter(hdr)
	case status >= 500:
		e.Kind = KindServer
	case status >= 400:
		e.Kind = KindBadRequest
	default:
		e.Kind = KindServer
	}
	return e
}

// oaHTTPText 原实现错误文案口径："HTTP %d %s" % (code, body[:160])（按字符截断）。
func oaHTTPText(status int, body []byte) string {
	return fmt.Sprintf("HTTP %d %s", status, oaCut(string(body), 160))
}

// oaBusinessCode 尽力从 JSON 体里取上游业务码（errorCode / code / error_code）。
func oaBusinessCode(body []byte) string {
	m, ok := oaObject(body)
	if !ok {
		return ""
	}
	for _, k := range []string{"errorCode", "code", "error_code"} {
		if s := oaStr(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// oaRetryAfter 解析 Retry-After（秒）；缺失或非法返回 0。
func oaRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// oaURL 区域 openapi 基址 + 路径。
func oaURL(realm, path string) string {
	return strings.TrimRight(ConfigFor(realm).OpenAPI, "/") + path
}

// oaTokenHeaders token 交换 / 刷新 / userinfo 用的最小头集（原实现 _post_token / import_pat 口径）。
// UA 必须是所属区域官方客户端的 UA：Go 默认的 "Go-http-client/x" 等于在每个请求上自报「我是脚本」。
func oaTokenHeaders(realm, bearer string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", ConfigFor(realm).UA)
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	return h
}

// oaPlainHeaders 普通 openapi 业务头（原实现 Account.headers）。
//
// X-Request-ID 每请求随机（官方客户端的请求 id 与账号无关）；X-Machine-ID / X-Session-ID
// 按 UID 稳定派生（一个账号 = 一台虚拟设备，不同账号互相独立，见 fingerprint.go）。
func oaPlainHeaders(cred Cred) http.Header {
	cfg := ConfigFor(cred.Realm)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("User-Agent", cfg.UA)
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set("X-Request-ID", RandomHex(16))
	h.Set("X-Machine-ID", DeriveID(cred.UID, "machine"))
	h.Set("X-Session-ID", DeriveID(cred.UID, "session"))
	h.Set("Origin", cfg.Website)
	h.Set("Referer", cfg.Website+"/")
	return h
}

// identityUse 一次桌面端请求实际用到的机器身份情况（供结果透出与自愈判定）。
type identityUse struct {
	source  string // IdentitySourceNative / IdentitySourceDerived
	headers string // MachineHeadersNative / MachineHeadersOmitted
	err     error  // 取不到真实身份的原因（source=derived 时非 nil）
}

// oaDesktopHeaders 桌面端同款出站头（活动平台 /sash/... 必需；原实现 Account.desktop_headers）。
//
// 两层坑（原实现都踩过）：
//  1. 缺 UA=Qoder / cosy-clienttype / cosy-version → 服务端不报错但返回**空活动列表**；
//  2. 机器头「全套派生」会被判为非官方客户端，把 CLAIMABLE 的「每日领取 100 Credits」整条
//     过滤（issue #10）。所以策略是：有官方组件给出的真实身份 → 发全套六头；
//     没有 → **一个机器头都不发**，只发 UA / clienttype / version。绝不发派生的假身份。
func (c *Client) oaDesktopHeaders(ctx context.Context, cred Cred) (http.Header, identityUse) {
	h := oaPlainHeaders(cred)
	h.Set("User-Agent", oaDesktopUA)
	h.Set("Cosy-Clienttype", oaDesktopClientType)
	h.Set("Cosy-Version", c.opts.DesktopVersion)
	ident, err := c.MachineIdentity(ctx)
	if err != nil || ident == nil || ident.MachineToken == "" {
		if err == nil {
			err = fmt.Errorf("runtime-info 未给出 machineToken")
		}
		return h, identityUse{source: IdentitySourceDerived, headers: MachineHeadersOmitted, err: err}
	}
	h.Set("Cosy-Machineid", DeriveID(cred.UID, "machine"))
	h.Set("Cosy-Machinetoken", ident.MachineToken)
	h.Set("Cosy-Machinetype", ident.MachineType)
	h.Set("Cosy-Machinecode", ident.MachineCode)
	h.Set("Cosy-Machineos", oaDesktopMachineOS)
	h.Set("Cosy-Machinehostname", oaDesktopHostname)
	return h, identityUse{source: IdentitySourceNative, headers: MachineHeadersNative}
}

// ---------------------------------------------------------------------------
// 上游 JSON 的宽松读取（按 Python 语义：上游字段类型会漂移——数字变字符串、布尔变 "false"）
// ---------------------------------------------------------------------------

// oaDecode 解析任意 JSON（数字保留为 json.Number，避免大整数精度丢失、也保留原文形态）。
func oaDecode(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// oaObject 解析 JSON 对象；非对象或解析失败返回 (nil, false)。
func oaObject(body []byte) (map[string]any, bool) {
	v, err := oaDecode(body)
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// oaMap 取对象（非对象返回 nil；对 nil map 取键是安全的）。
func oaMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// oaList 取数组（非数组返回 nil）。
func oaList(v any) []any {
	l, _ := v.([]any)
	return l
}

// oaTruthy Python bool(v) 语义：None / False / 0 / "" / 空容器为假。
// 注意 Python 的 bool("false") 为真——需要防御性布尔解读时用 oaTruthyField。
func oaTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case string:
		return x != ""
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

// oaTruthyField 原实现 _truthy：字符串 "false" / "0" / "no" / "off" / "none" / "null" 也算假；
// v 为 nil（显式 null）时取 def——「缺键」与「字段为 null」的语义由调用方决定。
func oaTruthyField(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "0", "false", "no", "off", "none", "null":
			return false
		}
		return true
	}
	return oaTruthy(v)
}

// oaNum Python float(v or 0) 语义；无法转换按 0（与原实现 _num 的 except 分支一致）。
func oaNum(v any) float64 {
	if !oaTruthy(v) {
		return 0
	}
	switch x := v.(type) {
	case bool:
		return 1
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0
		}
		return f
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// oaInt Python int(v or 0) 的宽松版：小数截断，非法按 0。
func oaInt(v any) int64 {
	return int64(oaNum(v))
}

// oaStr Python str(v or "") 语义：假值为空串，数字保留原文。
func oaStr(v any) string {
	if !oaTruthy(v) {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return "True"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

// normalizeEpoch 原实现 normalize_epoch：秒 / 毫秒自适应（> 1e11 视为毫秒），非法为 0。
func normalizeEpoch(v any) int64 {
	var f float64
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		f = p
	default:
		f = oaNum(v)
	}
	if f > 1e11 {
		f /= 1000
	}
	return int64(f)
}

// oaCut 按字符（rune）截断——原实现 body[:160] 作用在已解码的 str 上，按字节切会切坏中文。
func oaCut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// oaFmtNum 数值的日志口径：整数不带小数点（与原实现打印 int 一致），小数保留原精度。
func oaFmtNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
