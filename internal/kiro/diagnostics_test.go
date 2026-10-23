package kiro

// 出站失败要能从日志里看出原因：代理没起来、节点不通、没走代理却拿不到 Claude，三种情况过去长得一样。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type errRT struct{ err error }

func (e errRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestTransportErrorShowsCauseWithoutCredentials(t *testing.T) {
	cause := errors.New(`Post "https://oidc.us-east-1.amazonaws.com/token": proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused`)
	c := NewClient(Options{Transport: errRT{cause}, Logf: func(string, ...any) {}})
	_, kerr := c.Refresh(context.Background(), Cred{AuthMethod: "idc", RefreshToken: "rt", ClientID: "cid", ClientSecret: "cs", Region: "us-east-1"})
	if kerr == nil || kerr.Kind != KindTransport {
		t.Fatalf("err = %+v", kerr)
	}
	msg := kerr.Error()
	if !strings.Contains(msg, "连接 Kiro 上游失败") || !strings.Contains(msg, "connection refused") || !strings.Contains(msg, "127.0.0.1:7890") {
		t.Errorf("Error() must carry the cause: %q", msg)
	}

	leaky := errors.New(`Get "https://q.us-east-1.amazonaws.com/x": proxyconnect tcp: dial failed via http://user:secret@host:8080/p`)
	got := transportError(leaky)
	if strings.Contains(got.Error(), "secret") || strings.Contains(got.Body, "secret") {
		t.Errorf("proxy credentials must be scrubbed: body=%q", got.Body)
	}
	if !strings.Contains(got.Body, "http://***@host:8080/p") {
		t.Errorf("scrubbed body = %q", got.Body)
	}
}

// 目录里一个 Claude 都没有：打一条带当前出口的提示（限频）；有 Claude 不提示。
func TestNoClaudeHint(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	withoutClaude := []Model{{ID: "qwen3-coder-next"}, {ID: "kimi-k3"}}
	withClaude := []Model{{ID: "qwen3-coder-next"}, {ID: "claude-sonnet-4.5"}}

	c := NewClient(Options{Transport: newFake(), ProxyURL: "http://user:pw@127.0.0.1:7890", Logf: logf})
	cred := Cred{UID: "u1", Nickname: "felicia"}
	c.hintNoClaude(cred, withClaude)
	if count("没有 Claude") != 0 {
		t.Fatalf("a catalog with Claude must not warn")
	}
	c.hintNoClaude(cred, withoutClaude)
	c.hintNoClaude(cred, withoutClaude) // 限频：同一账号不重复刷
	if count("没有 Claude") != 1 {
		t.Fatalf("expected exactly one hint, got %d: %v", count("没有 Claude"), logs)
	}
	if count("http://127.0.0.1:7890") != 1 || count("pw") != 0 {
		t.Errorf("hint must name the (masked) egress: %v", logs)
	}
	// 另一个账号各自提示；没配代理时提示「没有走代理」。
	bare := NewClient(Options{Transport: newFake(), Logf: logf})
	bare.hintNoClaude(Cred{UID: "u2"}, withoutClaude)
	if count("没有走代理") != 1 {
		t.Errorf("no-proxy hint missing: %v", logs)
	}
}
