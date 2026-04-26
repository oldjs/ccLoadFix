package app

import (
	"sync"
	"sync/atomic"
	"time"
)

// URLSmoothWeightedRR URL 级平滑加权轮询调度器
//
// 算法来源：Nginx upstream smooth weighted round-robin
// 核心特性：
//   - 确定性：相同权重输入产生固定的轮询顺序，不依赖随机
//   - 周期性：所有 weight>0 的节点都被周期访问，避免加权随机的马太效应
//   - 比例平滑：被选频率严格按权重比例（5:1 权重 → 5:1 流量），但不会连续选中同一节点
//
// 设计：按 channelID 分片（16 片），同渠道的所有 URL 共享一份轮询状态
// （EWMA 延迟本身是 channel-URL 维度，不区分 model；同渠道的 model 共享 RR 状态更平滑）
type URLSmoothWeightedRR struct {
	shards [urlRRShardCount]urlRRShard

	// weightFloorMs 低延迟权重 floor（毫秒）。
	// 当某 URL 的 effectiveLatency 低于此值时，权重计算把延迟当作 floor。
	// 防御场景：掺假 URL 用极低 TTFB 冒充快速模型，绕过 low-latency guard 后仍可能在 SmoothWRR
	// 下拿到超高权重；floor 把"看起来快"的权重收益封顶，让假货拿不到流量倾斜。
	// 0 表示不启用，由 server 在启动 + 配置变更时同步。
	weightFloorMs atomic.Int64
}

const urlRRShardCount = 16

// urlRRShard 单个分片，独立锁
type urlRRShard struct {
	mu     sync.Mutex
	states map[int64]*urlRRState // channelID -> state
}

// urlRRState 单个 channel 的轮询状态
type urlRRState struct {
	// currentWeights Nginx SmoothWRR 算法的当前权重，per-URL
	currentWeights map[string]int64
	// selections 累计被选次数（用于 admin 可视化"分发情况"）
	selections map[string]int64
	// lastSelected 每个 URL 最后一次被选中的时间（用于"闲置 URL"识别）
	lastSelected map[string]time.Time
	// lastAccess 整个 state 最后访问时间，用于过期清理
	lastAccess time.Time
}

// NewURLSmoothWeightedRR 创建 URL 级平滑加权轮询调度器
func NewURLSmoothWeightedRR() *URLSmoothWeightedRR {
	rr := &URLSmoothWeightedRR{}
	for i := range rr.shards {
		rr.shards[i].states = make(map[int64]*urlRRState)
	}
	return rr
}

// shardFor 按 channelID 哈希定位分片
func (rr *URLSmoothWeightedRR) shardFor(channelID int64) *urlRRShard {
	return &rr.shards[uint64(channelID)%urlRRShardCount]
}

// SetWeightFloorMs 设置低延迟权重 floor（毫秒）。0 表示禁用。
// server 在启动和配置变更时调用，确保 floor 与 low_latency_affinity_min_ms 同步。
func (rr *URLSmoothWeightedRR) SetWeightFloorMs(ms int64) {
	if ms < 0 {
		ms = 0
	}
	rr.weightFloorMs.Store(ms)
}

// WeightFloorMs 返回当前权重 floor（毫秒）
func (rr *URLSmoothWeightedRR) WeightFloorMs() int64 {
	return rr.weightFloorMs.Load()
}

