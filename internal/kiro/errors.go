package kiro

import (
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 上游原文里的特征串（小写比较）。判定顺序见 classify：先认「账号本身出了问题」的强信号
// （封禁 / 额度），再认请求问题（过大 / 模型 / 参数），最后才按 HTTP 状态兜底。
var (
	suspendedMarkers = []string{
		"temporarily_suspended", "temporarily suspended", "temporarily is suspended",
		"account suspended", "account has been suspended", "account is suspended",
	}
	quotaMarkers = []string{
		"monthly_request_count", "reached the limit", "reached your limit", "usage limit",
		"limit has been reached", "insufficient credits", "no remaining", "overage",
		"monthly limit", "free tier limit",
	}
	tooLargeMarkers = []string{
		"content_length_exceeds_threshold", "input is too long", "prompt is too long",
		"context length", "context_window_exceeded", "contentlengthexceeded",
	}
	modelMarkers = []string{
		"invalid_model_id", "model is not supported", "unsupported model", "model not found",
		"model is not available", "is not available for this", "invalid model",
	}
	busyMarkers = []string{
		"insufficient_model_capacity", "model is currently experiencing high", "high load",
		"service is temporarily unavailable", "serviceunavailable",
	}
	throttleMarkers = []string{
		"throttlingexception", "throttling", "too many requests", "rate exceeded", "rate limit",
	}
	contentMarkers = []string{
		"guardrail", "content filter", "content_filter", "harmful content", "violates",
	}
	sessionDeadMarkers = []string{
		"invalid_grant", "invalidgrantexception", "invalid refresh token", "refresh token is invalid",
		"refresh token expired", "refresh token has expired", "unauthorizedclientexception",
		"invalidclientexception", "expiredtokenexception", "bad credentials", "invalid_client",
	}
	authMarkers = []string{
		"unauthorized", "forbidden", "accessdenied", "access denied", "invalid bearer",
		"token expired", "token has expired", "the security token", "invalid token",
	}
)

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// classify 按 HTTP 状态 + 上游原文判定错误类别（status=0 表示流内异常帧）。
func classify(status int, detail string) ErrKind {
	d := strings.ToLower(detail)
	switch {
	case containsAny(d, suspendedMarkers):
		return KindSuspended
	case status == 402 || containsAny(d, quotaMarkers):
		return KindQuota
	case status == 413 || containsAny(d, tooLargeMarkers):
		return KindTooLarge
	case containsAny(d, modelMarkers):
		return KindModelUnavailable
	case containsAny(d, busyMarkers):
		return KindBusy
	case status == 429 || containsAny(d, throttleMarkers):
		return KindRateLimit
	case containsAny(d, contentMarkers):
		return KindContent
	case status == 401 || status == 403:
		return KindAuth
	case status == 0 && containsAny(d, authMarkers):
		return KindAuth
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindBadRequest
	case strings.Contains(d, "validationexception") || strings.Contains(d, "improperly formed"):
		return KindBadRequest
	}
	return KindServer
}

// classifyRefresh 刷新接口的错误判定：4xx 带凭证失效特征 = 需要重新登录。
func classifyRefresh(status int, detail string) ErrKind {
	d := strings.ToLower(detail)
	switch {
	case containsAny(d, suspendedMarkers):
		return KindSuspended
	case status >= 400 && status < 500 && containsAny(d, sessionDeadMarkers):
		return KindSessionDead
	case status == 401:
		return KindSessionDead
	case status == 429:
		return KindRateLimit
	case status >= 500 || status == 0:
		return KindServer
	case status == 403:
		return KindAuth
	}
	return KindBadRequest
}

// friendlyMessage 某类错误的可读说明（上游原文由调用方另外附上）。
func friendlyMessage(kind ErrKind) string {
	switch kind {
	case KindTransport:
		return "连接 Kiro 上游失败"
	case KindRateLimit:
		return "Kiro 账号被限流（429），稍后再试"
	case KindAuth:
		return "Kiro 拒绝了当前凭证（401/403），将刷新 token 后重试"
	case KindSessionDead:
		return "Kiro 登录已失效，需要重新登录该账号"
	case KindSuspended:
		return "Kiro 账号被临时封禁（TEMPORARILY_SUSPENDED）"
	case KindQuota:
		return "Kiro 账号额度已用完"
	case KindBadRequest:
		return "Kiro 拒绝了请求参数"
	case KindTooLarge:
		return "请求内容超过 Kiro 的输入上限"
	case KindContent:
		return "请求被 Kiro 内容安全拦截"
	case KindModelUnavailable:
		return "该 Kiro 账号不能使用这个模型"
	case KindBusy:
		return "Kiro 模型容量不足，稍后再试"
	case KindTruncated:
		return "Kiro 上游流在没有结束信号的情况下中断"
	default:
		return "Kiro 上游服务端错误"
	}
}

// newError 由 HTTP 状态 + 原文构造错误信封。
func newError(status int, detail string, hdrRetryAfter string) *Error {
	kind := classify(status, detail)
	e := &Error{Kind: kind, Status: status, Code: errorCode(detail), Msg: friendlyMessage(kind), Body: truncate(detail, 2000)}
	if s := strings.TrimSpace(hdrRetryAfter); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// errorCode 从上游原文取异常类型 / 原因（"__type" / "reason" / "code"），取不到返回空。
func errorCode(detail string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(detail)), &obj) != nil {
		return ""
	}
	for _, k := range []string{"reason", "__type", "code", "errorCode"} {
		if v, ok := obj[k].(string); ok && v != "" {
			if i := strings.LastIndexByte(v, '#'); i >= 0 {
				v = v[i+1:]
			}
			return v
		}
	}
	return ""
}

