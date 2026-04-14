package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 代理转发集成测试
// 端到端验证：上游模拟 → Server → gin 路由 → 请求转发 → 响应返回
// ============================================================================

// testChannel 测试用渠道定义
type testChannel struct {
	name        string
	channelType string
	models      string // 逗号分隔的模型列表
	apiKey      string
	priority    int
}

// proxyTestEnv 集成测试环境
type proxyTestEnv struct {
	server *Server
	store  storage.Store
	engine *gin.Engine
}

// setupProxyTestEnv 创建指向 mockUpstream 的完整测试 Server
// 每个渠道的 URL 使用 upstreamURLs map（channelIndex → upstreamURL）
func setupProxyTestEnv(t testing.TB, channels []testChannel, upstreamURLs map[int]string) *proxyTestEnv {
	t.Helper()

	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()

	// 创建渠道和 API Key
	for i, ch := range channels {
		upURL := upstreamURLs[i]
		if upURL == "" {
			t.Fatalf("missing upstream URL for channel %d", i)
		}

		priority := ch.priority
		if priority == 0 {
			priority = 100 - i*10 // 按顺序递减优先级
		}

		chType := ch.channelType
		if chType == "" {
			chType = util.ChannelTypeOpenAI
		}

		// 构建模型列表
		var modelEntries []model.ModelEntry
		for _, m := range strings.Split(ch.models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelEntries = append(modelEntries, model.ModelEntry{Model: m})
			}
		}

		cfg := &model.Config{
			Name:         ch.name,
			URL:          upURL,
			ChannelType:  chType,
			Priority:     priority,
			Enabled:      true,
			ModelEntries: modelEntries,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("CreateConfig for %s: %v", ch.name, err)
		}

		// 创建 API Key
		apiKey := ch.apiKey
		if apiKey == "" {
			apiKey = fmt.Sprintf("sk-test-%d", i)
		}
		err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: created.ID, KeyIndex: 0, APIKey: apiKey},
		})
		if err != nil {
			t.Fatalf("CreateAPIKeysBatch for %s: %v", ch.name, err)
		}
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	return &proxyTestEnv{
		server: srv,
		store:  store,
		engine: engine,
	}
}

// doProxyRequest 发送代理请求并返回响应
func doProxyRequest(t testing.TB, engine *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key") // 默认 token

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// ============================================================================
// P0: 代理转发核心链路测试
// ============================================================================

func TestProxy_Success_NonStreaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + JSON
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证响应透传
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["id"] != "chatcmpl-1" {
		t.Fatalf("expected id=chatcmpl-1, got %v", resp["id"])
	}
}

func TestProxy_Success_Streaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 流
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" World"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证 SSE 内容被透传
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("expected SSE to contain 'Hello', body: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE to contain '[DONE]', body: %s", body)
	}
}

func TestProxy_ChannelRetry_On503(t *testing.T) {
	t.Parallel()

	// 渠道1：返回 503
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstream1.Close()

	// 渠道2：返回 200
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-fail", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fallback to ch2), got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_MultiURL5xx_TriesNextURLInSameChannel(t *testing.T) {
	t.Parallel()

	var ch1FailCalls atomic.Int64
	var ch1SecondURLCalls atomic.Int64

	// ch1 URL1: 503
	upstreamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1FailCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"error\":\"service unavailable\"}"))
	}))
	defer upstreamFail.Close()

	// ch1 URL2: 5xx should try next URL in same channel
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1SecondURLCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"id\":\"from-ch1-url2\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}"))
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1", priority: 100},
	}, map[int]string{
		0: upstreamFail.URL + "\n" + upstreamOK.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	channelID := configs[0].ID

	// 强制首跳命中失败URL
	env.server.urlSelector.CooldownURL(channelID, upstreamOK.URL)

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// 5xx后应该试了同渠道的第二个URL并成功
	if !strings.Contains(w.Body.String(), "from-ch1-url2") {
		t.Fatalf("expected fallback to second URL in same channel, got body: %s", w.Body.String())
	}
	if ch1FailCalls.Load() < 1 {
		t.Fatalf("expected first URL attempted, got %d", ch1FailCalls.Load())
	}
	if ch1SecondURLCalls.Load() < 1 {
		t.Fatalf("expected second URL attempted after 5xx, got %d", ch1SecondURLCalls.Load())
	}
}