// Select 从候选 URL 里选一个，按 Nginx SmoothWRR 算法
// urls 和 weights 长度必须一致，weights 必须 >= 1（小于 1 会被钳到 1）
// 同时记录 selection count + lastSelected，用于可视化
func (rr *URLSmoothWeightedRR) Select(channelID int64, urls []string, weights []int64) string {
	n := len(urls)
	if n == 0 {
		return ""
	}
	if n == 1 {
		// 单 URL 也走计数路径，便于面板看到独 URL 渠道的分发数
		rr.recordSelection(channelID, urls[0])
		return urls[0]
	}
	if len(weights) != n {
		return urls[0]
	}

	shard := rr.shardFor(channelID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	state, ok := shard.states[channelID]
	if !ok {
		state = newURLRRState()
		shard.states[channelID] = state
	}
	now := time.Now()
	state.lastAccess = now

	// 钳到至少 1，避免 0 权重导致整池 totalWeight=0
	totalWeight := int64(0)
	normWeights := make([]int64, n)
	for i, w := range weights {
		if w < 1 {
			w = 1
		}
		normWeights[i] = w
		totalWeight += w
	}

	// Nginx SmoothWRR 三步：
	// 1) 每个节点 currentWeight += weight
	// 2) 选 currentWeight 最大的节点
	// 3) 被选中节点 currentWeight -= totalWeight
	for i, u := range urls {
		state.currentWeights[u] += normWeights[i]
	}

	selectedIdx := 0
	maxCW := state.currentWeights[urls[0]]
	for i := 1; i < n; i++ {
		cw := state.currentWeights[urls[i]]
		// tie-break：currentWeight 相同时取索引小的，确保确定性
		if cw > maxCW {
			maxCW = cw
			selectedIdx = i
		}
	}
	selected := urls[selectedIdx]
	state.currentWeights[selected] -= totalWeight

	state.selections[selected]++
	state.lastSelected[selected] = now
	return selected
}

// recordSelection 单 URL 路径不走主算法，但仍记录分发计数
func (rr *URLSmoothWeightedRR) recordSelection(channelID int64, rawURL string) {
	if rawURL == "" {
		return
	}
	shard := rr.shardFor(channelID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	state, ok := shard.states[channelID]
	if !ok {
		state = newURLRRState()
		shard.states[channelID] = state
	}
	now := time.Now()
	state.lastAccess = now
	state.selections[rawURL]++
	state.lastSelected[rawURL] = now
}

func newURLRRState() *urlRRState {
	return &urlRRState{
		currentWeights: make(map[string]int64),
		selections:     make(map[string]int64),
		lastSelected:   make(map[string]time.Time),
	}
}

// PruneChannel 清掉指定渠道里不再存在的 URL（URL 列表变更时调用）
func (rr *URLSmoothWeightedRR) PruneChannel(channelID int64, keepURLs map[string]struct{}) {
	shard := rr.shardFor(channelID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	state, ok := shard.states[channelID]
	if !ok {
		return
	}
	for url := range state.currentWeights {
		if _, keep := keepURLs[url]; !keep {
			delete(state.currentWeights, url)
		}
	}
	for url := range state.selections {
		if _, keep := keepURLs[url]; !keep {
			delete(state.selections, url)
		}
	}
	for url := range state.lastSelected {
		if _, keep := keepURLs[url]; !keep {
			delete(state.lastSelected, url)
		}
	}
}

// RemoveChannel 删除整个渠道的 RR 状态（渠道被删时调用）
func (rr *URLSmoothWeightedRR) RemoveChannel(channelID int64) {
	shard := rr.shardFor(channelID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.states, channelID)
}

// Cleanup 清理超过 maxAge 没访问的 channel 状态（防止删渠道后状态泄漏）
func (rr *URLSmoothWeightedRR) Cleanup(maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for i := range rr.shards {
		shard := &rr.shards[i]
		shard.mu.Lock()
		for cid, state := range shard.states {
			if state == nil || state.lastAccess.Before(cutoff) {
				delete(shard.states, cid)
			}
		}
		shard.mu.Unlock()
	}
}

// URLSelectionStat 单个 URL 的分发统计快照（admin 面板用）
type URLSelectionStat struct {
	URL              string `json:"url"`
	Selections       int64  `json:"selections"`          // 累计被选次数（轮询命中数）
	LastSelectedAtMs int64  `json:"last_selected_at_ms"` // Unix 毫秒，0 表示从未被选
	IdleMs           int64  `json:"idle_ms"`             // 距最后选中过去多久（毫秒），-1 表示从未被选
	CurrentWeight    int64  `json:"current_weight"`      // SmoothWRR 当前权重（可正可负）
}

// URLChannelDistribution 单个渠道的 URL 分发快照
type URLChannelDistribution struct {
	ChannelID int64              `json:"channel_id"`
	URLs      []URLSelectionStat `json:"urls"`
}

// snapshotStateLocked 把 state 的分发数据导出成快照。调用方已持有 shard 锁。
func snapshotStateLocked(channelID int64, state *urlRRState, now time.Time) URLChannelDistribution {
	out := URLChannelDistribution{ChannelID: channelID, URLs: nil}
	if state == nil {
		return out
	}
	// 三张 map 的 key 取并集，确保所有出现过的 URL 都被纳入快照
	urls := make(map[string]struct{}, len(state.selections))
	for u := range state.selections {
		urls[u] = struct{}{}
	}
	for u := range state.currentWeights {
		urls[u] = struct{}{}
	}
	for u := range state.lastSelected {
		urls[u] = struct{}{}
	}
	out.URLs = make([]URLSelectionStat, 0, len(urls))
	for u := range urls {
		stat := URLSelectionStat{
			URL:           u,
			Selections:    state.selections[u],
			CurrentWeight: state.currentWeights[u],
			IdleMs:        -1,
		}
		if ts, ok := state.lastSelected[u]; ok {
			stat.LastSelectedAtMs = ts.UnixMilli()
			stat.IdleMs = now.Sub(ts).Milliseconds()
		}
		out.URLs = append(out.URLs, stat)
	}
	return out
}

// SnapshotChannel 导出指定渠道的轮询分发数据
func (rr *URLSmoothWeightedRR) SnapshotChannel(channelID int64) URLChannelDistribution {
	shard := rr.shardFor(channelID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return snapshotStateLocked(channelID, shard.states[channelID], time.Now())
}

// SnapshotAll 导出所有渠道的分发数据（admin 面板的总览页用）
// 每片单次锁内完成快照，避免迭代过程中重入锁
func (rr *URLSmoothWeightedRR) SnapshotAll() []URLChannelDistribution {
	now := time.Now()
	var out []URLChannelDistribution
	for i := range rr.shards {
		shard := &rr.shards[i]
		shard.mu.Lock()
		for cid, state := range shard.states {
			snap := snapshotStateLocked(cid, state, now)
			if len(snap.URLs) > 0 {
				out = append(out, snap)
			}
		}
		shard.mu.Unlock()
	}
	return out
}
