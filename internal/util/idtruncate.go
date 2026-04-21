// Package util 中 idtruncate 提供针对超长 ID 字段的截断工具
//
// 背景：部分上游（如 Codex / GPT-5.4 系列）会生成超过 64 字符的
// call_id / tool_call_id / tool_calls[].id，下游 OpenAI/Codex 客户端
// 严格校验长度时会直接报错。这里把超长值统一截到 maxLen 字符。
//
// 实现完全对齐参考脚本 /tmp/cpa-ref/TrunCationProxy.py 的两条正则：
//   - "(call_id|tool_call_id)"\s*:\s*"([^"]{65,})"
//   - "id"\s*:\s*"([^"]{65,})"
package util

import (
	"bytes"
	"regexp"
)

// DefaultIDMaxLen 默认 ID 最大字符数（与参考脚本一致）
const DefaultIDMaxLen = 64

// 触发截断的最小长度阈值 = maxLen+1（即 > maxLen 才截）
// 这两条正则是预编译的，避免每次请求重复编译

var (
	// 匹配 call_id / tool_call_id 字段超长值
	reCallIDFields = regexp.MustCompile(`"(call_id|tool_call_id)"\s*:\s*"([^"]{65,})"`)
	// 匹配裸 id 字段超长值（覆盖 tool_calls[].id）
	reBareID = regexp.MustCompile(`"id"\s*:\s*"([^"]{65,})"`)
)

// TruncateLongIDs 对一段字节做整体截断，返回新字节和截断字段数
// 入参 maxLen <= 0 时使用 DefaultIDMaxLen
// 没截断时返回原 slice（避免无意义拷贝）
func TruncateLongIDs(data []byte, maxLen int) ([]byte, int) {
	if len(data) == 0 {
		return data, 0
	}
	if maxLen <= 0 {
		maxLen = DefaultIDMaxLen
	}

	// 快速路径：完全没有可疑字段直接返回，省正则开销
	if !bytes.Contains(data, []byte(`"id"`)) &&
		!bytes.Contains(data, []byte(`"call_id"`)) &&
		!bytes.Contains(data, []byte(`"tool_call_id"`)) {
		return data, 0
	}

	count := 0

	// 第一遍：call_id / tool_call_id
	out := reCallIDFields.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := reCallIDFields.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		val := sub[2]
		if len(val) <= maxLen {
			return match
		}
		count++
		// 重建：保持原始 key 名（call_id 或 tool_call_id）
		buf := make([]byte, 0, len(sub[1])+maxLen+6)
		buf = append(buf, '"')
		buf = append(buf, sub[1]...)
		buf = append(buf, '"', ':', '"')
		buf = append(buf, val[:maxLen]...)
		buf = append(buf, '"')
		return buf
	})

	// 第二遍：裸 id（在第一遍输出上跑，避免重复处理）
	out = reBareID.ReplaceAllFunc(out, func(match []byte) []byte {
		sub := reBareID.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		val := sub[1]
		if len(val) <= maxLen {
			return match
		}
		count++
		buf := make([]byte, 0, maxLen+8)
		buf = append(buf, '"', 'i', 'd', '"', ':', '"')
		buf = append(buf, val[:maxLen]...)
		buf = append(buf, '"')
		return buf
	})

	return out, count
}

// IDTruncator 流式截断器，用于 SSE 响应
//
// 跨 chunk 边界处理策略：以最后一个 '\n' 为安全切点
// 因为 SSE 协议里 data: 行内的 JSON 不含原始换行符，行尾的 \n
// 一定不会切到任何字段中间。
//
// 兜底：如果累积 buffer 超过 maxBufferSize 都还没换行，
// 直接吐出避免内存爆，代价是这段不做截断
type IDTruncator struct {
	maxLen        int
	pending       []byte // 跨 chunk 缓存
	maxBufferSize int    // 兜底上限
	totalCount    int    // 累计截断字段数（供日志查询）
}

// NewIDTruncator 创建流式截断器
// maxLen <= 0 用默认值；maxBufferSize <= 0 用 256KB 兜底
func NewIDTruncator(maxLen int) *IDTruncator {
	if maxLen <= 0 {
		maxLen = DefaultIDMaxLen
	}
	return &IDTruncator{
		maxLen:        maxLen,
		maxBufferSize: 256 * 1024,
	}
}

// Transform 处理一段 chunk，返回截断后的字节
// 没有完整行（无换行）就先全部 buffer，等下个 chunk
func (t *IDTruncator) Transform(chunk []byte) []byte {
	if len(chunk) == 0 {
		return chunk
	}

	// 拼上之前 pending 的残段
	var combined []byte
	if len(t.pending) > 0 {
		combined = make([]byte, 0, len(t.pending)+len(chunk))
		combined = append(combined, t.pending...)
		combined = append(combined, chunk...)
		t.pending = t.pending[:0]
	} else {
		combined = chunk
	}

	// 找最后一个 '\n' 作为安全切点
	lastNL := bytes.LastIndexByte(combined, '\n')
	if lastNL < 0 {
		// 整段没换行：判断是否超兜底
		if len(combined) >= t.maxBufferSize {
			// 直接吐出，本段放弃截断，返回原始数据
			out := make([]byte, len(combined))
			copy(out, combined)
			return out
		}
		// buffer 起来等下个 chunk
		t.pending = append(t.pending[:0], combined...)
		return nil
	}

	// 安全部分 = combined[:lastNL+1]，含换行
	safe := combined[:lastNL+1]
	tail := combined[lastNL+1:]
	// 残段塞回 pending
	if len(tail) > 0 {
		t.pending = append(t.pending[:0], tail...)
	}

	// 对安全段做截断
	out, n := TruncateLongIDs(safe, t.maxLen)
	t.totalCount += n
	// TruncateLongIDs 没截断时直接返回原 slice，但原 slice 指向 combined
	// 这里需要确保返回的 slice 在调用方写入完成前不被复用
	// 由于 combined 可能就是入参 chunk（无 pending 场景），调用方要么立即写入，
	// 要么得自己复制。proxy_stream.go 的 streamCopyWithBufferSize 是立即写入的，
	// 所以这里安全，无需额外复制。
	return out
}

// Flush EOF 时调用，吐出 pending 残段并做最后一次截断
func (t *IDTruncator) Flush() []byte {
	if len(t.pending) == 0 {
		return nil
	}
	out, n := TruncateLongIDs(t.pending, t.maxLen)
	t.totalCount += n
	// 复制一份避免后续 Reset 影响调用方
	result := make([]byte, len(out))
	copy(result, out)
	t.pending = t.pending[:0]
	return result
}

// TruncatedCount 返回累计截断字段数（用于日志统计）
func (t *IDTruncator) TruncatedCount() int {
	return t.totalCount
}
