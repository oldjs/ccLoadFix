package util

import (
	"bytes"
	"strings"
	"testing"
)

// 造一个 n 字符的 a 串
func longStr(n int) string {
	return strings.Repeat("a", n)
}

func TestTruncateLongIDs_NoMatch(t *testing.T) {
	// 完全没有可疑字段，应原样返回
	data := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	out, n := TruncateLongIDs(data, 64)
	if n != 0 {
		t.Errorf("期望截断数=0，实得 %d", n)
	}
	if !bytes.Equal(out, data) {
		t.Errorf("期望原样返回，实得差异")
	}
}

func TestTruncateLongIDs_ShortIDsUntouched(t *testing.T) {
	// 短 ID（<=64 字符）不应被截断
	tests := []string{
		`{"call_id":"short_id"}`,
		`{"tool_call_id":"` + longStr(64) + `"}`, // 正好 64
		`{"id":"abc123"}`,
	}
	for _, in := range tests {
		out, n := TruncateLongIDs([]byte(in), 64)
		if n != 0 {
			t.Errorf("输入 %q 期望截断数=0，实得 %d", in, n)
		}
		if string(out) != in {
			t.Errorf("输入 %q 期望原样，实得 %q", in, string(out))
		}
	}
}

func TestTruncateLongIDs_CallID(t *testing.T) {
	val := longStr(100)
	in := `{"call_id":"` + val + `","other":"data"}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	expected := `{"call_id":"` + longStr(64) + `","other":"data"}`
	if string(out) != expected {
		t.Errorf("期望 %q\n实得 %q", expected, string(out))
	}
}

func TestTruncateLongIDs_ToolCallID(t *testing.T) {
	val := longStr(80)
	in := `{"tool_call_id":"` + val + `"}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	expected := `{"tool_call_id":"` + longStr(64) + `"}`
	if string(out) != expected {
		t.Errorf("期望 %q\n实得 %q", expected, string(out))
	}
}

func TestTruncateLongIDs_BareID(t *testing.T) {
	// tool_calls[].id 场景
	val := longStr(120)
	in := `{"tool_calls":[{"id":"` + val + `","type":"function"}]}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	expected := `{"tool_calls":[{"id":"` + longStr(64) + `","type":"function"}]}`
	if string(out) != expected {
		t.Errorf("期望 %q\n实得 %q", expected, string(out))
	}
}

func TestTruncateLongIDs_MultipleFields(t *testing.T) {
	// 混合多个超长字段
	valCall := longStr(100)
	valTool := longStr(80)
	valID := longStr(70)
	in := `{"call_id":"` + valCall + `","tool_call_id":"` + valTool + `","tool_calls":[{"id":"` + valID + `"}]}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 3 {
		t.Errorf("期望截断数=3，实得 %d", n)
	}
	expected := `{"call_id":"` + longStr(64) + `","tool_call_id":"` + longStr(64) + `","tool_calls":[{"id":"` + longStr(64) + `"}]}`
	if string(out) != expected {
		t.Errorf("期望 %q\n实得 %q", expected, string(out))
	}
}

