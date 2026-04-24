package app

import (
	"context"
	"testing"
	"time"
)

func TestResolveTimeouts_GPTImageStreaming(t *testing.T) {
	s := &Server{
		firstByteTimeout:          30 * time.Second,
		nonStreamTimeout:          600 * time.Second, // 模拟用户"10分钟"
		gptImageFirstByteTimeout:  30 * time.Second,
		gptImageUpstreamTimeout:   90 * time.Second,
		gptImageStreamIdleTimeout: 45 * time.Second,
	}

	d := s.resolveTimeouts(true, "gpt-image-1", nil)
	if d.firstByteTimeout != 30*time.Second {
		t.Errorf("gpt-image streaming TTFB: want 30s, got %v", d.firstByteTimeout)
	}
	if d.streamIdleTimeout != 45*time.Second {
		t.Errorf("gpt-image streaming idle: want 45s, got %v", d.streamIdleTimeout)
	}
	if d.overallTimeout != 0 {
		t.Errorf("streaming should not set overallTimeout, got %v", d.overallTimeout)
	}
}

func TestResolveTimeouts_GPTImageNonStreaming(t *testing.T) {
	s := &Server{
		firstByteTimeout:          30 * time.Second,
		nonStreamTimeout:          600 * time.Second,
		gptImageFirstByteTimeout:  30 * time.Second,
		gptImageUpstreamTimeout:   90 * time.Second,
		gptImageStreamIdleTimeout: 45 * time.Second,
	}

	d := s.resolveTimeouts(false, "gpt-image-1", nil)
	if d.overallTimeout != 90*time.Second {
		t.Errorf("gpt-image non-stream overall: want 90s (shorter override), got %v", d.overallTimeout)
	}
	if d.firstByteTimeout != 30*time.Second {
		t.Errorf("gpt-image non-stream TTFB: want 30s, got %v", d.firstByteTimeout)
	}
	if d.streamIdleTimeout != 0 {
		t.Errorf("non-stream should not set idle timeout, got %v", d.streamIdleTimeout)
	}
}

func TestResolveTimeouts_GPTImageUpstreamZeroMeansInherit(t *testing.T) {
	// upstream_timeout=0 应退化到 nonStreamTimeout
	s := &Server{
		nonStreamTimeout:         600 * time.Second,
		gptImageFirstByteTimeout: 30 * time.Second,
		gptImageUpstreamTimeout:  0, // 用户显式关掉专用上限
	}
	d := s.resolveTimeouts(false, "gpt-image-1", nil)
	if d.overallTimeout != 600*time.Second {
		t.Errorf("gpt-image upstream=0 should inherit nonStreamTimeout 600s, got %v", d.overallTimeout)
	}
}

func TestResolveTimeouts_NormalModelUnaffected(t *testing.T) {
	s := &Server{
		firstByteTimeout:          30 * time.Second,
		nonStreamTimeout:          120 * time.Second,
		gptImageFirstByteTimeout:  30 * time.Second,
		gptImageUpstreamTimeout:   90 * time.Second,
		gptImageStreamIdleTimeout: 45 * time.Second,
	}

	// 普通文字模型非流式：保留 nonStreamTimeout，不挂 TTFB
	d := s.resolveTimeouts(false, "gpt-4o", nil)
	if d.overallTimeout != 120*time.Second {
		t.Errorf("gpt-4o non-stream: want inherit 120s, got %v", d.overallTimeout)
	}
	if d.firstByteTimeout != 0 {
		t.Errorf("gpt-4o non-stream should have no TTFB, got %v", d.firstByteTimeout)
	}

	// 普通流式：走 modelFirstByteTimeout 的既定分级，不受 gpt-image 影响
	d = s.resolveTimeouts(true, "gpt-4o", nil)
	if d.firstByteTimeout != 30*time.Second {
		t.Errorf("gpt-4o streaming TTFB should be 30s default, got %v", d.firstByteTimeout)
	}
	if d.streamIdleTimeout != 0 {
		t.Errorf("gpt-4o streaming should have no idle (streamIdleTimeout=0 globally), got %v", d.streamIdleTimeout)
	}
}

func TestResolveTimeouts_SlowModelStillSlow(t *testing.T) {
	// claude-opus 流式 TTFB 应该是 60s（走慢模型分支），不被 gpt-image 干扰
	s := &Server{firstByteTimeout: 30 * time.Second}
	d := s.resolveTimeouts(true, "claude-opus-4-1", nil)
	if d.firstByteTimeout != 60*time.Second {
		t.Errorf("claude-opus streaming TTFB: want 60s, got %v", d.firstByteTimeout)
	}
}

