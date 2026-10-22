package qoder

// 上游错误分类与 error 帧往返。移植自 qoder2api-hub（MIT）qoder_proxy.py 的
// friendly_upstream_error / _is_request_fault / _is_transient_* / session_dead
// 与 _queue_wait_seconds，按网关统一错误口径收敛成 ErrKind。

import (
	"regexp"
	"strconv"
	"strings"
)

// 会话死亡标记：上游吊销离线会话，刷新已无意义，需要重新登录（qoder_accounts.py 原样）。
var sessionDeadMarkers = []string{"TOKEN_EXPIRE", "12153", "Offline user session not found"}

func sessionDead(detail string) bool {
	for _, m := range sessionDeadMarkers {
		if strings.Contains(detail, m) {
			return true
		}
	}
	return false
}

// 内容安全审核标记（确定性拒绝，重试无效）。
var contentPolicyMarkers = []string{
	"DataInspectionFailed", "inappropriate content", "input text data may contain",
	"ContentFilter", "SensitiveContent",
}

// 请求参数自身的确定性错误（换号结果不变）。
var requestFaultMarkers = []string{
	"invalid_parameter_error", "invalid_request_error", `"Range of `,
}

// 额度耗尽标记（原实现在额度护栏/账号侧判定；chat 路径best-effort 收敛）。
var quotaMarkers = []string{
	"insufficient_quota", "insufficient quota", "quota exceeded", "QuotaExceeded",
	"credits exhausted", "credit_exhausted", "余额不足", "额度不足", "额度已用完",
	"out of credits", "balance", "欠费",
}

