package app

import (
	"sync"
	"testing"
	"time"
)

// TestURLSmoothWeightedRR_SingleURL 单 URL 时直接返回该 URL 并记录计数
func TestURLSmoothWeightedRR_SingleURL(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	got := rr.Select(1, []string{"https://a.com"}, []int64{10})
	if got != "https://a.com" {
		t.Fatalf("expected https://a.com, got %q", got)
	}
	snap := rr.SnapshotChannel(1)
	if len(snap.URLs) != 1 || snap.URLs[0].Selections != 1 {
		t.Fatalf("expected 1 selection recorded, got %+v", snap.URLs)
	}
}

// TestURLSmoothWeightedRR_EmptyURLs 空输入应返回空字符串，不 panic
func TestURLSmoothWeightedRR_EmptyURLs(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	if got := rr.Select(1, nil, nil); got != "" {
		t.Fatalf("expected empty string for nil urls, got %q", got)
	}
	if got := rr.Select(1, []string{}, []int64{}); got != "" {
		t.Fatalf("expected empty string for empty urls, got %q", got)
	}
}

// TestURLSmoothWeightedRR_EqualWeights 等权重下严格均匀分发
// 这是 SmoothWRR 相对于加权随机的最大优势：等权重 = 严格轮询，不存在概率扎堆
func TestURLSmoothWeightedRR_EqualWeights(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"a", "b", "c", "d", "e"}
	weights := []int64{10, 10, 10, 10, 10}

	const rounds = 1000
	counts := map[string]int{}
	for range rounds {
		got := rr.Select(1, urls, weights)
		counts[got]++
	}
	expected := rounds / len(urls)
	for _, u := range urls {
		if counts[u] != expected {
			t.Errorf("equal weight: %s got %d, expected exactly %d (SmoothWRR is deterministic)", u, counts[u], expected)
		}
	}
}

// TestURLSmoothWeightedRR_WeightProportions 5:1 权重应产生 5:1 分布
func TestURLSmoothWeightedRR_WeightProportions(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"fast", "slow"}
	weights := []int64{50, 10} // 5:1

	const rounds = 600
	counts := map[string]int{}
	for range rounds {
		counts[rr.Select(1, urls, weights)]++
	}
	// 5:1 = 500:100
	if counts["fast"] != 500 || counts["slow"] != 100 {
		t.Errorf("expected 5:1 split (500/100), got fast=%d slow=%d", counts["fast"], counts["slow"])
	}
}

// TestURLSmoothWeightedRR_AllURLsAccessedPeriodically 极端权重比下，弱节点也必须被周期访问
// 这是根治"号池闲置"的核心证据
func TestURLSmoothWeightedRR_AllURLsAccessedPeriodically(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"strong", "medium", "weak"}
	weights := []int64{100, 10, 1} // 100:10:1

	const rounds = 1110 // = 100+10+1 的倍数，便于验证比例
	counts := map[string]int{}
	for range rounds {
		counts[rr.Select(1, urls, weights)]++
	}
	if counts["weak"] == 0 {
		t.Fatalf("weak URL must be selected at least once under SmoothWRR, got %v", counts)
	}
	// 严格按权重比例：1000:100:10
	if counts["strong"] != 1000 || counts["medium"] != 100 || counts["weak"] != 10 {
		t.Errorf("expected 1000:100:10 split, got %v", counts)
	}
}

// TestURLSmoothWeightedRR_ZeroWeightClampedToOne 0 权重被钳到 1，保留最低存在感
func TestURLSmoothWeightedRR_ZeroWeightClampedToOne(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"main", "dead"}
	weights := []int64{99, 0} // dead 应被钳到 1

	const rounds = 500
	counts := map[string]int{}
	for range rounds {
		counts[rr.Select(1, urls, weights)]++
	}
	if counts["dead"] == 0 {
		t.Fatalf("zero-weight URL should still be selected occasionally (clamped to 1), got %v", counts)
	}
	// 99:1 = 495:5
	if counts["dead"] != 5 {
		t.Errorf("expected dead URL to be selected exactly 5 times in 500 (99:1), got %d", counts["dead"])
	}
}

// TestURLSmoothWeightedRR_PerChannelIsolation 不同 channel 的状态完全隔离
func TestURLSmoothWeightedRR_PerChannelIsolation(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"a", "b"}
	weights := []int64{1, 1}

	// 渠道 1 选 100 次
	for range 100 {
		rr.Select(1, urls, weights)
	}
	// 渠道 2 选 1 次：state 应该独立
	rr.Select(2, urls, weights)

	snap1 := rr.SnapshotChannel(1)
	snap2 := rr.SnapshotChannel(2)

	total1 := int64(0)
	for _, s := range snap1.URLs {
		total1 += s.Selections
	}
	if total1 != 100 {
		t.Errorf("channel 1: expected 100 selections, got %d", total1)
	}

	total2 := int64(0)
	for _, s := range snap2.URLs {
		total2 += s.Selections
	}
	if total2 != 1 {
		t.Errorf("channel 2: expected 1 selection, got %d", total2)
	}
}

