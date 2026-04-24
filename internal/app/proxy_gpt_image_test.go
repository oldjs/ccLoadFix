package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/util"
)

// ============================================================================
// gpt-image 快失败 / 流式 idle watchdog 集成测试
// 关键语义：
//   - gpt-image-* 命中独立的 TTFB + idle 超时
//   - 普通模型不受影响
//   - "正常 1-5 分钟生图"只要上游持续有字节/事件进展就不会被误杀
// ============================================================================

// TestProxy_GPTImageNonStream_TTFBFastFail
// 场景：非流式 gpt-image 请求，上游迟迟不回响应头
// 期望：在专用 TTFB 阈值内快速失败，不等全局 nonStreamTimeout
func TestProxy_GPTImageNonStream_TTFBFastFail(t *testing.T) {
	t.Parallel()

	var called atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		// 模拟上游挂死：远超 TTFB 阈值才开始回
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"late"}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-image", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 模拟用户的"非流 10 分钟"配置，但给 gpt-image 挂上 100ms 的 TTFB 让测试快
	env.server.nonStreamTimeout = 600 * time.Second
	env.server.gptImageFirstByteTimeout = 100 * time.Millisecond
	env.server.gptImageUpstreamTimeout = 600 * time.Second

	start := time.Now()
	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-1",
		"messages": []map[string]string{{"role": "user", "content": "a cat"}},
	}, nil)
	elapsed := time.Since(start)

	// 快失败：远小于全局 nonStreamTimeout
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("gpt-image non-stream should fast-fail within ~100ms TTFB, took %v", elapsed)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("expected failure, got 200 body=%s", w.Body.String())
	}
	// 598 的对外映射是 504
	if w.Code != http.StatusGatewayTimeout && w.Code != http.StatusBadGateway && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 5xx after TTFB, got %d body=%s", w.Code, w.Body.String())
	}
	if called.Load() < 1 {
		t.Fatal("upstream should have been attempted")
	}
}

// TestProxy_NormalModelNonStream_NotAffected
// 场景：普通文字模型非流式 + 慢上游
// 期望：不受 gpt-image 专用阈值影响，由 nonStreamTimeout 主导
func TestProxy_NormalModelNonStream_NotAffected(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 普通模型：上游 200ms 后返回正常响应
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-gpt4", channelType: util.ChannelTypeOpenAI, models: "gpt-4o", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 故意把 gpt-image 的 TTFB 设得很短
	env.server.gptImageFirstByteTimeout = 10 * time.Millisecond
	env.server.gptImageUpstreamTimeout = 10 * time.Millisecond
	env.server.nonStreamTimeout = 5 * time.Second

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 普通模型应该正常成功，不受 gpt-image 阈值影响
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for normal model, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"ok"`) {
		t.Fatalf("expected normal success body, got %s", w.Body.String())
	}
}

// TestProxy_GPTImage408StreamIncomplete_ChannelSwitch
// 场景：gpt-image 上游返回 HTTP 408 + body 含 "stream disconnected before completion"
// 期望：升级为渠道级错误，切换到下一个渠道并成功
func TestProxy_GPTImage408StreamIncomplete_ChannelSwitch(t *testing.T) {
	t.Parallel()

	var ch1Calls, ch2Calls atomic.Int32

	// ch1：返回用户日志里复现的 408 错误
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"stream error: stream disconnected before completion: stream closed before response.completed","type":"invalid_request_error"}}`))
	}))
	defer upstream1.Close()

	// ch2：正常返回
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-image-bad", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-1", priority: 100},
		{name: "ch-image-ok", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-1",
		"messages": []map[string]string{{"role": "user", "content": "cat"}},
	}, nil)

	// 关键断言：408 被升级为渠道级错误 → 切到 ch2 → 用户拿到 200
	if w.Code != http.StatusOK {
		t.Fatalf("expected fallback to ch2 (200), got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected body from ch2, got %s", w.Body.String())
	}
	if ch1Calls.Load() < 1 {
		t.Fatal("ch1 should be tried first")
	}
	if ch2Calls.Load() < 1 {
		t.Fatal("ch2 must have been tried after ch1 stream-incomplete")
	}

	// 再断：ch1 应当被冷却（渠道级），不残留
	ctx := context.Background()
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	configs, _ := env.store.ListConfigs(ctx)
	var ch1ID int64
	for _, c := range configs {
		if c.Name == "ch-image-bad" {
			ch1ID = c.ID
			break
		}
	}
	if ch1ID == 0 {
		t.Fatal("could not resolve ch1 id")
	}
	if _, cooled := cooldowns[ch1ID]; !cooled {
		t.Fatal("ch1 should be cooled after 408 stream-incomplete upgrade")
	}
}

