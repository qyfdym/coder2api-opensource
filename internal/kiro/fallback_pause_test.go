package kiro

import (
	"context"
	"os"
	"testing"
	"time"
)

// 换入口前的停顿在生产里是几百毫秒；其余测试不关心节奏，统一关掉，专门的用例再临时打开。
func TestMain(m *testing.M) {
	endpointFallbackPause = 0
	os.Exit(m.Run())
}

func TestPauseBeforeNextEndpoint(t *testing.T) {
	old := endpointFallbackPause
	defer func() { endpointFallbackPause = old }()
	endpointFallbackPause = 80 * time.Millisecond

	// 还有下一个入口：停顿在 ±25% 抖动范围内。
	start := time.Now()
	if !pauseBeforeNextEndpoint(context.Background(), 0, 3) {
		t.Fatal("pause must report ok when the context is alive")
	}
	if d := time.Since(start); d < 55*time.Millisecond || d > 400*time.Millisecond {
		t.Errorf("pause = %v, want about 60–100ms", d)
	}

	// 已经是最后一个入口：不停。
	start = time.Now()
	if !pauseBeforeNextEndpoint(context.Background(), 2, 3) || time.Since(start) > 30*time.Millisecond {
		t.Errorf("no pause expected after the last endpoint (took %v)", time.Since(start))
	}

	// 停顿期间 ctx 取消：立即返回 false，不把等待拖满。
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	start = time.Now()
	if pauseBeforeNextEndpoint(ctx, 0, 3) {
		t.Error("a cancelled context must stop the pause")
	}
	if time.Since(start) > 60*time.Millisecond {
		t.Errorf("cancel took %v to be noticed", time.Since(start))
	}
}

// 回退链路上限流的账号：仍然会换入口（额度桶不同），但相邻两次请求之间有间隔。
func TestEndpointFallbackIsPaced(t *testing.T) {
	old := endpointFallbackPause
	defer func() { endpointFallbackPause = old }()
	endpointFallbackPause = 120 * time.Millisecond

	f := newFake()
	f.on(routeIDE, errResp(429, `{"message":"Too many requests"}`))
	f.on(routeCW, errResp(429, `{"message":"Too many requests"}`))
	c := testClient(f)
	start := time.Now()
	rc, err := c.ChatStream(context.Background(), testCred, []byte(`{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rc != nil || err == nil || err.Kind != KindRateLimit {
		t.Fatalf("got rc=%v err=%+v", rc != nil, err)
	}
	if n := len(f.calls(routeCW)); n != 1 {
		t.Errorf("codewhisperer calls=%d, the fallback itself must be kept", n)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("fallback fired after %v, want a pause of about 120ms between endpoints", d)
	}
}
