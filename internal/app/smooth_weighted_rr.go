package app

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	modelpkg "ccLoad/internal/model"
)

// rrShardCount 分片数量，必须是2的幂
const rrShardCount = 16

// rrShard 独立分片，持有自己的锁和状态，不同分片互不竞争
type rrShard struct {
	mu     sync.Mutex
	states map[string]*rrGroupState // key: 渠道ID组合的签名
}

// SmoothWeightedRR 平滑加权轮询调度器
// 算法来源：Nginx upstream smooth weighted round-robin
// 优化：16分片锁，不同渠道组合大概率落在不同分片，高并发下减少锁竞争
type SmoothWeightedRR struct {
	shards [rrShardCount]rrShard
}

// rrGroupState 单个优先级组的轮询状态
type rrGroupState struct {
	currentWeights map[int64]int // channelID -> currentWeight
	lastAccess     time.Time     // 最后访问时间，用于过期清理
}

// NewSmoothWeightedRR 创建平滑加权轮询调度器
func NewSmoothWeightedRR() *SmoothWeightedRR {
	rr := &SmoothWeightedRR{}
	for i := range rr.shards {
		rr.shards[i].states = make(map[string]*rrGroupState)
	}
	return rr
}

// shardFor 根据 groupKey 哈希定位分片（内联 FNV-1a，零分配）
func (rr *SmoothWeightedRR) shardFor(groupKey string) *rrShard {
	h := uint32(2166136261) // FNV-1a offset basis
	for i := 0; i < len(groupKey); i++ {
		h ^= uint32(groupKey[i])
		h *= 16777619 // FNV-1a prime
	}
	return &rr.shards[h%rrShardCount]
}

// Select 从渠道列表中选择下一个渠道（平滑加权轮询）
// channels: 同优先级的渠道列表（已按优先级分组）
// weights: 每个渠道的权重（通常是有效Key数量）
// 返回: 按轮询顺序排列的渠道列表（第一个是本次选中的）
func (rr *SmoothWeightedRR) Select(
	channels []*modelpkg.Config,
	weights []int,
) []*modelpkg.Config {
	n := len(channels)
	if n == 0 {
		return channels
	}
	if len(weights) != n {
		// 参数不匹配时直接返回原列表
		return channels
	}
	if n == 1 {
		return channels
	}

	// 生成组签名（锁外完成，避免持锁做排序+拼接）
	groupKey := rr.generateGroupKey(channels)

	// 只锁目标分片，其他分片的请求不受影响
	shard := rr.shardFor(groupKey)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	// 获取或创建组状态
	state, exists := shard.states[groupKey]
	if !exists {
		state = &rrGroupState{
			currentWeights: make(map[int64]int),
		}
		shard.states[groupKey] = state
	}
	state.lastAccess = time.Now()

	// 计算总权重
	totalWeight := 0
	for _, w := range weights {
		totalWeight += w
	}
	if totalWeight == 0 {
		return channels
	}

	// Nginx 平滑加权轮询算法：
	// 1. 每个节点的 currentWeight += weight
	// 2. 选择 currentWeight 最大的节点
	// 3. 被选中节点的 currentWeight -= totalWeight

	// 步骤1: 增加权重
	for i, ch := range channels {
		state.currentWeights[ch.ID] += weights[i]
	}

	// 步骤2: 找到 currentWeight 最大的节点
	maxWeight := state.currentWeights[channels[0].ID]
	selectedIdx := 0
	for i := 1; i < n; i++ {
		cw := state.currentWeights[channels[i].ID]                                            //nolint:gosec // G602: i < n = len(channels)
		if cw > maxWeight || (cw == maxWeight && channels[i].ID < channels[selectedIdx].ID) { //nolint:gosec // G602: 同上
			maxWeight = cw
			selectedIdx = i
		}
	}

	// 步骤3: 减去总权重
	state.currentWeights[channels[selectedIdx].ID] -= totalWeight

	// 构建结果：将选中的渠道放在第一位
	result := make([]*modelpkg.Config, n)
	result[0] = channels[selectedIdx]
	idx := 1
	for i, ch := range channels {
		if i != selectedIdx {
			result[idx] = ch
			idx++
		}
	}

	return result
}

// SelectWithCooldown 带冷却感知的平滑加权轮询
// 权重 = 有效Key数量（总Key - 冷却中Key）
func (rr *SmoothWeightedRR) SelectWithCooldown(
	channels []*modelpkg.Config,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	n := len(channels)
	if n <= 1 {
		return channels
	}

	// 计算有效权重
	weights := make([]int, n)
	for i, ch := range channels {
		weights[i] = calcEffectiveKeyCount(ch, keyCooldowns, now)
	}

	return rr.Select(channels, weights)
}

// generateGroupKey 生成渠道组的唯一标识
// 使用所有渠道ID拼接，确保不同渠道组合生成不同的key。
// 规则：
// - 对 ID 排序，使同一集合不同顺序复用同一状态（避免状态爆炸）
// - 使用十进制+逗号分隔，保证可读且无歧义
func (rr *SmoothWeightedRR) generateGroupKey(channels []*modelpkg.Config) string {
	n := len(channels)
	if n == 0 {
		return ""
	}

	ids := make([]int64, 0, n)
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		ids = append(ids, ch.ID)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var b strings.Builder

	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}

// Cleanup 清理过期的轮询状态（遍历所有分片）
// 建议在后台定期调用
func (rr *SmoothWeightedRR) Cleanup(maxAge time.Duration) {
	now := time.Now()
	for i := range rr.shards {
		shard := &rr.shards[i]
		shard.mu.Lock()
		for key, state := range shard.states {
			if now.Sub(state.lastAccess) > maxAge {
				delete(shard.states, key)
			}
		}
		shard.mu.Unlock()
	}
}

// ResetAll 重置所有轮询状态（渠道配置变更时调用）
func (rr *SmoothWeightedRR) ResetAll() {
	for i := range rr.shards {
		shard := &rr.shards[i]
		shard.mu.Lock()
		shard.states = make(map[string]*rrGroupState)
		shard.mu.Unlock()
	}
}

// getState 查找指定 groupKey 的状态（仅测试用，不加锁）
func (rr *SmoothWeightedRR) getState(groupKey string) *rrGroupState {
	shard := rr.shardFor(groupKey)
	return shard.states[groupKey]
}

// stateCount 返回所有分片的状态总数（仅测试用，不加锁）
func (rr *SmoothWeightedRR) stateCount() int {
	total := 0
	for i := range rr.shards {
		total += len(rr.shards[i].states)
	}
	return total
}

// calcEffectiveKeyCount 计算渠道的有效Key数量（排除冷却中的Key）
func calcEffectiveKeyCount(cfg *modelpkg.Config, keyCooldowns map[int64]map[int]time.Time, now time.Time) int {
	total := cfg.KeyCount
	if total <= 0 {
		return 1 // 最小为1
	}

	keyMap, ok := keyCooldowns[cfg.ID]
	if !ok || len(keyMap) == 0 {
		return total // 无冷却信息，使用全部Key数量
	}

	// 统计冷却中的Key数量
	cooledCount := 0
	for _, cooldownUntil := range keyMap {
		if cooldownUntil.After(now) {
			cooledCount++
		}
	}

	effective := total - cooledCount
	if effective <= 0 {
		return 1 // 最小为1
	}
	return effective
}