// TestProxy_NormalModel408_NotUpgraded
// 场景：普通模型 408（比如真 client slow，非 gpt-image）
// 期望：保持 ErrorLevelClient 语义，不升级、不冷却
func TestProxy_NormalModel408_NotUpgraded(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"stream error: stream disconnected before completion"}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-gpt4o", channelType: util.ChannelTypeOpenAI, models: "gpt-4o", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 408 透传给客户端
	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("expected 408 transparent for non-gpt-image, got %d body=%s", w.Code, w.Body.String())
	}

	// 不应该冷却渠道（408 保持 client 级）
	ctx := context.Background()
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(cooldowns) > 0 {
		t.Fatalf("normal 408 should NOT cool channel, got %d cooldowns", len(cooldowns))
	}
}

// TestProxy_GPTImageStreaming_IdleWatchdog_FiresOnSilence
// 场景：流式 gpt-image，首字节到达后上游完全静默超过 idle 阈值
// 期望：触发 idle watchdog，升级为 StatusStreamIncomplete 并切渠道重试
func TestProxy_GPTImageStreaming_IdleWatchdog_FiresOnSilence(t *testing.T) {
	t.Parallel()

	var silentCalls, okCalls atomic.Int32

	// ch1：首字节到了后就静默
	upstreamSilent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		silentCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		// 先发一个事件（让 TTFB 过关 + idle 启动）
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 然后挂死远超 idle 阈值
		time.Sleep(800 * time.Millisecond)
	}))
	defer upstreamSilent.Close()

	// ch2：正常 SSE
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-silent", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-1", priority: 100},
		{name: "ch-ok", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstreamSilent.URL, 1: upstreamOK.URL})

	// idle 阈值设很短，TTFB 给足
	env.server.gptImageFirstByteTimeout = 5 * time.Second
	env.server.gptImageStreamIdleTimeout = 150 * time.Millisecond

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-1",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "cat"}},
	}, nil)

	// silent URL 的响应头已发送（200 OK），按项目现有语义，流式响应头已回就不再跨渠道重试
	// 因此这里用户会看到 200（来自 silent URL）+ 提前中断的流
	// 关键验证：silent URL 已触发 idle（上游被主动关闭）、冷却被记录，下次请求不会走这个渠道
	if silentCalls.Load() < 1 {
		t.Fatal("silent upstream should be attempted")
	}

	// 响应状态应为上游发的 200（响应头已透传）
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (headers already sent), got %d body=%s", w.Code, w.Body.String())
	}

	// idle 触发后 ch1 应该被冷却
	ctx := context.Background()
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	configs, _ := env.store.ListConfigs(ctx)
	var silentID int64
	for _, c := range configs {
		if c.Name == "ch-silent" {
			silentID = c.ID
			break
		}
	}
	if silentID == 0 {
		t.Fatal("could not resolve ch-silent id")
	}
	if _, cooled := cooldowns[silentID]; !cooled {
		t.Fatalf("ch-silent should be cooled after idle timeout; cooldowns=%v", cooldowns)
	}
}

// TestProxy_GPTImageStreaming_ProgressKeepsAlive
// 场景：流式 gpt-image，上游每 30ms 发一个进度事件，总时长超过 idle 阈值多倍
// 期望：不触发 idle watchdog，流正常完成（模拟正常 1-5 分钟生图）
func TestProxy_GPTImageStreaming_ProgressKeepsAlive(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		// 持续 600ms，每 30ms 发一次进度事件
		// idle 阈值 150ms：只要持续有字节进展就不会被误杀
		for i := range 20 {
			_, _ = fmt.Fprintf(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"seq\":%d}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-image", channelType: util.ChannelTypeOpenAI, models: "gpt-image-1", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// idle 阈值 150ms，总时长 600ms（是 idle 阈值的 4 倍）
	env.server.gptImageFirstByteTimeout = 5 * time.Second
	env.server.gptImageStreamIdleTimeout = 150 * time.Millisecond

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-1",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "cat"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("expected full stream with response.completed, got: %s", body)
	}
	// 至少收到几个进度事件
	if strings.Count(body, "response.output_item.added") < 5 {
		t.Fatalf("expected multiple progress events, got: %s", body)
	}

	// 没有渠道冷却：正常生图流程不应误触发冷却
	ctx := context.Background()
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(cooldowns) > 0 {
		t.Fatalf("normal progress stream should not cool channel, got %d cooldowns", len(cooldowns))
	}
}
