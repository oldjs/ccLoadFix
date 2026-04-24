package util

import "strings"

// IsGPTImageModel 判断是不是 gpt-image 系列的图像生成模型
// 覆盖：
//   - gpt-image-*         (gpt-image-1 / gpt-image-1-mini / gpt-image-1.5 等)
//   - chatgpt-image-*     (chatgpt-image-latest)
//
// 用途：这类模型容易因上游链路卡死/提前断流需要独立的超时和错误分类策略
func IsGPTImageModel(model string) bool {
	if model == "" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(lower, "gpt-image-") ||
		strings.HasPrefix(lower, "chatgpt-image-")
}

// GPTImageStreamIncompletePatterns 上游返回 HTTP 408 时，如果 body 中含这些特征，
// 说明是"上游自己的流断了而非客户端慢发请求"，应当升级为渠道级错误以触发重试/冷却
// 仅在 gpt-image 系列模型上下文下用这个列表，避免误伤其他场景的 408
var gptImageStreamIncompletePatterns = []string{
	"stream disconnected before completion",
	"stream closed before response.completed",
	"stream closed before response completed",
	"stream closed",
	"response.completed",
	"stream error",
}

// BodyLooksLikeStreamIncomplete 判断响应体是否像 "上游流提前断了" 的特征
// 只扫小写字面量，不解析 JSON（容错，速度优先）
func BodyLooksLikeStreamIncomplete(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 限制扫描长度，防止超大 body 浪费 CPU
	const maxScan = 4096
	if len(body) > maxScan {
		body = body[:maxScan]
	}
	lower := strings.ToLower(string(body))
	for _, pat := range gptImageStreamIncompletePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}