// 模型不可用标记（不存在 / 无权限 / 已下线）。
var modelUnavailableMarkers = []string{
	"model not found", "model_not_found", "ModelNotFound", "not_supported_model",
	"model is not available", "model unavailable", "unknown model", "模型不存在",
	"模型不可用", "no such model",
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

var retryAfterRe = regexp.MustCompile(`retryAfterSeconds\D+(\d+)`)

// queueWaitSeconds 从 403/排队响应体解析等待秒数；不是排队信号返回 0。
// 排队信号：detail 含 10605 / isQueued / queue（大小写不敏感）。
func queueWaitSeconds(detail string) int {
	low := strings.ToLower(detail)
	if !strings.Contains(detail, "10605") && !strings.Contains(low, "isqueued") &&
		!strings.Contains(low, "queue") {
		return 0
	}
	if m := retryAfterRe.FindStringSubmatch(detail); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return 30 // 排队但没给时长：按 30s 保守处理（原实现口径）
}

// isQueueSignal 该 detail 是否是上游排队/资源受限信号。
func isQueueSignal(detail string) bool {
	low := strings.ToLower(detail)
	return strings.Contains(detail, "10605") || strings.Contains(low, "isqueued") ||
		strings.Contains(detail, "RESOURCE_LIMIT")
}

// classifyUpstream 把一次上游失败（HTTP 状态或信封 statusCodeValue + 原文）
// 归类为 ErrKind。顺序：排队 → 版本门 → 会话死亡 → 内容审核 → 额度 → 参数 →
// 模型不可用 → 鉴权/限流 → 瞬时服务端。
func classifyUpstream(status int, detail string) ErrKind {
	d := detail
	if isQueueSignal(d) {
		return KindQueued
	}
	if status == 430 || strings.Contains(d, "430") && strings.Contains(strings.ToLower(d), "version") {
		return KindVersion
	}
	if sessionDead(d) {
		return KindSessionDead
	}
	if containsAny(d, contentPolicyMarkers) {
		return KindContent
	}
	if containsAny(d, quotaMarkers) {
		return KindQuota
	}
	if containsAny(d, requestFaultMarkers) {
		return KindBadRequest
	}
	if containsAny(d, modelUnavailableMarkers) {
		return KindModelUnavailable
	}
	switch status {
	case 429:
		return KindRateLimit
	case 413:
		return KindTooLarge
	case 401, 403:
		return KindAuth
	case 430:
		return KindVersion
	}
	if status == 418 || status >= 500 {
		return KindServer
	}
	if status >= 400 && status < 500 {
		// 其余 4xx：带 provider_error 视为瞬时服务端，否则按请求错误。
		if strings.Contains(d, "provider_error") {
			return KindServer
		}
		return KindBadRequest
	}
	return KindServer
}

// friendlyMessage 为某类错误给出可读说明（透传上游原文由调用方附加）。
func friendlyMessage(kind ErrKind, status int, detail string) string {
	switch kind {
	case KindVersion:
		return "客户端/网关版本过低（430）：上游要求升级后才能继续，与账号或网络无关"
	case KindTooLarge:
		return "请求体过大，超过上游上限，已在本地拦下、未发送"
	case KindQueued:
		return "上游排队中（10605 / RESOURCE_LIMIT），已按 retryAfterSeconds 等待并重试仍无可用资源"
	case KindContent:
		return "上游内容安全审核未通过（DataInspectionFailed）：输入可能含不当内容，属确定性拒绝"
	case KindSessionDead:
		return "会话失效（TOKEN_EXPIRE / 12153）：需要重新登录"
	case KindRateLimit:
		return "账号级限流（429）"
	case KindQuota:
		return "额度耗尽 / 超出限额"
	case KindModelUnavailable:
		return "模型不存在、无权限或已下线"
	case KindBadRequest:
		return "请求参数非法（换号也不会成功）"
	case KindAuth:
		return "鉴权失败（HTTP " + strconv.Itoa(status) + "）"
	case KindTransport:
		return "连接上游失败"
	default:
		if status != 0 {
			return "上游错误（HTTP " + strconv.Itoa(status) + "）"
		}
		return "上游错误"
	}
}

// errKindCode 错误帧里 type 的「qoder_」后缀值（即 ErrKind.String()）→ ErrKind 的反查表。
var errKindByString = func() map[string]ErrKind {
	m := map[string]ErrKind{}
	for k := KindNone; k <= KindServer; k++ {
		m[k.String()] = k
	}
	return m
}()

// errorFramePayload 构造 error 帧的 JSON（保序：message / type / code）。
// type = "qoder_<kind 字符串>"；code 为上游业务码，可空（空则省略该键）。
func errorFramePayload(e *Error) string {
	msg := e.Msg
	if e.Body != "" {
		if msg != "" {
			msg += ": " + e.Body
		} else {
			msg = e.Body
		}
	}
	inner := jObjV("message", msg, "type", "qoder_"+e.Kind.String())
	if e.Code != "" {
		inner.set("code", jStrV(e.Code))
	}
	return jObjV("error", inner).dumpCompact()
}

// KindFromErrorFrame 解析 ChatStream 输出的 error 帧 data 内容，还原 ErrKind。
//
// 入参既可带 "data:" 前缀也可是裸 JSON；取 error.type，去掉 "qoder_" 前缀后反查
// ErrKind.String()。无法识别时返回 KindServer（与分类兜底一致）。
func KindFromErrorFrame(payload string) ErrKind {
	data := stripDataPrefix(payload)
	if data == "" {
		data = strings.TrimSpace(payload)
	}
	v, err := parseJString(data)
	if err != nil || !v.isObj() {
		return KindServer
	}
	errObj := v.get("error")
	if !errObj.isObj() {
		return KindServer
	}
	typ := errObj.get("type").str()
	typ = strings.TrimPrefix(typ, "qoder_")
	if k, ok := errKindByString[typ]; ok {
		return k
	}
	return KindServer
}

// upstreamStatus 信封 statusCodeValue != 200，或协议级失败（工具参数非法）。
// protocol=true 时不进入连接层重试，直接作为 error 帧输出。
type upstreamStatus struct {
	status   int
	detail   string
	protocol bool
	code     string
	kind     ErrKind
}

func (e *upstreamStatus) Error() string {
	return "qoder upstream status " + strconv.Itoa(e.status) + ": " + truncate(e.detail, 200)
}

func newUpstreamStatus(status int, detail string) *upstreamStatus {
	return &upstreamStatus{status: status, detail: detail}
}

// invalidToolArgs 工具调用回声内层 arguments 非法（issue #37）：fail-closed。
func invalidToolArgs() *upstreamStatus {
	return &upstreamStatus{status: 502, protocol: true, kind: KindBadRequest,
		code:   "invalid_tool_arguments",
		detail: "upstream tool-call echo contains invalid JSON arguments; no tools were emitted"}
}

// emptyToolEcho 只有残缺工具回声、无可用调用（issue #72）：fail-closed。
func emptyToolEcho(chars int, kind string) *upstreamStatus {
	return &upstreamStatus{status: 502, protocol: true, kind: KindBadRequest,
		code: "empty_tool_echo",
		detail: "upstream returned only a truncated tool-call echo (no usable call) [" +
			strconv.Itoa(chars) + " chars, " + kind + "]"}
}