func TestStreamIdleWatchdog_FiresOnInactivity(t *testing.T) {
	s := &Server{
		firstByteTimeout:          30 * time.Second,
		gptImageFirstByteTimeout:  30 * time.Second,
		gptImageStreamIdleTimeout: 50 * time.Millisecond, // 极短便于测试
	}

	// 流式 gpt-image 请求
	body := []byte(`{"model":"gpt-image-1","stream":true}`)
	reqCtx := s.newRequestContext(context.Background(), "/v1/responses", body, "gpt-image-1")
	defer reqCtx.cleanup()

	var expireCalled bool
	reqCtx.startStreamIdleWatchdog(func() {
		expireCalled = true
	})

	// 等待 timer 自然过期
	time.Sleep(120 * time.Millisecond)

	if !reqCtx.streamIdleTimeoutTriggered() {
		t.Fatal("expected streamIdleFired=true after timer expiry")
	}
	if !expireCalled {
		t.Fatal("expected onExpire callback to be called")
	}
}

func TestStreamIdleWatchdog_ResetKeepsAlive(t *testing.T) {
	// 模拟：每 20ms 一次字节进展，idle 阈值 50ms，连续 5 次进展后仍应存活
	s := &Server{
		gptImageFirstByteTimeout:  30 * time.Second,
		gptImageStreamIdleTimeout: 50 * time.Millisecond,
	}

	body := []byte(`{"model":"gpt-image-1","stream":true}`)
	reqCtx := s.newRequestContext(context.Background(), "/v1/responses", body, "gpt-image-1")
	defer reqCtx.cleanup()

	reqCtx.startStreamIdleWatchdog(func() {})

	for range 5 {
		time.Sleep(20 * time.Millisecond)
		reqCtx.resetStreamIdleWatchdog()
	}

	// 总共 100ms，但每 20ms reset 一次，不应触发
	if reqCtx.streamIdleTimeoutTriggered() {
		t.Fatal("idle should NOT trigger while progress keeps resetting timer")
	}

	// 现在停止 reset，让它过期
	time.Sleep(120 * time.Millisecond)
	if !reqCtx.streamIdleTimeoutTriggered() {
		t.Fatal("idle should trigger after reset stops")
	}
}

func TestStreamIdleWatchdog_Disabled(t *testing.T) {
	// 非 gpt-image 且全局 streamIdleTimeout=0：不启动 idle watchdog
	s := &Server{
		firstByteTimeout:  30 * time.Second,
		streamIdleTimeout: 0,
	}

	body := []byte(`{"model":"gpt-4o","stream":true}`)
	reqCtx := s.newRequestContext(context.Background(), "/v1/chat/completions", body, "gpt-4o")
	defer reqCtx.cleanup()

	// startStreamIdleWatchdog 在 streamIdleTimeout<=0 时是 no-op
	reqCtx.startStreamIdleWatchdog(func() {
		t.Error("onExpire must not fire when idle watchdog is disabled")
	})

	time.Sleep(30 * time.Millisecond)
	if reqCtx.streamIdleTimeoutTriggered() {
		t.Fatal("idle should not trigger when streamIdleTimeout=0")
	}
}

func TestNewRequestContext_GPTImageNonStreamTTFBFires(t *testing.T) {
	s := &Server{
		firstByteTimeout:         30 * time.Second,
		nonStreamTimeout:         600 * time.Second,
		gptImageFirstByteTimeout: 50 * time.Millisecond,
		gptImageUpstreamTimeout:  90 * time.Second,
	}

	body := []byte(`{"model":"gpt-image-1"}`) // 非流式：无 stream:true
	reqCtx := s.newRequestContext(context.Background(), "/v1/responses", body, "gpt-image-1")
	defer reqCtx.cleanup()

	if reqCtx.isStreaming {
		t.Fatal("request should be non-streaming")
	}
	if reqCtx.firstByteTimeoutUsed != 50*time.Millisecond {
		t.Errorf("expected firstByteTimeoutUsed=50ms, got %v", reqCtx.firstByteTimeoutUsed)
	}

	// 等 TTFB 超时触发
	time.Sleep(120 * time.Millisecond)
	if !reqCtx.firstByteTimeoutTriggered() {
		t.Fatal("non-stream gpt-image should trigger TTFB timeout even without streaming")
	}
	if reqCtx.ctx.Err() == nil {
		t.Fatal("ctx should be canceled by TTFB timer")
	}
}

func TestNewRequestContext_NormalNonStreamNoTTFB(t *testing.T) {
	s := &Server{
		firstByteTimeout: 30 * time.Second,
		nonStreamTimeout: 500 * time.Millisecond,
	}

	body := []byte(`{"model":"gpt-4o"}`)
	reqCtx := s.newRequestContext(context.Background(), "/v1/chat/completions", body, "gpt-4o")
	defer reqCtx.cleanup()

	if reqCtx.firstByteTimeoutUsed != 0 {
		t.Errorf("normal non-stream should have no TTFB, got %v", reqCtx.firstByteTimeoutUsed)
	}
	// 50ms 内 firstByteTimedOut 不会被置位
	time.Sleep(50 * time.Millisecond)
	if reqCtx.firstByteTimeoutTriggered() {
		t.Fatal("normal non-stream should not have TTFB fire")
	}
}
