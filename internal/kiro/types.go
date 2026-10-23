// Package kiro 是 Kiro（AWS CodeWhisperer / Amazon Q Developer）平台的 Go 原生协议实现。
//
// 协议细节移植自 Kiro-Go（https://github.com/Quorinex/Kiro-Go，MIT，版权与许可声明见
// 仓库 LICENSE 末尾）：generateAssistantResponse 请求体构造（OpenAI → conversationState）、
// AWS event-stream 二进制帧解码、工具调用分片拼接、思考内容分离、token 刷新（OIDC /
// Kiro 桌面认证）、Builder ID / IAM Identity Center 设备授权登录、getUsageLimits /
// ListAvailableModels / ListAvailableProfiles。
//
// 与网关的契约（与 internal/qoder 相同）：
//   - ChatStream 输入 OpenAI chat.completions 请求体，输出**永远是 OpenAI SSE**（流尾带
//     usage 帧和 [DONE]），网关的流式透传 / 非流式聚合 / 用量统计照常工作；
//   - 首个有效输出之前的失败返回 (nil, *Error)，网关据 Kind 处置账号并换号；
//   - 已有输出之后的失败以 error 帧收尾（type = "kiro_<kind>"），KindFromErrorFrame 可还原。
//
// 本包不碰账号池与凭证落盘：凭证以 Cred 值传入，刷新结果以 TokenSet 返回，
// 落盘与并发刷新锁在 internal/kiroacct。
package kiro

import (
	"fmt"
	"strings"
	"time"
)

// Cred 一个 Kiro 账号的调用凭证快照。
type Cred struct {
	UID      string
	Nickname string

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒；API Key 账号为 0

	// AuthMethod social / idc / api_key。
	AuthMethod string
	// Provider BuilderId / Enterprise / Google / Github / APIKey。
	Provider string
	// Region OIDC 认证区域；API Key 账号是数据面区域。
	Region       string
	ClientID     string
	ClientSecret string
	StartURL     string
	// ProfileArn 数据面 profile（决定请求打到哪个区域）。
	ProfileArn string
	MachineID  string
	// APIKey ksk_ 密钥（api_key 账号）。
	APIKey string
}

// IsAPIKey 报告该凭证是否为 Kiro API Key（走 Kiro CLI runtime，不刷新、不带 profileArn）。
func (c Cred) IsAPIKey() bool {
	if strings.TrimSpace(c.APIKey) != "" {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(c.AuthMethod))
	return m == "api_key" || m == "apikey"
}

// bearer Authorization: Bearer 的取值（API Key 账号用密钥，其余用 access token）。
func (c Cred) bearer() string {
	if c.IsAPIKey() {
		if k := strings.TrimSpace(c.APIKey); k != "" {
			return k
		}
	}
	return strings.TrimSpace(c.AccessToken)
}

// label 日志用的账号标识（昵称优先，不含任何凭证）。
func (c Cred) label() string {
	switch {
	case c.Nickname != "":
		return c.Nickname
	case len(c.UID) > 8:
		return c.UID[:8]
	default:
		return c.UID
	}
}

// TokenSet 刷新 / 登录得到的凭证。
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	ProfileArn   string
}

// ErrKind 上游错误类别。网关据此决定冷却 / 停用 / 换号 / 直接回客户端。
type ErrKind int

const (
	KindNone             ErrKind = iota
	KindTransport                // 连接层失败（DNS / TLS / 重置 / 超时）
	KindRateLimit                // 429 / ThrottlingException：账号级限流
	KindAuth                     // 401 / 403（非封禁）：access token 失效，刷新后可再试
	KindSessionDead              // refresh token 失效 / OIDC 客户端过期：需要重新登录
	KindSuspended                // 账号被临时封禁（TEMPORARILY_SUSPENDED）
	KindQuota                    // 额度耗尽（402 / 月度额度用完）
	KindBadRequest               // 请求参数非法（同一请求换号也不会成功）
	KindTooLarge                 // 请求体超过上游上限（Input is too long）
	KindContent                  // 内容安全拦截
	KindModelUnavailable         // 模型不存在 / 该账号无权使用
	KindBusy                     // 上游模型容量不足（INSUFFICIENT_MODEL_CAPACITY）
	KindTruncated                // 流在没有结束信号的情况下中断（上游抖动，不罚号）
	KindServer                   // 5xx / 未知服务端错误
)

func (k ErrKind) String() string {
	switch k {
	case KindTransport:
		return "transport"
	case KindRateLimit:
		return "rate_limit"
	case KindAuth:
		return "auth"
	case KindSessionDead:
		return "session_dead"
	case KindSuspended:
		return "suspended"
	case KindQuota:
		return "quota"
	case KindBadRequest:
		return "bad_request"
	case KindTooLarge:
		return "too_large"
	case KindContent:
		return "content"
	case KindModelUnavailable:
		return "model_unavailable"
	case KindBusy:
		return "busy"
	case KindTruncated:
		return "truncated"
	case KindServer:
		return "server"
	default:
		return "none"
	}
}

var errKindByString = func() map[string]ErrKind {
	m := map[string]ErrKind{}
	for k := KindTransport; k <= KindServer; k++ {
		m[k.String()] = k
	}
	return m
}()

// Error 上游错误信封。Body 保留上游原文（截断后），供透传给客户端排障。
type Error struct {
	Kind       ErrKind
	Status     int    // HTTP 状态；0 = 无（流内异常帧 / 传输层）
	Code       string // 上游异常类型或业务原因（如 ThrottlingException / MONTHLY_REQUEST_COUNT）
	Msg        string // 可读描述
	Body       string // 上游原文（截断）
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := "kiro " + e.Kind.String()
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

// truncate 按字节截断（用于日志 / 错误信封）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
