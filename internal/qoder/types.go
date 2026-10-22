// Package qoder 是 Qoder（qoder.com 国际版 / qoder.com.cn 国内版）上游的 Go 原生实现。
//
// 定位：网关统一账号池里的第二种上游。选号、冷却、熔断、粘性会话由网关账号池负责；
// 本包只负责「给定一个 Qoder 账号凭证，如何与 Qoder 官方服务端通信」——COSY 签名、
// 请求体构造、SSE 信封解包为 OpenAI 形态、错误分类、token 刷新、额度与签到。
//
// 本包刻意**不依赖**网关其它内部包（auth / pool / upstream）：凭证以 Cred 值对象传入，
// 刷新结果以返回值交还，持久化与账号池记账由调用方（server / panel 适配层）完成。
// 这样协议实现可以独立测试，也不会因为账号池改动而牵连。
//
// 协议细节移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors），
// 逐字节对齐其签名与请求形态；原项目的 Python 实现是本包行为的参考基准。
package qoder

import (
	"fmt"
	"strings"
	"time"
)

// 区域标识。国内版与国际版是两套独立的账号体系、端点与模型目录。
const (
	RealmCN   = "cn"
	RealmIntl = "intl"
)

// RealmConfig 单个区域的端点与客户端常量（逆向自官方桌面 / CLI 客户端）。
type RealmConfig struct {
	Name    string
	OpenAPI string // 账号 / 额度 / 签到等业务接口基址（纯 Bearer，无 COSY 签名）
	Gateway string // 推理与模型目录基址（COSY 签名）
	// GatewayFallbacks 主推理主机传输失败时依次尝试的备用主机（与官方客户端候选顺序一致）。
	GatewayFallbacks []string
	Website          string
	ClientID         string
	RedirectURI      string
	Domain           string
	UA               string // 账号接口 User-Agent（官方 Electron 客户端口径）
	// 设备授权 URL 形态差异：国内版带 client_id + machine_id + redirect_uri，国际版不带 redirect_uri。
	SendClientID    bool
	SendRedirectURI bool
	NonceDashed     bool // 国内版 nonce 用带横线 uuid，国际版用 32 位 hex
}

var realmConfigs = map[string]RealmConfig{
	RealmCN: {
		Name:            "国内版",
		OpenAPI:         "https://openapi.qoder.com.cn",
		Gateway:         "https://gateway.qoder.com.cn",
		Website:         "https://qoder.com.cn",
		ClientID:        "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		RedirectURI:     "qoder-work-cn://",
		Domain:          "qoder.com.cn",
		UA:              "QoderWork/1.1.64",
		SendClientID:    true,
		SendRedirectURI: true,
		NonceDashed:     true,
	},
	RealmIntl: {
		Name:             "国际版",
		OpenAPI:          "https://openapi.qoder.sh",
		Gateway:          "https://api3.qoder.sh",
		GatewayFallbacks: []string{"https://api2.qoder.sh", "https://api1.qoder.sh"},
		Website:          "https://qoder.com",
		ClientID:         "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		RedirectURI:      "qoder://aicoding.aicoding-agent/login-success",
		Domain:           "qoder.com",
		UA:               "Qoder/1.1.64",
		SendClientID:     true,
		SendRedirectURI:  false,
		NonceDashed:      false,
	},
}

// NormalizeRealm 把外部输入归一为 cn / intl；未知值回落 cn（与原实现一致）。
func NormalizeRealm(realm string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case RealmIntl, "global", "international":
		return RealmIntl
	default:
		return RealmCN
	}
}

// ConfigFor 返回区域配置（未知区域按国内版）。
func ConfigFor(realm string) RealmConfig {
	return realmConfigs[NormalizeRealm(realm)]
}

// RealmFromDomain 按账号 domain 推断区域（qoder.com.cn → cn，其余 qoder.* → intl）。
func RealmFromDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case d == "":
		return RealmCN
	case strings.HasSuffix(d, "qoder.com.cn"):
		return RealmCN
	case strings.Contains(d, "qoder"):
		return RealmIntl
	default:
		return RealmCN
	}
}

// DefaultUserType 官方身份体缺省用户类型（与原实现一致）。
const DefaultUserType = "personal_professional_trial"

// Cred 单个 Qoder 账号调用上游所需的凭证快照。
//
// 由调用方从持久化账号构造并按值传入；本包从不持久化、也不缓存 Cred 本身
// （COSY 会话按 UID+AccessToken 缓存，token 一变即重建）。
type Cred struct {
	UID          string
	Nickname     string
	Realm        string // cn / intl
	AccessToken  string // dt-（设备授权，约 30 天）或 jt-（PAT 交换，24 小时）
	RefreshToken string // drt-（约 1 年，每次刷新都会换新）或 jrt-（48 小时）
	PAT          string // pt- 个人访问令牌（长期；只作刷新失败时的兜底）
	ExpiresAt    int64  // access token 到期（Unix 秒；0 = 未知）
	UserType     string
	OrgID        string
	OrgName      string
}

// TokenSet 刷新 / 交换 / 授权得到的新凭证（调用方负责写回并落盘）。
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
}

// ErrKind 上游错误类别。调用方据此决定冷却 / 停用 / 换号 / 直接回客户端，
// 并映射到网关账号池的统一错误策略。
type ErrKind int

const (
	KindNone             ErrKind = iota
	KindTransport                // 连接层失败（DNS / TLS / 重置 / 超时）
	KindQueued                   // 上游排队（403 isQueued / 10605 / RESOURCE_LIMIT）：等待后可重试
	KindRateLimit                // 账号级 429 限流
	KindSessionDead              // 会话失效（TOKEN_EXPIRE / 12153 / Offline user session not found）：需重新登录
	KindAuth                     // 401/403 等鉴权失败（非会话死亡；可先刷新 token 再试）
	KindQuota                    // 额度耗尽 / 超出限额
	KindBadRequest               // 请求参数非法（同一请求换号也不会成功）
	KindContent                  // 内容安全审核拒绝
	KindVersion                  // 430 客户端版本过低
	KindModelUnavailable         // 模型不存在 / 该账号无权限 / 已下线
	KindTooLarge                 // 请求体超过上游上限
	KindServer                   // 5xx / 418 / 未知服务端错误
)

func (k ErrKind) String() string {
	switch k {
	case KindTransport:
		return "transport"
	case KindQueued:
		return "queued"
	case KindRateLimit:
		return "rate_limit"
	case KindSessionDead:
		return "session_dead"
	case KindAuth:
		return "auth"
	case KindQuota:
		return "quota"
	case KindBadRequest:
		return "bad_request"
	case KindContent:
		return "content"
	case KindVersion:
		return "version"
	case KindModelUnavailable:
		return "model_unavailable"
	case KindTooLarge:
		return "too_large"
	case KindServer:
		return "server"
	default:
		return "none"
	}
}

// Error 上游错误信封。Body 保留上游原文（截断后），供透传给客户端排障。
type Error struct {
	Kind       ErrKind
	Status     int    // HTTP 状态或 SSE 信封 statusCodeValue；0 = 无
	Code       string // 上游业务码（如 "10605" / "TOKEN_EXPIRE"）；可能为空
	Msg        string // 可读描述
	Body       string // 上游原文（截断）
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := "qoder " + e.Kind.String()
	if e.Status != 0 {
		s += fmt.Sprintf(" (status %d)", e.Status)
	}
	if e.Code != "" {
		s += " [" + e.Code + "]"
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// truncate 截断字符串用于日志 / 错误信封（按字节，保证不切坏 UTF-8 由调用方容忍）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