func TestTruncateLongIDs_WhitespaceAroundColon(t *testing.T) {
	// 参考脚本行为：空白会在重建时被去掉（JSON 等价，不影响语义）
	val := longStr(100)
	in := `{"call_id" : "` + val + `"}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	// 重建后是紧凑格式（与 Python 参考脚本一致）
	if !bytes.Contains(out, []byte(`"call_id":"`+longStr(64)+`"`)) {
		t.Errorf("截断失败，实得 %q", string(out))
	}
}

func TestTruncateLongIDs_EmptyInput(t *testing.T) {
	out, n := TruncateLongIDs(nil, 64)
	if n != 0 || out != nil {
		t.Errorf("nil 入参应返回 nil 和 0，实得 %v %d", out, n)
	}
	out, n = TruncateLongIDs([]byte{}, 64)
	if n != 0 || len(out) != 0 {
		t.Errorf("空入参应返回空，实得 %v %d", out, n)
	}
}

func TestTruncateLongIDs_DefaultMaxLen(t *testing.T) {
	// maxLen <= 0 应走默认值 64
	val := longStr(100)
	in := `{"call_id":"` + val + `"}`
	out, n := TruncateLongIDs([]byte(in), 0)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	if !bytes.Contains(out, []byte(`"call_id":"`+longStr(DefaultIDMaxLen)+`"`)) {
		t.Errorf("默认长度截断失败，实得 %q", string(out))
	}
}

func TestIDTruncator_SingleChunk(t *testing.T) {
	tr := NewIDTruncator(64)
	val := longStr(100)
	data := []byte(`{"call_id":"` + val + `"}` + "\n")

	out := tr.Transform(data)
	expected := `{"call_id":"` + longStr(64) + `"}` + "\n"
	if string(out) != expected {
		t.Errorf("期望 %q\n实得 %q", expected, string(out))
	}
	if tr.TruncatedCount() != 1 {
		t.Errorf("期望计数=1，实得 %d", tr.TruncatedCount())
	}
}

func TestIDTruncator_NoNewlineBuffers(t *testing.T) {
	tr := NewIDTruncator(64)
	val := longStr(100)
	data := []byte(`{"call_id":"` + val + `"}`)

	// 没换行：应全部 buffer，返回 nil
	out := tr.Transform(data)
	if out != nil {
		t.Errorf("无换行时应返回 nil，实得 %q", string(out))
	}

	// Flush：应输出截断后的内容
	tail := tr.Flush()
	expected := `{"call_id":"` + longStr(64) + `"}`
	if string(tail) != expected {
		t.Errorf("期望 Flush %q\n实得 %q", expected, string(tail))
	}
}

func TestIDTruncator_MultiChunkBoundary(t *testing.T) {
	// SSE 场景：一个事件被切成两段
	tr := NewIDTruncator(64)
	val := longStr(100)
	full := []byte(`data: {"call_id":"` + val + `"}` + "\n\n")

	// 在中间某个位置切开（故意切在字段值中间）
	cut := 30 // 卡在 val 中间
	part1 := full[:cut]
	part2 := full[cut:]

	var collected []byte
	if out := tr.Transform(part1); out != nil {
		collected = append(collected, out...)
	}
	if out := tr.Transform(part2); out != nil {
		collected = append(collected, out...)
	}
	if tail := tr.Flush(); tail != nil {
		collected = append(collected, tail...)
	}

	expected := `data: {"call_id":"` + longStr(64) + `"}` + "\n\n"
	if string(collected) != expected {
		t.Errorf("跨 chunk 截断失败\n期望 %q\n实得 %q", expected, string(collected))
	}
	if tr.TruncatedCount() != 1 {
		t.Errorf("期望计数=1，实得 %d", tr.TruncatedCount())
	}
}

func TestIDTruncator_MultipleEvents(t *testing.T) {
	// 多个 SSE 事件在一个 chunk
	tr := NewIDTruncator(64)
	a := longStr(100)
	b := longStr(80)
	data := []byte(`data: {"call_id":"` + a + `"}` + "\n\n" +
		`data: {"tool_call_id":"` + b + `"}` + "\n\n")

	out := tr.Transform(data)
	expected := `data: {"call_id":"` + longStr(64) + `"}` + "\n\n" +
		`data: {"tool_call_id":"` + longStr(64) + `"}` + "\n\n"
	if string(out) != expected {
		t.Errorf("多事件截断失败\n期望 %q\n实得 %q", expected, string(out))
	}
	if tr.TruncatedCount() != 2 {
		t.Errorf("期望计数=2，实得 %d", tr.TruncatedCount())
	}
}

func TestIDTruncator_NoMatchPassthrough(t *testing.T) {
	tr := NewIDTruncator(64)
	data := []byte("data: {\"content\":\"hello world\"}\n\n")
	out := tr.Transform(data)
	if string(out) != string(data) {
		t.Errorf("无匹配应原样透传\n期望 %q\n实得 %q", string(data), string(out))
	}
	if tr.TruncatedCount() != 0 {
		t.Errorf("期望计数=0，实得 %d", tr.TruncatedCount())
	}
}

func TestIDTruncator_IncrementalNoNewlineUntilEnd(t *testing.T) {
	// 非 SSE 响应：整个 body 没换行，多 chunk 累积，EOF 时一次性吐出
	tr := NewIDTruncator(64)
	val := longStr(100)
	full := `{"call_id":"` + val + `","extra":"text"}`

	// 切成 3 段
	l := len(full)
	c1 := full[:l/3]
	c2 := full[l/3 : 2*l/3]
	c3 := full[2*l/3:]

	var collected []byte
	for _, part := range []string{c1, c2, c3} {
		if out := tr.Transform([]byte(part)); out != nil {
			collected = append(collected, out...)
		}
	}
	if tail := tr.Flush(); tail != nil {
		collected = append(collected, tail...)
	}

	expected := `{"call_id":"` + longStr(64) + `","extra":"text"}`
	if string(collected) != expected {
		t.Errorf("非 SSE 多 chunk 截断失败\n期望 %q\n实得 %q", expected, string(collected))
	}
}

func TestIDTruncator_FlushEmpty(t *testing.T) {
	// Flush 无 pending 应返回 nil
	tr := NewIDTruncator(64)
	if out := tr.Flush(); out != nil {
		t.Errorf("空 Flush 应返回 nil，实得 %q", string(out))
	}
}

func TestTruncateLongIDs_RealisticOpenAIToolCalls(t *testing.T) {
	// 模拟真实的 OpenAI tool_calls 响应
	longCallID := "call_" + longStr(120)
	in := `{"id":"chatcmpl-abc123","choices":[{"message":{"tool_calls":[{"id":"` + longCallID + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`
	out, n := TruncateLongIDs([]byte(in), 64)
	if n != 1 {
		t.Errorf("期望截断数=1，实得 %d", n)
	}
	// chatcmpl-abc123 短，不截；longCallID 超长要截
	if !bytes.Contains(out, []byte(`"id":"chatcmpl-abc123"`)) {
		t.Errorf("短 id 被误截")
	}
	if !bytes.Contains(out, []byte(`"id":"`+longCallID[:64]+`"`)) {
		t.Errorf("长 id 未按规则截到 64")
	}
}
