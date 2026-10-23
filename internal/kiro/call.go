package kiro

// 推理请求（移植自 Kiro-Go proxy/kiro.go 的 CallKiroAPIContext）：按入口偏好依次尝试，
// 首个输出之前的失败在包内重试（同入口一次 + 换入口），之后的失败原样返回。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type endpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

// ideEndpoints OAuth 账号的三个推理入口（顺序 = auto 模式的回退顺序）。
var ideEndpoints = []endpoint{
	{URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", Origin: "AI_EDITOR", Name: "Kiro IDE"},
	{URL: "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", Origin: "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse", Name: "CodeWhisperer"},
	{URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", Origin: "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage", Name: "AmazonQ"},
}

// cliEndpoint API Key 账号的入口（Kiro CLI runtime，AWS JSON 1.0）。
var cliEndpoint = endpoint{Origin: "KIRO_CLI", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse", Name: "Kiro CLI"}

const (
	maxStreamAttemptsPerEndpoint = 2
	streamRetryBackoff           = 700 * time.Millisecond
)

func (c *Client) endpointsFor(cred Cred) []endpoint {
	if cred.IsAPIKey() {
		return []endpoint{cliEndpoint}
	}
	primary := -1
	switch c.opts.Endpoint {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	}
	if primary < 0 {
		return append([]endpoint(nil), ideEndpoints...)
	}
	out := []endpoint{ideEndpoints[primary]}
	for i, ep := range ideEndpoints {
		if i != primary {
			out = append(out, ep)
		}
	}
	return out
}

// noRetryKinds 换入口也不会好的错误：账号或请求本身的问题。
func noRetryKind(k ErrKind) bool {
	switch k {
	case KindQuota, KindSuspended, KindAuth, KindSessionDead, KindTooLarge, KindBadRequest, KindContent, KindModelUnavailable:
		return true
	}
	return false
}

// callUpstream 发起一次推理并把 event-stream 解码到 cb。返回 nil = 正常结束。
func (c *Client) callUpstream(ctx context.Context, cred Cred, p *kiroPayload, cb *streamCallbacks) *Error {
	if cred.bearer() == "" {
		return &Error{Kind: KindAuth, Msg: "账号没有可用的 access token"}
	}
	p.ProfileArn = ""
	if !cred.IsAPIKey() {
		p.ProfileArn = strings.TrimSpace(cred.ProfileArn)
	}
	// 响应里的工具名还原成客户端原名（出站时可能被缩短）。
	if len(p.toolNameMap) > 0 && cb != nil && cb.OnToolUse != nil {
		orig := cb.OnToolUse
		nm := p.toolNameMap
		wrapped := *cb
		wrapped.OnToolUse = func(tu toolUse) {
			if o, ok := nm[tu.Name]; ok {
				tu.Name = o
			}
			orig(tu)
		}
		cb = &wrapped
	}

	isAPIKey := cred.IsAPIKey()
	eps := c.endpointsFor(cred)
	var last *Error
endpoints:
	for epIdx, ep := range eps {
		p.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin
		target := regionalize(ep.URL, dataRegion(cred, p.ProfileArn))
		if isAPIKey {
			target = cliRuntimeURL(cred)
		}
		body, merr := json.Marshal(p)
		if merr != nil {
			return &Error{Kind: KindBadRequest, Msg: "encode request: " + merr.Error()}
		}
		ua, amzUA := c.userAgents(cred, "codewhispererstreaming", streamingSDKVersion, "m/E")
		invocation := newUUID()
		for attempt := 1; attempt <= maxStreamAttemptsPerEndpoint; attempt++ {
			if ctx.Err() != nil {
				return &Error{Kind: KindTransport, Msg: "context canceled", Body: ctx.Err().Error()}
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
			if err != nil {
				return &Error{Kind: KindBadRequest, Msg: err.Error()}
			}
			if isAPIKey {
				req.Header.Set("Content-Type", "application/x-amz-json-1.0")
			} else {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Accept", "*/*")
			if ep.AmzTarget != "" {
				req.Header.Set("X-Amz-Target", ep.AmzTarget)
			}
			applyBaseHeaders(req, cred, ua, amzUA)
			if isAPIKey {
				req.Header.Set("x-amzn-codewhisperer-optout", "false") // Kiro CLI 抓包口径
			} else {
				req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
			}
			req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=%d", attempt, maxStreamAttemptsPerEndpoint))
			req.Header.Set("Amz-Sdk-Invocation-Id", invocation)

			resp, err := c.stream.Do(req)
			if err != nil {
				last = transportError(err)
				if !retryableTransport(err) || ctx.Err() != nil {
					return last
				}
				c.logf("endpoint %s for %s failed: %v", ep.Name, cred.label(), err)
				continue endpoints
			}
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				last = newError(resp.StatusCode, string(raw), resp.Header.Get("Retry-After"))
				// 鉴权 / 付费 / 封禁 / 额度类错误不换入口重试（换了也一样，只会放大请求量）。
				if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 ||
					last.Kind == KindQuota || last.Kind == KindSuspended {
					return last
				}
				c.logf("endpoint %s for %s: HTTP %d, trying next", ep.Name, cred.label(), resp.StatusCode)
				continue endpoints
			}
			emitted, perr := parseEventStream(resp.Body, cb)
			resp.Body.Close()
			if perr == nil {
				return nil
			}
			pe := streamError(perr)
			if ctx.Err() != nil {
				return &Error{Kind: KindTransport, Msg: "context canceled", Body: ctx.Err().Error()}
			}
			// 已触发过输出回调：重试会把两次尝试的输出拼在一起，只能原样返回。
			if emitted || noRetryKind(pe.Kind) {
				return pe
			}
			last = pe
			hasSame := attempt < maxStreamAttemptsPerEndpoint
			hasNext := epIdx+1 < len(eps)
			if !hasSame && !hasNext {
				break endpoints
			}
			c.logf("endpoint %s for %s: stream failed before output (attempt %d/%d): %v",
				ep.Name, cred.label(), attempt, maxStreamAttemptsPerEndpoint, perr)
			if !sleepCtx(ctx, streamRetryBackoff) {
				return &Error{Kind: KindTransport, Msg: "context canceled"}
			}
			if !hasSame {
				continue endpoints
			}
		}
	}
	if last != nil {
		return last
	}
	return &Error{Kind: KindServer, Msg: "all Kiro endpoints failed"}
}

// streamError 解码阶段的错误 → *Error。
func streamError(err error) *Error {
	var ke *Error
	switch {
	case errors.As(err, &ke):
		return ke
	case errors.Is(err, errEmptyStream):
		return &Error{Kind: KindServer, Msg: "Kiro 上游返回了空流", Body: err.Error()}
	case errors.Is(err, errIncompleteToolInput):
		return &Error{Kind: KindTruncated, Msg: friendlyMessage(KindTruncated), Body: err.Error()}
	case errors.Is(err, errInvalidEventStream):
		return &Error{Kind: KindServer, Msg: "Kiro 上游流格式异常", Body: truncate(err.Error(), 300)}
	}
	return transportError(err)
}

// retryableTransport 超时 / 取消不换入口（同一个慢上游换了入口也一样慢），其它传输错误可换。
func retryableTransport(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	return !errors.As(err, &ne) || !ne.Timeout()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
