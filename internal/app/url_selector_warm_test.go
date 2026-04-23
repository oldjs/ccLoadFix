package app

import (
	"testing"
	"time"
)

// TestWarm_HitAfterAffinityCleared 主亲和被清除后，warm 命中的 URL 应作为确定性首跳，
// 不再受 weighted-random 的概率偏差影响。
func TestWarm_HitAfterAffinityCleared(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// 先让三个 URL 都有 EWMA，A 最快
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 150*time.Millisecond)
	sel.RecordLatency(1, "https://c.com", 200*time.Millisecond)

	// 成功过的 A 进入亲和 + warm
	sel.SetModelAffinity(1, "gpt-4", "https://a.com")
	// 主亲和丢失（模拟一次失败触发 Clear）
	sel.ClearModelAffinity(1, "gpt-4", "https://a.com")

	// warm 应该稳定把 A 作为首跳，100 次应该全是 A
	for range 100 {
		selected, _ := sel.SelectURLForModel(1, "gpt-4", urls)
		if selected != "https://a.com" {
			t.Fatalf("warm fallback expected A every time, got %s", selected)
		}
	}
}

// TestWarm_CooldownRemovesFromWarm URL 进冷却后必须从 warm 里剥离，
// 即便主亲和已失效也不能再把它推成首跳。
func TestWarm_CooldownRemovesFromWarm(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}

	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 400*time.Millisecond)

	sel.SetModelAffinity(1, "gpt-4", "https://a.com")
	sel.ClearModelAffinity(1, "gpt-4", "https://a.com")

	// A 进冷却 → warm 里的 A 也要被摘掉
	sel.CooldownURL(1, "https://a.com")

	// 此刻 warm 要么空、要么只剩 B；无论如何首跳不能是 A（A 已冷却进 cooledFallback）
	// 直接跑 200 轮，A 不应被选中作为首跳
	aCount := 0
	for range 200 {
		selected, _ := sel.SelectURLForModel(1, "gpt-4", urls)
		if selected == "https://a.com" {
			aCount++
		}
	}
	if aCount != 0 {
		t.Fatalf("cooled URL should not be first-hop from warm, got %d A hits", aCount)
	}
}

// TestWarm_MoveToFrontKeepsNewest 同一 URL 重复成功时移到 [0]，保持 LRU 顺序。
func TestWarm_MoveToFrontKeepsNewest(t *testing.T) {
	sel := NewURLSelector()
	ak := modelAffinityKey{channelID: 1, model: "gpt-4"}
	sh := sel.getShard(1)

	sh.mu.Lock()
	now := time.Now()
	pushWarmInShard(sh, ak, "https://a.com", now)
	pushWarmInShard(sh, ak, "https://b.com", now.Add(time.Second))
	pushWarmInShard(sh, ak, "https://c.com", now.Add(2*time.Second))
	// 再次 push A，应该前移到 [0]
	pushWarmInShard(sh, ak, "https://a.com", now.Add(3*time.Second))

	entry := sh.warms[ak]
	sh.mu.Unlock()

	if entry == nil || entry.count != 3 {
		t.Fatalf("expected 3 slots, got %+v", entry)
	}
	if entry.slots[0].url != "https://a.com" {
		t.Fatalf("expected A at [0] after move-to-front, got %s", entry.slots[0].url)
	}
	if entry.slots[1].url != "https://c.com" || entry.slots[2].url != "https://b.com" {
		t.Fatalf("expected [C, B] trailing, got [%s, %s]", entry.slots[1].url, entry.slots[2].url)
	}
}

// TestWarm_CapacityBoundedToThree 超出容量时最老的被挤掉。
func TestWarm_CapacityBoundedToThree(t *testing.T) {
	sel := NewURLSelector()
	ak := modelAffinityKey{channelID: 1, model: "gpt-4"}
	sh := sel.getShard(1)

	sh.mu.Lock()
	now := time.Now()
	pushWarmInShard(sh, ak, "https://a.com", now)
	pushWarmInShard(sh, ak, "https://b.com", now.Add(time.Second))
	pushWarmInShard(sh, ak, "https://c.com", now.Add(2*time.Second))
	pushWarmInShard(sh, ak, "https://d.com", now.Add(3*time.Second))

	entry := sh.warms[ak]
	sh.mu.Unlock()

	if entry.count != warmCapacity {
		t.Fatalf("expected count=%d, got %d", warmCapacity, entry.count)
	}
	// 最老的 A 应该被挤出，剩下 D/C/B
	for i := uint8(0); i < entry.count; i++ {
		if entry.slots[i].url == "https://a.com" {
			t.Fatalf("oldest A should be evicted, still present at slot %d", i)
		}
	}
}

