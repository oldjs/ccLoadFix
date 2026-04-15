package app

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"time"
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
	// 流传输中段读取空闲定时器：首字节到达后启动，每次收到数据重置
	// 超过阈值无数据 → 判定流卡死 → cancel context → 关闭body打断阻塞Read
	readIdleTimer    *time.Timer
	readIdleTimedOut atomic.Bool
	readIdleTimeout  time.Duration // 存下来给 Reset 用
}

// newRequestContext 创建请求上下文（处理超时控制）
// 设计原则：
// - 流式请求：使用 firstByteTimeout（首字节超时，按模型分级），之后不限制
// - 非流式请求：使用 nonStreamTimeout（整体超时），超时主动关闭上游连接
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte, model string) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)

	ctx, cancel := context.WithCancel(parentCtx)

	// 非流式请求：在基础 cancel 之上叠加整体超时
	if !isStreaming && s.nonStreamTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, s.nonStreamTimeout)
		originalCancel := cancel
		cancel = func() {
			timeoutCancel()
			originalCancel()
		}
	}

	reqCtx := &requestContext{
		ctx:         ctx,
		cancel:      cancel,
		startTime:   time.Now(),
		isStreaming: isStreaming,
	}

	// 流式请求：按模型分级设置首字节超时
	if isStreaming {
		timeout := modelFirstByteTimeout(s.firstByteTimeout, model, body)
		if timeout > 0 {
			reqCtx.firstByteTimer = time.AfterFunc(timeout, func() {
				reqCtx.firstByteTimedOut.Store(true)
				cancel()
			})
		}
	}

	return reqCtx
}

// modelFirstByteTimeout 按模型分级返回首字节超时
// 快模型(flash/haiku/mini等)：10s，标准模型：15s(兜底)，慢模型(opus/o3/thinking)：25s
func modelFirstByteTimeout(serverDefault time.Duration, model string, body []byte) time.Duration {
	lower := strings.ToLower(model)

	// 慢模型：deep thinking、大推理，首字节天然慢
	if isSlowFirstByteModel(lower, body) {
		return 25 * time.Second
	}

	// 快模型：轻量推理，首字节应该很快
	if isFastFirstByteModel(lower) {
		return 10 * time.Second
	}

	// 标准模型：用服务端配置的兜底值（默认15s）
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

// startReadIdleTimer 在首字节到达后启动读取空闲定时器
// 到期时设置标志 + cancel context → AfterFunc 关闭 body → 打断阻塞的 Read
func (rc *requestContext) startReadIdleTimer(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	rc.readIdleTimeout = timeout
	rc.readIdleTimer = time.AfterFunc(timeout, func() {
		rc.readIdleTimedOut.Store(true)
		rc.cancel()
	})
}

// resetReadIdleTimer 每次从上游收到数据时重置定时器，重新计时
func (rc *requestContext) resetReadIdleTimer() {
	if rc.readIdleTimer != nil {
		rc.readIdleTimer.Reset(rc.readIdleTimeout)
	}
}

func (rc *requestContext) readIdleTimeoutTriggered() bool {
	return rc.readIdleTimedOut.Load()
}

// Duration 返回从请求开始到现在的时间
func (rc *requestContext) Duration() time.Duration {
	return time.Since(rc.startTime)
}

// cleanup 统一清理请求上下文资源（定时器 + context）
// [INFO] 符合 Go 惯用法：defer reqCtx.cleanup() 一行搞定
func (rc *requestContext) cleanup() {
	rc.stopFirstByteTimer() // 停止首字节超时定时器
	if rc.readIdleTimer != nil {
		rc.readIdleTimer.Stop() // 停止读取空闲定时器
	}
	rc.cancel() // 取消 context（总是非 nil，无需检查）
}
