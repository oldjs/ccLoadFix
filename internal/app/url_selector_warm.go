package app

import "time"

// warm 备选列表：主亲和失效那次请求的 fallback 首跳候选。
// 现状（无 warm）：丢亲和后 buildPlannedOrder 用 weightedRandomCandidate 抽首跳，
// EWMA 分数接近时首跳概率分散，经常抽到次优 URL，TTFB 明显抬升。
// warm 记住最近 N 次成功过的 URL，主亲和丢失时硬选 warm 作为首跳，躲开随机抖动。

const (
	// warmCapacity 每个 (channel, model) 最多保留几个 warm URL。
	// 3 足够覆盖"主 + 备 + 备"三档，再多没意义还占内存。
	warmCapacity = 3

	// warmEntryTTL slot.lastSuccess 超过这个时间就当过期，不再作为 warm 命中。
	// 亲和 TTL 24h 已经很长，warm 再守 30 分钟就够了，避免凭老数据误导。
	warmEntryTTL = 30 * time.Minute
)

// warmSlot 单个 warm URL 记录。
type warmSlot struct {
	url         string
	lastSuccess time.Time
}

// warmEntry 定长槽位，[0] 最新，按 lastSuccess 倒序。
// 用定长数组不用切片：一次请求热路径零分配、CPU cache 友好。
type warmEntry struct {
	slots [warmCapacity]warmSlot
	count uint8
}

// pushWarmInShard 把 url 推入 (channel, model) 的 warm list。调用方已持写锁。
// 已存在 → move-to-front；不存在 → 头插，超容挤掉最老的。
func pushWarmInShard(sh *urlShard, ak modelAffinityKey, rawURL string, now time.Time) {
	if rawURL == "" {
		return
	}
	entry, ok := sh.warms[ak]
	if !ok {
		entry = &warmEntry{}
		sh.warms[ak] = entry
	}

	// 已存在就前移，刷新时间戳
	for i := uint8(0); i < entry.count; i++ {
		if entry.slots[i].url == rawURL {
			if i > 0 {
				copy(entry.slots[1:i+1], entry.slots[0:i])
			}
			entry.slots[0] = warmSlot{url: rawURL, lastSuccess: now}
			return
		}
	}

	// 新 URL 插 head，其他下移
	shiftLen := entry.count
	if shiftLen >= warmCapacity {
		shiftLen = warmCapacity - 1
	}
	if shiftLen > 0 {
		copy(entry.slots[1:shiftLen+1], entry.slots[0:shiftLen])
	}
	entry.slots[0] = warmSlot{url: rawURL, lastSuccess: now}
	if entry.count < warmCapacity {
		entry.count++
	}
}

// removeWarmSlot 从 entry 中删掉指定 URL 的 slot，后面的往前挤一位。
func removeWarmSlot(entry *warmEntry, rawURL string) bool {
	for i := uint8(0); i < entry.count; i++ {
		if entry.slots[i].url == rawURL {
			if i < entry.count-1 {
				copy(entry.slots[i:entry.count-1], entry.slots[i+1:entry.count])
			}
			entry.slots[entry.count-1] = warmSlot{}
			entry.count--
			return true
		}
	}
	return false
}

// removeWarmURLInChannel 清掉 channel 下所有 model 的 warm 中匹配的 URL。
// 调用点：CooldownURL / SuspectLowLatencyCooldown / RecordModelNotFound 触发冷却。
// 成本：O(活跃 model 数 × warmCapacity)，单渠道下通常不大。调用方已持写锁。
func removeWarmURLInChannel(sh *urlShard, channelID int64, rawURL string) {
	if rawURL == "" {
		return
	}
	for key, entry := range sh.warms {
		if key.channelID != channelID || entry == nil {
			continue
		}
		if removeWarmSlot(entry, rawURL) && entry.count == 0 {
			delete(sh.warms, key)
		}
	}
}

