package model

// URL 运行时状态种类常量。每行 url_runtime_state 表用 Kind 区分语义。
const (
	URLRuntimeKindLatency      = "latency"       // EWMA 真实请求 TTFB
	URLRuntimeKindProbeLatency = "probe_latency" // EWMA 探测 RTT
	URLRuntimeKindCooldown     = "cooldown"      // URL 级冷却（含累计失败次数）
	URLRuntimeKindSlowIso      = "slow_iso"      // 慢 TTFB 隔离
	URLRuntimeKindNoThinking   = "no_thinking"   // (URL,model) 维度的 thinking 黑名单
	URLRuntimeKindAffinity     = "affinity"      // (channel,model) → URL 亲和
	URLRuntimeKindWarm         = "warm"          // (channel,model) 的 warm 备选 slots
)

// URLRuntimeState 是 URL 选择器持久化用的统一行结构
// 字段按 Kind 解释：
//   - latency / probe_latency: 用 EWMAms
//   - cooldown:                用 ExpiresAt + ConsecutiveFails
//   - slow_iso / no_thinking:  用 ExpiresAt
//   - affinity / warm:         用 Payload (JSON)
//
// 主键: (ChannelID, URL, Model, Kind)；URL 或 Model 不适用时填空字符串。
type URLRuntimeState struct {
	ChannelID        int64
	URL              string  // 空字符串表示 model 维度（affinity/warm）
	Model            string  // 空字符串表示 url 维度（latency/cooldown/...）
	Kind             string  // URLRuntimeKind* 之一
	EWMAms           float64 // 仅 latency / probe_latency
	ExpiresAt        int64   // unix 秒；仅 cooldown / slow_iso / no_thinking
	ConsecutiveFails int64   // 仅 cooldown
	Payload          string  // JSON；affinity / warm
	UpdatedAt        int64   // unix 秒
}

// ChannelAffinityState 渠道级软亲和（model→channel）的持久化行
// TTL 由运行时配置决定，加载时按当前 TTL 过滤过期条目，DB 里允许残留。
type ChannelAffinityState struct {
	Model     string
	ChannelID int64
	UpdatedAt int64 // unix 秒
}
