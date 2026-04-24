package app

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"time"

	"ccLoad/internal/util"
)

// requestContext 封装单次请求的上下文和超时控制
// 从 forwardOnceAsync 提取，遵循SRP原则
// 补充首字节超时管控（可选）
type requestContext struct {
	ctx               context.Context
	cancel            context.CancelFunc // [INFO] 总是非 nil（即使是 noop），调用方无需检查
	startTime         time.Time
	isStreaming       bool
	firstByteTimer    *time.Timer
	firstByteTimedOut atomic.Bool

	// 首字节超时阈值（用于日志打印）
	firstByteTimeoutUsed time.Duration

	// [INFO] gpt-image 等上游容易静默卡死的模型：首字节后仍需 idle watchdog
	// 只要上游持续有字节/事件/心跳进展就重置，正常 1-5 分钟流式生图不会被误杀
	streamIdleTimeout time.Duration // 0 = 关闭 idle watchdog
	streamIdleTimer   *time.Timer
	streamIdleFired   atomic.Bool
	lastProgressAt    atomic.Int64

	// 是否命中 gpt-image 专用超时路径（用于日志/诊断）
	isGPTImageModel bool
}

// timeoutsDecision 汇总单次请求的超时决策结果
type timeoutsDecision struct {
	overallTimeout    time.Duration // 整体超时（非流式）；0=不限制
	firstByteTimeout  time.Duration // 首字节超时；0=不挂 firstByteTimer
	streamIdleTimeout time.Duration // 流式 idle 超时；0=关闭
}

// resolveTimeouts 按模型+请求类型，决定本次请求各类超时
// 设计：gpt-image 系列独立一套阈值；其他模型保持现有行为
func (s *Server) resolveTimeouts(isStreaming bool, model string, body []byte) timeoutsDecision {
	isImage := util.IsGPTImageModel(model)

	if isStreaming {
		var ttfb time.Duration
		switch {
		case isImage && s.gptImageFirstByteTimeout > 0:
			ttfb = s.gptImageFirstByteTimeout
		default:
			ttfb = modelFirstByteTimeout(s.firstByteTimeout, model, body)
		}
		var idle time.Duration
		switch {
		case isImage && s.gptImageStreamIdleTimeout > 0:
			idle = s.gptImageStreamIdleTimeout
		case s.streamIdleTimeout > 0:
			idle = s.streamIdleTimeout
		}
		return timeoutsDecision{
			firstByteTimeout:  ttfb,
			streamIdleTimeout: idle,
		}
	}

	// 非流式
	overall := s.nonStreamTimeout
	if isImage && s.gptImageUpstreamTimeout > 0 {
		// 取更小者：不会让 gpt-image 非流请求超过 nonStreamTimeout，但允许更短的专用上限
		if overall == 0 || s.gptImageUpstreamTimeout < overall {
			overall = s.gptImageUpstreamTimeout
		}
	}
	var ttfb time.Duration
	if isImage && s.gptImageFirstByteTimeout > 0 {
		ttfb = s.gptImageFirstByteTimeout
	}
	return timeoutsDecision{
		overallTimeout:   overall,
		firstByteTimeout: ttfb,
	}
}

// newRequestContext 创建请求上下文（处理超时控制）
// 设计原则：
// - 流式请求：使用 firstByteTimeout（首字节超时，按模型分级），之后可选 idle watchdog
// - 非流式请求：使用 nonStreamTimeout（整体超时），gpt-image 额外挂 TTFB watchdog
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte, model string) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)
	decision := s.resolveTimeouts(isStreaming, model, body)

	ctx, cancel := context.WithCancel(parentCtx)

	// 非流式请求：在基础 cancel 之上叠加整体超时（gpt-image 可能用更短的专用值）
	if !isStreaming && decision.overallTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, decision.overallTimeout)
		originalCancel := cancel
		cancel = func() {
			timeoutCancel()
			originalCancel()
		}
	}

	reqCtx := &requestContext{
		ctx:                  ctx,
		cancel:               cancel,
		startTime:            time.Now(),
		isStreaming:          isStreaming,
		streamIdleTimeout:    decision.streamIdleTimeout,
		firstByteTimeoutUsed: decision.firstByteTimeout,
		isGPTImageModel:      util.IsGPTImageModel(model),
	}

	// 挂首字节超时：流式用 firstByteTimer；非流式 gpt-image 也挂（只有响应头都没回才触发）
	if decision.firstByteTimeout > 0 {
		reqCtx.firstByteTimer = time.AfterFunc(decision.firstByteTimeout, func() {
			reqCtx.firstByteTimedOut.Store(true)
			cancel()
		})
	}

	return reqCtx
}