// TestURLSmoothWeightedRR_PruneChannel 剪枝清掉指定 URL，保留其他
func TestURLSmoothWeightedRR_PruneChannel(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"a", "b", "c"}
	weights := []int64{1, 1, 1}
	for range 30 {
		rr.Select(1, urls, weights)
	}

	keep := map[string]struct{}{"a": {}, "b": {}}
	rr.PruneChannel(1, keep)

	snap := rr.SnapshotChannel(1)
	for _, s := range snap.URLs {
		if s.URL == "c" {
			t.Errorf("expected c to be pruned, still in snapshot: %+v", s)
		}
	}
}

// TestURLSmoothWeightedRR_RemoveChannel 移除整渠道状态
func TestURLSmoothWeightedRR_RemoveChannel(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	rr.Select(1, []string{"a"}, []int64{1})
	rr.RemoveChannel(1)
	snap := rr.SnapshotChannel(1)
	if len(snap.URLs) != 0 {
		t.Errorf("expected channel 1 to be empty after remove, got %+v", snap.URLs)
	}
}

// TestURLSmoothWeightedRR_Cleanup 长时间未访问的 channel state 被清理
func TestURLSmoothWeightedRR_Cleanup(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	rr.Select(1, []string{"a"}, []int64{1})

	// 模拟时间过去：手动把 lastAccess 改老
	shard := rr.shardFor(1)
	shard.mu.Lock()
	shard.states[1].lastAccess = time.Now().Add(-2 * time.Hour)
	shard.mu.Unlock()

	rr.Cleanup(time.Hour)
	snap := rr.SnapshotChannel(1)
	if len(snap.URLs) != 0 {
		t.Errorf("expected stale channel to be cleaned up, got %+v", snap.URLs)
	}
}

// TestURLSmoothWeightedRR_ConcurrentSafe 并发场景下 selection 总数严格等于调用次数
// 证明：分片锁工作正常，无丢失/重复计数
func TestURLSmoothWeightedRR_ConcurrentSafe(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"a", "b", "c", "d"}
	weights := []int64{1, 1, 1, 1}

	const goroutines = 16
	const perGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				rr.Select(1, urls, weights)
			}
		}()
	}
	wg.Wait()

	snap := rr.SnapshotChannel(1)
	total := int64(0)
	for _, s := range snap.URLs {
		total += s.Selections
	}
	expected := int64(goroutines * perGoroutine)
	if total != expected {
		t.Errorf("concurrent: expected %d total selections, got %d (lost or double-counted)", expected, total)
	}
}

// TestComputeRRWeight_FloorClampsSuspiciouslyFastURL 低延迟权重 floor 把"看起来快"
// 的 URL 权重收益封顶，防御掺假 URL 用极低 TTFB 在 SmoothWRR 下拿到超高权重。
//
// 场景：假货 URL TTFB=30ms，正常 URL TTFB=200ms，其他 URL TTFB=400ms
// 不开 floor：30ms URL 权重 33333，比 200ms 的 5000 高 6.7 倍 → 流量倾斜
// 开 floor=100ms：30ms URL 被夹到 100ms，权重 10000，与 100ms 的真实快 URL 同档 → 不再独占
func TestComputeRRWeight_FloorClampsSuspiciouslyFastURL(t *testing.T) {
	const floor = 100.0

	// 没开 floor：30ms 拿到的权重比 100ms 高 3.3 倍
	wFastNoFloor := computeRRWeight(30, 1.0, 0)
	wNormNoFloor := computeRRWeight(100, 1.0, 0)
	if wFastNoFloor <= wNormNoFloor*3 {
		t.Fatalf("baseline check failed: 30ms should get >3x weight of 100ms without floor, got fast=%d norm=%d", wFastNoFloor, wNormNoFloor)
	}

	// 开 floor=100ms：30ms 被夹到 100ms，权重等于 100ms 的真实 URL
	wFastFloored := computeRRWeight(30, 1.0, floor)
	wNormFloored := computeRRWeight(100, 1.0, floor)
	if wFastFloored != wNormFloored {
		t.Errorf("with floor=%v, suspiciously-fast 30ms URL should get same weight as 100ms URL, got fast=%d norm=%d", floor, wFastFloored, wNormFloored)
	}

	// floor 不影响 >= floor 的 URL
	wAboveFloor := computeRRWeight(200, 1.0, floor)
	wAboveNoFloor := computeRRWeight(200, 1.0, 0)
	if wAboveFloor != wAboveNoFloor {
		t.Errorf("floor should not affect URLs above floor, got with-floor=%d no-floor=%d", wAboveFloor, wAboveNoFloor)
	}
}