// TestWarm_AffinityTakesPriorityOverWarm 主亲和仍存在时不走 warm 硬选路径。
// 行为：affinity 的 1.5x 乘数生效，weighted random 占主导（不保证全选 A）。
func TestWarm_AffinityTakesPriorityOverWarm(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}

	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 100*time.Millisecond)

	// 两次 SetModelAffinity：warm 里 [B, A]，affinity 指向 B
	sel.SetModelAffinity(1, "gpt-4", "https://a.com")
	sel.SetModelAffinity(1, "gpt-4", "https://b.com")

	// affinity 存在 → 不应走 warm 硬选。concrete 验证：B 有 1.5x 优势但 A 仍有概率被选中
	aSeen := false
	bSeen := false
	for range 500 {
		selected, _ := sel.SelectURLForModel(1, "gpt-4", urls)
		switch selected {
		case "https://a.com":
			aSeen = true
		case "https://b.com":
			bSeen = true
		}
		if aSeen && bSeen {
			break
		}
	}
	if !aSeen || !bSeen {
		t.Fatalf("affinity path should keep weighted-random distribution, aSeen=%v bSeen=%v", aSeen, bSeen)
	}
}

// TestWarm_TTLExpiry 超过 TTL 的 warm slot 不再命中。
func TestWarm_TTLExpiry(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	ak := modelAffinityKey{channelID: 1, model: "gpt-4"}
	sh := sel.getShard(1)

	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 100*time.Millisecond)

	// 手动塞一条过期的 warm
	sh.mu.Lock()
	pushWarmInShard(sh, ak, "https://a.com", time.Now().Add(-2*warmEntryTTL))
	sh.mu.Unlock()

	// 主亲和不存在 + warm slot 过期 → 回退到 weighted random，B 必须有机会被选中
	bSeen := false
	for range 500 {
		selected, _ := sel.SelectURLForModel(1, "gpt-4", urls)
		if selected == "https://b.com" {
			bSeen = true
			break
		}
	}
	if !bSeen {
		t.Fatalf("expired warm slot should not pin first-hop to A")
	}
}

// TestWarm_MarkNoThinkingOnlyAffectsModel MarkNoThinking 只清 (channel, model) 的 warm，
// 不能误伤同 channel 下其他 model 的 warm。
func TestWarm_MarkNoThinkingOnlyAffectsModel(t *testing.T) {
	sel := NewURLSelector()
	akThink := modelAffinityKey{channelID: 1, model: "thinking-model"}
	akOther := modelAffinityKey{channelID: 1, model: "other-model"}

	sel.SetModelAffinity(1, "thinking-model", "https://a.com")
	sel.SetModelAffinity(1, "other-model", "https://a.com")

	sel.MarkNoThinking(1, "https://a.com", "thinking-model")

	sh := sel.getShard(1)
	sh.mu.RLock()
	thinkEntry := sh.warms[akThink]
	otherEntry := sh.warms[akOther]
	sh.mu.RUnlock()

	if thinkEntry != nil && thinkEntry.count > 0 {
		t.Fatalf("thinking-model warm should be cleared, got %+v", thinkEntry)
	}
	if otherEntry == nil || otherEntry.count != 1 || otherEntry.slots[0].url != "https://a.com" {
		t.Fatalf("other-model warm should be intact, got %+v", otherEntry)
	}
}

// TestWarm_PruneAndRemoveChannel PruneChannel / RemoveChannel 要同步清 warm。
func TestWarm_PruneAndRemoveChannel(t *testing.T) {
	sel := NewURLSelector()

	sel.SetModelAffinity(1, "gpt-4", "https://a.com")
	sel.SetModelAffinity(1, "gpt-4", "https://b.com")
	sel.SetModelAffinity(2, "gpt-4", "https://c.com")

	// Prune：只保留 B，A 应被剔除
	sel.PruneChannel(1, []string{"https://b.com"})

	ak := modelAffinityKey{channelID: 1, model: "gpt-4"}
	sh := sel.getShard(1)
	sh.mu.RLock()
	entry := sh.warms[ak]
	sh.mu.RUnlock()
	if entry == nil || entry.count != 1 || entry.slots[0].url != "https://b.com" {
		t.Fatalf("prune should keep only B, got %+v", entry)
	}

	// RemoveChannel(2) 应该整渠道清掉
	sel.RemoveChannel(2)
	ak2 := modelAffinityKey{channelID: 2, model: "gpt-4"}
	sh2 := sel.getShard(2)
	sh2.mu.RLock()
	entry2 := sh2.warms[ak2]
	sh2.mu.RUnlock()
	if entry2 != nil {
		t.Fatalf("channel 2 warm should be wiped, got %+v", entry2)
	}
}

// TestWarm_LowLatencyGuardBlocksWarmWrite 可疑低延迟响应走 SuspectLowLatencyCooldown 路径
// 时不会触发 SetModelAffinity，warm 也就不会被写入。
// 这里直接验证 SuspectLowLatencyCooldown 本身会清 warm —— 对称保护。
func TestWarm_SuspectLowLatencyCooldownCleansWarm(t *testing.T) {
	sel := NewURLSelector()

	sel.SetModelAffinity(1, "gpt-4", "https://a.com")
	sel.SuspectLowLatencyCooldown(1, "https://a.com", 30*time.Second)

	ak := modelAffinityKey{channelID: 1, model: "gpt-4"}
	sh := sel.getShard(1)
	sh.mu.RLock()
	entry := sh.warms[ak]
	sh.mu.RUnlock()

	if entry != nil && entry.count > 0 {
		t.Fatalf("suspect-low-latency cooldown should strip URL from warm, got %+v", entry)
	}
}