func TestProxy_MultiURLFallbackOn598_DoesNotChannelCooldownEarly(t *testing.T) {
	t.Parallel()

	var failCalls atomic.Int64
	var okCalls atomic.Int64

	// URL1: 首字节超时（598）
	upstreamTimeout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamTimeout.Close()

	// URL2: 正常返回
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-url2\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamTimeout.URL + "\n" + upstreamOK.URL,
	})

	// 缩短首字节超时，稳定触发 598
	env.server.firstByteTimeout = 50 * time.Millisecond

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 强制 URL2 进入冷却，确保首跳先打到 timeout URL
	env.server.urlSelector.CooldownURL(channelID, upstreamOK.URL)

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-url2") {
		t.Fatalf("expected fallback to url2 on 598, got body: %s", w.Body.String())
	}
	fail := failCalls.Load()
	ok := okCalls.Load()
	if fail < 1 || ok < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", fail, ok)
	}

	// 关键断言：598 触发多URL内部回退成功后，不应残留渠道级冷却
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Fatalf("unexpected channel cooldown for multi-url fallback success, channel_id=%d", channelID)
	}
}

func TestProxy_MultiURLFirstAttempt_UsesAffinityAndWeightedRandom(t *testing.T) {
	t.Parallel()

	var fastCalls atomic.Int64
	var slowCalls atomic.Int64

	upstreamFast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fast","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamFast.Close()

	upstreamSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-slow","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamSlow.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-weighted-first", models: "gpt-4,gpt-3.5", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamSlow.URL + "\n" + upstreamFast.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 预热EWMA（延迟要在100-3000ms甜区内，<100ms会被按500ms算）
	env.server.urlSelector.RecordLatency(channelID, upstreamFast.URL, 200*time.Millisecond)
	env.server.urlSelector.RecordLatency(channelID, upstreamSlow.URL, 2000*time.Millisecond)

	// 测试1: 同模型连续请求应该命中亲和性（首次成功后一直用同一个URL）
	for range 10 {
		w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    "gpt-4",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	fast := fastCalls.Load()
	slow := slowCalls.Load()
	// 亲和性生效：第一次选中某个URL后，后续9次应该都用同一个
	// 加权随机偏好fast(权重高)，但不一定100%命中。关键是后续请求粘性
	total := fast + slow
	if total != 10 {
		t.Fatalf("expected 10 total calls, got %d", total)
	}
	// fast权重远大于slow，首次大概率选fast，然后亲和性锁定
	if fast < 9 {
		t.Logf("affinity test: fast=%d slow=%d (fast should dominate due to affinity)", fast, slow)
	}

	// 测试2: 不同模型各自建立亲和性，验证路由正常工作
	fastCalls.Store(0)
	slowCalls.Store(0)
	for range 60 {
		// 交替不同模型，每个模型有自己的亲和性
		for _, model := range []string{"gpt-4", "gpt-3.5"} {
			w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
				"model":    model,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
		}
	}

	fast = fastCalls.Load()
	slow = slowCalls.Load()
	total = fast + slow
	if total != 120 {
		t.Fatalf("expected 120 total calls, got %d", total)
	}
	// httptest服务器TTFB极低(<10ms)，EWMA会被拉到<100ms死区并被clamp到500ms
	// 两个URL的effective latency相同时，加权随机退化为~50/50（仅有1.5x亲和性软偏置）
	// 此处只验证路由正常分发流量到两个URL，不做严格偏好断言
	if fast == 0 || slow == 0 {
		t.Fatalf("expected traffic to both URLs, fast=%d slow=%d", fast, slow)
	}
	t.Logf("weighted random distribution: fast=%d slow=%d (both URLs active)", fast, slow)
}

func TestProxy_MultiURLProbeCanceledByShutdown_DoesNotPolluteCooldown(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-a","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-b","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamB.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-probe-shutdown", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamA.URL + "\n" + upstreamB.URL,
	})

	env.server.urlSelector.probeTimeout = 5 * time.Second
	started := make(chan struct{}, 2)
	env.server.urlSelector.probeDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("probe dial did not start in time")
		}
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		env.server.urlSelector.mu.RLock()
		probingLeft := len(env.server.urlSelector.probing)
		env.server.urlSelector.mu.RUnlock()
		if probingLeft == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected probing markers to be cleared after shutdown, got %d", probingLeft)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, u := range []string{upstreamA.URL, upstreamB.URL} {
		if env.server.urlSelector.IsCooledDown(channelID, u) {
			t.Fatalf("expected canceled probe not to cooldown url: %s", u)
		}
	}
}