// TestURLSmoothWeightedRR_WeightFloorBlocksFakeURLDomination 端到端：
// 通过 SmoothWRR 选 URL 时启用 floor，验证假货 URL 不再独占流量
func TestURLSmoothWeightedRR_WeightFloorBlocksFakeURLDomination(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	rr.SetWeightFloorMs(100)

	// 模拟两个 URL：fake=30ms（假货），real=200ms（正常）
	// 在 floor=100 下，fake 被夹到 100ms 等价权重，real 是 200ms
	urls := []string{"fake", "real"}
	weights := []int64{
		computeRRWeight(30, 1.0, float64(rr.WeightFloorMs())),  // fake
		computeRRWeight(200, 1.0, float64(rr.WeightFloorMs())), // real
	}

	const rounds = 600
	counts := map[string]int{}
	for range rounds {
		counts[rr.Select(1, urls, weights)]++
	}

	// 没 floor 时 fake 应占 6.7/(6.7+1) ≈ 87%，开 floor 后应降到 2:1 = 67%
	fakeShare := float64(counts["fake"]) / float64(rounds)
	if fakeShare > 0.70 {
		t.Errorf("with weight floor, fake URL should not dominate; got %.1f%% (counts=%v)", fakeShare*100, counts)
	}
	if counts["real"] == 0 {
		t.Errorf("real URL should still be selected periodically, got %v", counts)
	}
}

// TestURLSmoothWeightedRR_SetWeightFloorMs 验证 Set/Get 线程安全
func TestURLSmoothWeightedRR_SetWeightFloorMs(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	if got := rr.WeightFloorMs(); got != 0 {
		t.Errorf("default floor should be 0, got %d", got)
	}
	rr.SetWeightFloorMs(150)
	if got := rr.WeightFloorMs(); got != 150 {
		t.Errorf("expected 150, got %d", got)
	}
	// 负值钳到 0
	rr.SetWeightFloorMs(-1)
	if got := rr.WeightFloorMs(); got != 0 {
		t.Errorf("negative floor should clamp to 0, got %d", got)
	}
}

// TestURLSmoothWeightedRR_LastSelectedTracking 记录每个 URL 的最后选中时间，闲置识别用
func TestURLSmoothWeightedRR_LastSelectedTracking(t *testing.T) {
	rr := NewURLSmoothWeightedRR()
	urls := []string{"a", "b"}
	weights := []int64{100, 1} // a 几乎独占，b 几百轮才被选一次

	before := time.Now()
	rr.Select(1, urls, weights)
	after := time.Now()

	snap := rr.SnapshotChannel(1)
	var aStat URLSelectionStat
	for _, s := range snap.URLs {
		if s.URL == "a" {
			aStat = s
		}
	}
	if aStat.LastSelectedAtMs < before.UnixMilli() || aStat.LastSelectedAtMs > after.UnixMilli() {
		t.Errorf("LastSelectedAtMs (%d) out of range [%d, %d]", aStat.LastSelectedAtMs, before.UnixMilli(), after.UnixMilli())
	}
	if aStat.IdleMs < 0 {
		t.Errorf("IdleMs should be >=0 after selection, got %d", aStat.IdleMs)
	}
}

// ============================================================
// 集成测试：高并发场景下的 URL 分布
// ============================================================

// TestURLSelector_HighConcurrency_DistributesAcrossLargePool 高并发 + 大号池场景，
// 验证根治效果：100 个 URL，800 个并发请求，每个 URL 都应被选到（号池均匀利用）。
//
// 这是用户报告问题的直接复现 + 修复证据。
func TestURLSelector_HighConcurrency_DistributesAcrossLargePool(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(42)
	model := "claude-sonnet-4-6"

	// 100 个 URL，预先注入相近的延迟（模拟探测后的状态：所有 URL 都有 EWMA 数据）
	urls := make([]string, 100)
	for i := range urls {
		urls[i] = fmtSelectorURL(i)
		sel.RecordLatency(channelID, urls[i], time.Duration(100+i%30)*time.Millisecond)
	}

	// 800 并发请求模拟生产高并发
	const totalReqs = 800
	const goroutines = 50
	per := totalReqs / goroutines

	var mu sync.Mutex
	counts := map[string]int{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				picked, _ := sel.SelectURLForModel(channelID, model, urls)
				mu.Lock()
				counts[picked]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// 关键断言：所有 100 个 URL 都被选到（号池利用率 = 100%）
	hit := 0
	for _, u := range urls {
		if counts[u] > 0 {
			hit++
		}
	}
	if hit != len(urls) {
		t.Fatalf("expected all %d URLs to be selected at least once, only %d were used (pool starvation regression!)", len(urls), hit)
	}

	// 关键断言：没有 URL 独占超过总流量的 5%（即使最快的 URL 也要分流）
	for u, c := range counts {
		ratio := float64(c) / float64(totalReqs)
		if ratio > 0.05 {
			t.Errorf("URL %s got %.1f%% of traffic (>5%%), SmoothWRR fairness regressed: %d/%d", u, ratio*100, c, totalReqs)
		}
	}
}

func fmtSelectorURL(i int) string {
	return "https://upstream-" + intToStr(i) + ".example.com"
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{'0' + byte(i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