// removeWarmURLForModelOnly 只清 (channel, model) 维度的 warm 中该 URL。
// 调用点：MarkNoThinking —— 黑名单是 (URL, model) 粒度，别误伤其他 model 的 warm。
func removeWarmURLForModelOnly(sh *urlShard, ak modelAffinityKey, rawURL string) {
	if rawURL == "" {
		return
	}
	entry, ok := sh.warms[ak]
	if !ok || entry == nil {
		return
	}
	if removeWarmSlot(entry, rawURL) && entry.count == 0 {
		delete(sh.warms, ak)
	}
}

// pruneWarmsForChannel 渠道 URL 列表变更时，剔除不再存在的 URL。
// keep 为空集时等价于清空该渠道 warm。
func pruneWarmsForChannel(sh *urlShard, channelID int64, keep map[string]struct{}) {
	for key, entry := range sh.warms {
		if key.channelID != channelID || entry == nil {
			continue
		}
		// 原地过滤：逐 slot 检查，不在 keep 集合的删掉
		for i := uint8(0); i < entry.count; {
			if _, ok := keep[entry.slots[i].url]; ok {
				i++
				continue
			}
			if i < entry.count-1 {
				copy(entry.slots[i:entry.count-1], entry.slots[i+1:entry.count])
			}
			entry.slots[entry.count-1] = warmSlot{}
			entry.count--
		}
		if entry.count == 0 {
			delete(sh.warms, key)
		}
	}
}

// removeWarmsForChannel 整渠道清空。RemoveChannel 调用。
func removeWarmsForChannel(sh *urlShard, channelID int64) {
	for key := range sh.warms {
		if key.channelID == channelID {
			delete(sh.warms, key)
		}
	}
}

// gcWarms 扫过期 entry（所有 slot 都超 TTL）。调用方已持写锁。
func gcWarms(sh *urlShard, now time.Time) {
	cutoff := now.Add(-warmEntryTTL)
	for key, entry := range sh.warms {
		if entry == nil || entry.count == 0 {
			delete(sh.warms, key)
			continue
		}
		// 按 slot 顺序逐个检查；倒序裁剪过期槽位（lastSuccess 越靠后越老）
		allExpired := true
		for i := uint8(0); i < entry.count; i++ {
			if !entry.slots[i].lastSuccess.Before(cutoff) {
				allExpired = false
				break
			}
		}
		if allExpired {
			delete(sh.warms, key)
		}
	}
}

// markWarmCandidate 扫 candidates，给 warm list 里"最新未过期 & 可用"的首个 URL 打 warm 标记。
// 只标一个，用于 pickFirstHop 硬选首跳。调用方已持读锁。
// 返回是否命中，便于上层决定是否跳过 canary 探索。
func markWarmCandidate(sh *urlShard, channelID int64, model string, candidates []selectorCandidate, now time.Time) bool {
	if model == "" {
		return false
	}
	ak := modelAffinityKey{channelID: channelID, model: model}
	entry, ok := sh.warms[ak]
	if !ok || entry == nil || entry.count == 0 {
		return false
	}
	cutoff := now.Add(-warmEntryTTL)
	// slot 已经按 lastSuccess 倒序存放，[0] 最新 —— 直接顺序扫
	for i := uint8(0); i < entry.count; i++ {
		slot := entry.slots[i]
		if slot.lastSuccess.Before(cutoff) {
			continue
		}
		// 在 candidates 里找 URL 相同且"当前可用"的条目
		// 冷却/慢隔离/thinking 黑名单已经在 buildCandidate 里打了 cooled 标记，这里跳过
		for j := range candidates {
			c := candidates[j]
			if c.url != slot.url {
				continue
			}
			if c.cooled || c.noThinkingBlocked {
				break // 同一 URL 不会再匹配到，直接跳出内层找下一个 slot
			}
			candidates[j].warm = true
			return true
		}
	}
	return false
}