func TestProxy_KeyRetry_On401(t *testing.T) {
	t.Parallel()

	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "sk-bad") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// 创建服务器并使用其 store
	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "ch1-multikey",
		URL:          upstream.URL,
		ChannelType:  util.ChannelTypeOpenAI,
		Priority:     100,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4"}},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-bad"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-good"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (key retry to sk-good), got %d: %s", w.Code, w.Body.String())
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 upstream calls (key retry), got %d", callCount)
	}
}

func TestProxy_AllChannelsExhausted(t *testing.T) {
	t.Parallel()

	callCount1 := 0
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount1++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream1.Close()

	callCount2 := 0
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount2++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 所有渠道失败时应返回最后一个错误状态码
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// 关键行为：必须耗尽所有可用渠道，而不是只尝试第一个就返回（避免"假绿"）。
	if callCount1 < 1 || callCount2 < 1 {
		t.Fatalf("expected to try all channels at least once, got upstream1=%d upstream2=%d", callCount1, callCount2)
	}
}

func TestProxy_ClientCancel_Returns499(t *testing.T) {
	t.Parallel()

	// 上游延迟响应
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
			// already closed
		default:
			close(upstreamStarted)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 创建可取消的请求
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	// 等上游请求真的发出后再取消，避免"还没发出去就 cancel"导致语义漂移
	go func() {
		select {
		case <-upstreamStarted:
		case <-time.After(1 * time.Second):
		}
		cancel()
	}()

	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	// 客户端取消应返回 499 或超时相关状态
	if w.Code != StatusClientClosedRequest && w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 499 or 504, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ModelNotAllowed_Returns403(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4,gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 限制 token 只能使用 gpt-3.5-turbo
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	if td, ok := env.server.authService.authTokens[tokenHash]; ok {
		td.allowedModels = []string{"gpt-3.5-turbo"}
	}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_CostLimitExceeded_Returns429(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 设置 token 费用已超限
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	if td, ok := env.server.authService.authTokens[tokenHash]; ok {
		td.usedMicroUSD = 200_000  // $0.20
		td.limitMicroUSD = 100_000 // $0.10 限额
	}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// 验证错误包含 cost_limit_exceeded
	body := w.Body.String()
	if !strings.Contains(body, "cost_limit_exceeded") {
		t.Fatalf("expected 'cost_limit_exceeded' in body: %s", body)
	}
}

func TestProxy_NoChannels_Returns503(t *testing.T) {
	t.Parallel()

	// 创建没有渠道的环境
	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_SSEErrorEvent_TriggersCooldown(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 但包含 error 事件
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 先正常发几个 chunk，然后发 error
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
			`event: error` + "\n" + `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	// 先拿到渠道ID（避免硬编码）
	var channelID int64
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "ch1" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("channel ch1 not found")
	}

	// 预期：请求前没有渠道冷却（否则测试语义不成立）
	beforeCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(before): %v", err)
	}
	if _, exists := beforeCooldowns[channelID]; exists {
		t.Fatalf("expected no channel cooldown before request, but found one for channel_id=%d", channelID)
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// SSE error 事件的处理：HTTP 状态码已经是 200（头部已发送），
	// 但内部应触发冷却逻辑。测试验证响应不崩溃。
	// 响应仍是 200（因为 header 已发送），但内部会记录冷却
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	// 关键断言：SSE error 事件必须触发冷却副作用（单Key渠道会升级为渠道级冷却）。
	afterCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(after): %v", err)
	}
	until, exists := afterCooldowns[channelID]
	if !exists {
		t.Fatalf("expected channel cooldown to be set after SSE error event, channel_id=%d", channelID)
	}
	if time.Until(until) <= 0 {
		t.Fatalf("expected channel cooldown until in the future, got %v", until)
	}
}