// urlCredentials 错误文本里可能出现的 scheme://user:pass@host 形式的凭据。
var urlCredentials = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`)

// scrubCredentials 去掉错误文本里 URL 带的用户名密码（代理地址可能带，不能进日志 / 面板 / 响应）。
func scrubCredentials(s string) string { return urlCredentials.ReplaceAllString(s, "${1}***@") }

// transportError 传输层失败的信封。原因（拨号被拒绝 / 代理隧道失败 / 超时……）保留在 Body 里，
// Error() 会带上它——只写「连接 Kiro 上游失败」的话，代理没起来、节点不通、DNS 失败看起来一模一样。
func transportError(err error) *Error {
	return &Error{Kind: KindTransport, Msg: friendlyMessage(KindTransport), Body: scrubCredentials(truncate(err.Error(), 500))}
}

// asError 把任意错误转成 *Error（已经是 *Error 的原样返回）。
func asError(err error) *Error {
	if err == nil {
		return nil
	}
	var ke *Error
	if errors.As(err, &ke) {
		return ke
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return transportError(err)
	}
	return &Error{Kind: KindTransport, Msg: friendlyMessage(KindTransport), Body: truncate(err.Error(), 300)}
}

// errorFramePayload error 帧的 JSON：{"error":{"message":…,"type":"kiro_<kind>","code":…}}。
func errorFramePayload(e *Error) string {
	msg := e.Msg
	if e.Body != "" {
		if msg != "" {
			msg += ": " + e.Body
		} else {
			msg = e.Body
		}
	}
	inner := map[string]any{"message": msg, "type": "kiro_" + e.Kind.String()}
	if e.Code != "" {
		inner["code"] = e.Code
	}
	raw, _ := json.Marshal(map[string]any{"error": inner})
	return string(raw)
}

// KindFromErrorFrame 解析 ChatStream 输出的 error 帧（可带 "data:" 前缀），还原 ErrKind。
// 无法识别时返回 KindServer。
func KindFromErrorFrame(payload string) ErrKind {
	data := strings.TrimSpace(payload)
	data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
	var obj struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &obj) != nil {
		return KindServer
	}
	if k, ok := errKindByString[strings.TrimPrefix(obj.Error.Type, "kiro_")]; ok {
		return k
	}
	return KindServer
}