// modelFirstByteTimeout 按模型分级返回首字节超时
// 快模型(flash/haiku/mini等)：15s，标准模型：30s(兜底)，慢模型(opus/o3/thinking)：60s
func modelFirstByteTimeout(serverDefault time.Duration, model string, body []byte) time.Duration {
	lower := strings.ToLower(model)

	// 慢模型：deep thinking、大推理，首字节天然慢
	if isSlowFirstByteModel(lower, body) {
		return 60 * time.Second
	}

	// 快模型：轻量推理，首字节应该很快
	if isFastFirstByteModel(lower) {
		return 15 * time.Second
	}

	// 标准模型：用服务端配置的兜底值（默认30s）
	return serverDefault
}

// isSlowFirstByteModel 判断是否为首字节天然慢的模型
func isSlowFirstByteModel(lower string, body []byte) bool {
	// thinking/extended_thinking 模式：不管什么模型都慢
	if hasThinkingEnabled(body) {
		return true
	}

	// 已知慢模型
	slowPrefixes := []string{
		"o3", "o4-mini", // OpenAI reasoning 系列
		"claude-opus", "claude-3-opus", // Anthropic Opus
		"claude-3.7-sonnet",                  // 3.7-sonnet 有 thinking
		"gemini-2.5-pro", "gemini-2.5-flash", // Gemini thinking 系列
		"deepseek-r1", "deepseek-reasoner", // DeepSeek reasoning
		"qwq", // 通义千问推理
	}
	for _, prefix := range slowPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// isFastFirstByteModel 判断是否为首字节很快的轻量模型
func isFastFirstByteModel(lower string) bool {
	fastPrefixes := []string{
		"gpt-4o-mini", "gpt-4.1-mini", "gpt-4.1-nano",
		"claude-3-haiku", "claude-3.5-haiku", "claude-haiku",
		"gemini-2.0-flash", "gemini-1.5-flash",
		"deepseek-chat",
		"qwen-turbo", "qwen-plus",
	}
	for _, prefix := range fastPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// hasThinkingEnabled 检查请求体里有没有开 thinking/extended_thinking
func hasThinkingEnabled(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 快速字符串扫描，避免完整 JSON 解析
	return bytes.Contains(body, []byte(`"thinking":`)) ||
		bytes.Contains(body, []byte(`"extended_thinking":`)) ||
		bytes.Contains(body, []byte(`"reasoning_effort":`))
}

func (rc *requestContext) stopFirstByteTimer() {
	if rc.firstByteTimer != nil {
		rc.firstByteTimer.Stop()
	}
}

func (rc *requestContext) firstByteTimeoutTriggered() bool {
	return rc.firstByteTimedOut.Load()
}

// startStreamIdleWatchdog 启动流式 idle watchdog
// 通常在首字节到达时调用；onExpire 用于关闭上游 body 以打断阻塞 Read
// onExpire 必须幂等（可能被多次调用或与正常关闭并发）
// 设计：把 onExpire 捕获到 closure 里，避免跨 goroutine 共享字段引发数据竞争
func (rc *requestContext) startStreamIdleWatchdog(onExpire func()) {
	if rc.streamIdleTimeout <= 0 || onExpire == nil {
		return
	}
	rc.lastProgressAt.Store(time.Now().UnixNano())
	rc.streamIdleTimer = time.AfterFunc(rc.streamIdleTimeout, func() {
		// 幂等：已触发过就不重复执行 onExpire
		if rc.streamIdleFired.Swap(true) {
			return
		}
		onExpire()
	})
}

// resetStreamIdleWatchdog 收到字节/事件后重置 idle timer
// 由 firstByteDetector.onBytesRead 调用，高频小开销：定长 Reset
func (rc *requestContext) resetStreamIdleWatchdog() {
	if rc.streamIdleTimer == nil {
		return
	}
	rc.lastProgressAt.Store(time.Now().UnixNano())
	rc.streamIdleTimer.Reset(rc.streamIdleTimeout)
}

func (rc *requestContext) stopStreamIdleWatchdog() {
	if rc.streamIdleTimer != nil {
		rc.streamIdleTimer.Stop()
	}
}

func (rc *requestContext) streamIdleTimeoutTriggered() bool {
	return rc.streamIdleFired.Load()
}

// streamIdleDuration 返回从上次进展到现在的时长（用于日志）
func (rc *requestContext) streamIdleDuration() time.Duration {
	last := rc.lastProgressAt.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// Duration 返回从请求开始到现在的时间
func (rc *requestContext) Duration() time.Duration {
	return time.Since(rc.startTime)
}

// cleanup 统一清理请求上下文资源（定时器 + context）
// [INFO] 符合 Go 惯用法：defer reqCtx.cleanup() 一行搞定
func (rc *requestContext) cleanup() {
	rc.stopFirstByteTimer()     // 停止首字节超时定时器
	rc.stopStreamIdleWatchdog() // 停止 idle watchdog
	rc.cancel()                 // 取消 context（总是非 nil，无需检查）
}
