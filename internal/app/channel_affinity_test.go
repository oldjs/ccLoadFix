package app

import (
	"sync"
	"testing"
	"time"

	modelpkg "ccLoad/internal/model"
)

func TestChannelAffinity_SetAndGet(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 600 * time.Second

	// 空查询
	_, ok := ca.Get("claude-sonnet-4-20250514", ttl)
	if ok {
		t.Fatal("expected no affinity for unset model")
	}

	// 设置并查询
	ca.Set("claude-sonnet-4-20250514", 42)
	id, ok := ca.Get("claude-sonnet-4-20250514", ttl)
	if !ok || id != 42 {
		t.Fatalf("expected channelID=42, got %d ok=%v", id, ok)
	}

	// 覆盖写入
	ca.Set("claude-sonnet-4-20250514", 99)
	id, ok = ca.Get("claude-sonnet-4-20250514", ttl)
	if !ok || id != 99 {
		t.Fatalf("expected channelID=99 after overwrite, got %d", id)
	}
}

func TestChannelAffinity_TTLExpiry(t *testing.T) {
	ca := NewChannelAffinity()

	// 手动设置一个过去的时间
	ca.mu.Lock()
	ca.affinities["gpt-4o"] = &channelAffinityEntry{
		channelID: 10,
		updatedAt: time.Now().Add(-11 * time.Minute),
	}
	ca.mu.Unlock()

	// 用 600 秒 TTL 查，应该过期（11分钟 > 10分钟）
	_, ok := ca.Get("gpt-4o", 600*time.Second)
	if ok {
		t.Fatal("expected TTL expiry, but got affinity")
	}

	// 用 15 分钟 TTL 查，应该还在
	id, ok := ca.Get("gpt-4o", 15*time.Minute)
	if !ok || id != 10 {
		t.Fatalf("expected channelID=10 with longer TTL, got %d ok=%v", id, ok)
	}
}

func TestChannelAffinity_ClearByModel(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 600 * time.Second

	ca.Set("model-a", 1)
	ca.Set("model-b", 2)

	// Clear 只在 channelID 匹配时清
	ca.Clear("model-a", 999) // 不匹配，不清
	_, ok := ca.Get("model-a", ttl)
	if !ok {
		t.Fatal("Clear with wrong channelID should not remove entry")
	}

	// 匹配时清
	ca.Clear("model-a", 1)
	_, ok = ca.Get("model-a", ttl)
	if ok {
		t.Fatal("Clear with matching channelID should remove entry")
	}

	// model-b 不受影响
	id, ok := ca.Get("model-b", ttl)
	if !ok || id != 2 {
		t.Fatal("model-b should not be affected")
	}
}

func TestChannelAffinity_ClearByChannel(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 600 * time.Second

	// 多个 model 指向同一个 channel
	ca.Set("model-a", 5)
	ca.Set("model-b", 5)
	ca.Set("model-c", 10)

	ca.ClearByChannel(5)

	_, ok := ca.Get("model-a", ttl)
	if ok {
		t.Fatal("model-a should be cleared (channel 5)")
	}
	_, ok = ca.Get("model-b", ttl)
	if ok {
		t.Fatal("model-b should be cleared (channel 5)")
	}

	// model-c 指向 channel 10，不受影响
	id, ok := ca.Get("model-c", ttl)
	if !ok || id != 10 {
		t.Fatal("model-c (channel 10) should not be affected")
	}
}

func TestChannelAffinity_Cleanup(t *testing.T) {
	ca := NewChannelAffinity()

	// 一个新鲜的，一个过期的
	ca.Set("fresh", 1)
	ca.mu.Lock()
	ca.affinities["stale"] = &channelAffinityEntry{
		channelID: 2,
		updatedAt: time.Now().Add(-11 * time.Minute),
	}
	ca.mu.Unlock()

	ca.Cleanup(600 * time.Second)

	_, ok := ca.Get("fresh", 600*time.Second)
	if !ok {
		t.Fatal("fresh entry should survive cleanup")
	}
	_, ok = ca.Get("stale", 15*time.Minute)
	if ok {
		t.Fatal("stale entry should be cleaned up")
	}
}

func TestChannelAffinity_ListAll(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 600 * time.Second

	ca.Set("model-x", 1)
	ca.Set("model-y", 2)

	// 加一个过期的（11分钟 > 10分钟TTL）
	ca.mu.Lock()
	ca.affinities["expired"] = &channelAffinityEntry{
		channelID: 3,
		updatedAt: time.Now().Add(-11 * time.Minute),
	}
	ca.mu.Unlock()

	list := ca.ListAll(ttl)
	if len(list) != 2 {
		t.Fatalf("expected 2 active entries, got %d", len(list))
	}

	// 过期的不应出现
	for _, s := range list {
		if s.Model == "expired" {
			t.Fatal("expired entry should not appear in ListAll")
		}
		if s.TTLMs <= 0 {
			t.Fatalf("TTL should be positive for active entry, got %d", s.TTLMs)
		}
	}
}

func TestChannelAffinity_Concurrent(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 600 * time.Second

	var wg sync.WaitGroup
	// 并发写
	for i := range 100 {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ca.Set("concurrent-model", id)
		}(int64(i))
	}
	wg.Wait()

	// 最终应该有一个值
	id, ok := ca.Get("concurrent-model", ttl)
	if !ok {
		t.Fatal("should have an affinity after concurrent writes")
	}
	if id < 0 || id >= 100 {
		t.Fatalf("unexpected channelID: %d", id)
	}

	// 并发读写混合
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ca.Set("mixed-model", 42)
		}()
		go func() {
			defer wg.Done()
			ca.Get("mixed-model", ttl)
		}()
	}
	wg.Wait()
}

// TestApplyChannelAffinity_TopBucketOnly 测试亲和仅在 top priority bucket 内生效
func TestApplyChannelAffinity_TopBucketOnly(t *testing.T) {
	ca := NewChannelAffinity()

	// 构造 Server（最小化，不需要 store）
	s := &Server{
		channelAffinity: ca,
		// healthCache 为 nil → getEffPriorityBucket 用 basePriority
	}

	// 三个渠道，priority 1000（同桶）
	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}
	ch3 := &modelpkg.Config{ID: 3, Priority: 1000}

	// 设置亲和到 ch3
	ca.Set("test-model", 3)

	candidates := []*modelpkg.Config{ch1, ch2, ch3}
	result := s.applyChannelAffinity(candidates, "test-model")

	if result[0].ID != 3 {
		t.Fatalf("expected ch3 (affinity) at position 0, got ch%d", result[0].ID)
	}
}

// TestApplyChannelAffinity_CrossBucketNoEffect 测试亲和渠道不在 top bucket 时不生效
func TestApplyChannelAffinity_CrossBucketNoEffect(t *testing.T) {
	ca := NewChannelAffinity()

	s := &Server{
		channelAffinity: ca,
	}

	// ch1 priority=1000, ch2 priority=500 (不同桶)
	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 500}

	// 亲和设到低优先级的 ch2
	ca.Set("test-model", 2)

	candidates := []*modelpkg.Config{ch1, ch2}
	result := s.applyChannelAffinity(candidates, "test-model")

	// ch1 应该还在首位，亲和不能跨桶提升
	if result[0].ID != 1 {
		t.Fatalf("expected ch1 to stay at position 0 (cross-bucket), got ch%d", result[0].ID)
	}
}

// TestApplyChannelAffinity_NoAffinity 无亲和时原样返回
func TestApplyChannelAffinity_NoAffinity(t *testing.T) {
	ca := NewChannelAffinity()
	s := &Server{channelAffinity: ca}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	candidates := []*modelpkg.Config{ch1, ch2}
	result := s.applyChannelAffinity(candidates, "unknown-model")

	if result[0].ID != 1 {
		t.Fatalf("expected original order preserved, got ch%d at position 0", result[0].ID)
	}
}

// TestApplyChannelAffinity_SingleCandidate 单个候选不做任何处理
func TestApplyChannelAffinity_SingleCandidate(t *testing.T) {
	ca := NewChannelAffinity()
	s := &Server{channelAffinity: ca}

	ca.Set("m", 1)
	ch := &modelpkg.Config{ID: 1, Priority: 1000}

	result := s.applyChannelAffinity([]*modelpkg.Config{ch}, "m")
	if len(result) != 1 || result[0].ID != 1 {
		t.Fatal("single candidate should pass through unchanged")
	}
}

// TestApplyChannelAffinity_Disabled 开关关闭时不应用
func TestApplyChannelAffinity_Disabled(t *testing.T) {
	ca := NewChannelAffinity()

	// 模拟 configService 返回 disabled
	// 这里用 nil configService 测试默认行为（默认 enabled）
	s := &Server{channelAffinity: ca}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	ca.Set("m", 2)
	result := s.applyChannelAffinity([]*modelpkg.Config{ch1, ch2}, "m")

	// configService 为 nil 时默认 enabled，所以亲和应该生效
	if result[0].ID != 2 {
		t.Fatalf("with nil configService (default enabled), affinity should work, got ch%d", result[0].ID)
	}
}

// TestApplyChannelAffinity_AffinityAlreadyFirst 亲和渠道已经在首位，不做多余 swap
func TestApplyChannelAffinity_AffinityAlreadyFirst(t *testing.T) {
	ca := NewChannelAffinity()
	s := &Server{channelAffinity: ca}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	ca.Set("m", 1)
	result := s.applyChannelAffinity([]*modelpkg.Config{ch1, ch2}, "m")

	if result[0].ID != 1 {
		t.Fatalf("expected ch1 to stay first, got ch%d", result[0].ID)
	}
}

// ============================================================================
// Cross-channel warm boost（channel affinity 失效后的软兜底）
// ============================================================================

// injectWarmSlot 白盒辅助：直接把 warm slot 写进分片，便于注入自定义时间戳
func injectWarmSlot(t *testing.T, sel *URLSelector, channelID int64, model, url string, lastSuccess time.Time) {
	t.Helper()
	ak := modelAffinityKey{channelID: channelID, model: model}
	sh := sel.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	entry, ok := sh.warms[ak]
	if !ok {
		entry = &warmEntry{}
		sh.warms[ak] = entry
	}
	// 插到 head，保持 [0] 最新的不变量
	shiftLen := entry.count
	if shiftLen >= warmCapacity {
		shiftLen = warmCapacity - 1
	}
	if shiftLen > 0 {
		copy(entry.slots[1:shiftLen+1], entry.slots[0:shiftLen])
	}
	entry.slots[0] = warmSlot{url: url, lastSuccess: lastSuccess}
	if entry.count < warmCapacity {
		entry.count++
	}
}

// injectURLCooldown 白盒辅助：把指定 URL 放进冷却
func injectURLCooldown(t *testing.T, sel *URLSelector, channelID int64, url string, until time.Time) {
	t.Helper()
	sh := sel.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.cooldowns[urlKey{channelID: channelID, url: url}] = urlCooldownState{until: until}
}

// TestPickCrossChannelWarmBoost_FreshWarmInOther 其他 channel 有新鲜 warm → 返回该 idx + 强档概率
func TestPickCrossChannelWarmBoost_FreshWarmInOther(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}
	ch3 := &modelpkg.Config{ID: 3, Priority: 1000}

	now := time.Now()
	// ch3 有 2 分钟前的 warm（强档：<5min）
	injectWarmSlot(t, s.urlSelector, 3, "m", "https://ch3.example.com", now.Add(-2*time.Minute))

	idx, prob := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2, ch3}, "m", now)
	if idx != 2 {
		t.Fatalf("expected bestIdx=2 (ch3), got %d", idx)
	}
	if prob != warmBoostProbStrong {
		t.Fatalf("expected strong prob %v, got %v", warmBoostProbStrong, prob)
	}
}

// TestPickCrossChannelWarmBoost_WeakWindow 10-30 分钟之间 → 弱档概率
func TestPickCrossChannelWarmBoost_WeakWindow(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	now := time.Now()
	// ch2 有 20 分钟前的 warm（弱档：10-30min）
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", now.Add(-20*time.Minute))

	idx, prob := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", now)
	if idx != 1 {
		t.Fatalf("expected bestIdx=1, got %d", idx)
	}
	if prob != warmBoostProbWeak {
		t.Fatalf("expected weak prob %v, got %v", warmBoostProbWeak, prob)
	}
}

// TestPickCrossChannelWarmBoost_StaleWarm warm 超 30min → 不 boost
func TestPickCrossChannelWarmBoost_StaleWarm(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	now := time.Now()
	// ch2 的 warm 是 45 分钟前（超过 warmEntryTTL，实际 GetFreshWarmURL 会先 filter 掉）
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", now.Add(-45*time.Minute))

	idx, _ := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", now)
	if idx != -1 {
		t.Fatalf("expected -1 (stale warm filtered), got %d", idx)
	}
}

// TestPickCrossChannelWarmBoost_URLCooledDown warm URL 在冷却 → 不 boost
func TestPickCrossChannelWarmBoost_URLCooledDown(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	now := time.Now()
	// ch2 的 warm 很新，但 URL 正在冷却
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", now.Add(-1*time.Minute))
	injectURLCooldown(t, s.urlSelector, 2, "https://ch2.example.com", now.Add(30*time.Second))

	idx, _ := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", now)
	if idx != -1 {
		t.Fatalf("expected -1 (cooled URL), got %d", idx)
	}
}

// TestPickCrossChannelWarmBoost_CrossBucket warm channel 在低桶 → 不 boost
func TestPickCrossChannelWarmBoost_CrossBucket(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	// ch1 priority=1000 (top), ch2 priority=500 (低桶)
	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 500}

	now := time.Now()
	// ch2 有新鲜 warm，但在低桶，不应被提权
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", now.Add(-1*time.Minute))

	idx, _ := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", now)
	if idx != -1 {
		t.Fatalf("expected -1 (cross-bucket), got %d", idx)
	}
}

// TestPickCrossChannelWarmBoost_AlreadyFirst 最佳 warm 本来就在首位 → 不做动作
func TestPickCrossChannelWarmBoost_AlreadyFirst(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	now := time.Now()
	// ch1 已在首位且有新鲜 warm
	injectWarmSlot(t, s.urlSelector, 1, "m", "https://ch1.example.com", now.Add(-1*time.Minute))

	idx, prob := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", now)
	// bestIdx=0 + prob=0 表示"找到了但本来就在首位，什么都不用做"
	if idx != 0 || prob != 0 {
		t.Fatalf("expected (0, 0) for already-first, got (%d, %v)", idx, prob)
	}
}

// TestPickCrossChannelWarmBoost_PicksFreshest 多个 channel 都有 warm，选最新鲜的那个
func TestPickCrossChannelWarmBoost_PicksFreshest(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}
	ch3 := &modelpkg.Config{ID: 3, Priority: 1000}

	now := time.Now()
	// ch2 较老（20min，弱档），ch3 最新（30s，强档）
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", now.Add(-20*time.Minute))
	injectWarmSlot(t, s.urlSelector, 3, "m", "https://ch3.example.com", now.Add(-30*time.Second))

	idx, prob := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2, ch3}, "m", now)
	if idx != 2 {
		t.Fatalf("expected bestIdx=2 (ch3 freshest), got %d", idx)
	}
	if prob != warmBoostProbStrong {
		t.Fatalf("expected strong prob, got %v", prob)
	}
}

// TestPickCrossChannelWarmBoost_NoURLSelector urlSelector 未初始化 → 不 boost
func TestPickCrossChannelWarmBoost_NoURLSelector(t *testing.T) {
	s := &Server{channelAffinity: NewChannelAffinity()} // 无 urlSelector

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	idx, _ := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "m", time.Now())
	if idx != -1 {
		t.Fatalf("expected -1 (no selector), got %d", idx)
	}
}

// TestPickCrossChannelWarmBoost_EmptyModel 空 model → 不 boost
func TestPickCrossChannelWarmBoost_EmptyModel(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}
	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}

	idx, _ := s.pickCrossChannelWarmBoostTarget([]*modelpkg.Config{ch1, ch2}, "", time.Now())
	if idx != -1 {
		t.Fatalf("expected -1 (empty model), got %d", idx)
	}
}

// TestApplyCrossChannelWarmBoost_Probabilistic 采样 swap 的概率分布在合理范围内
// 强档 75% → 400 次中期望 300，容忍区间 60%-90% 避免 flaky
func TestApplyCrossChannelWarmBoost_Probabilistic(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}
	// ch2 有强档 warm（2min 内）
	injectWarmSlot(t, s.urlSelector, 2, "m", "https://ch2.example.com", time.Now().Add(-2*time.Minute))

	const trials = 400
	swapped := 0
	for range trials {
		candidates := []*modelpkg.Config{ch1, ch2}
		result := s.applyCrossChannelWarmBoost(candidates, "m")
		if result[0].ID == 2 {
			swapped++
		}
	}

	// 强档 75% → 期望 300，宽松到 60%-90% 避免 flaky（≈ ±7σ）
	minSwap, maxSwap := trials*6/10, trials*9/10
	if swapped < minSwap || swapped > maxSwap {
		t.Fatalf("swap rate out of range: got %d/%d (expected %d-%d)", swapped, trials, minSwap, maxSwap)
	}
}

// TestApplyChannelAffinity_MissFallsThroughToWarmBoost 亲和 miss 时走 warm 兜底路径
// 仅验证：没有 panic、不会因为 miss 而无限循环、warm 可被选中
func TestApplyChannelAffinity_MissFallsThroughToWarmBoost(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	ch1 := &modelpkg.Config{ID: 1, Priority: 1000}
	ch2 := &modelpkg.Config{ID: 2, Priority: 1000}
	// 不 Set 任何 channel affinity → Get 会 miss
	// ch2 有新鲜 warm
	injectWarmSlot(t, s.urlSelector, 2, "warm-model", "https://ch2.example.com", time.Now().Add(-1*time.Minute))

	// 多次调用，ch2 至少被 swap 到首位一次（强档 50% 概率，50 次跑 miss 概率极低）
	sawCh2First := false
	for range 50 {
		candidates := []*modelpkg.Config{ch1, ch2}
		result := s.applyChannelAffinity(candidates, "warm-model")
		if result[0].ID == 2 {
			sawCh2First = true
			break
		}
	}
	if !sawCh2First {
		t.Fatal("expected warm boost to swap ch2 to front at least once in 50 trials")
	}
}

// TestGetFreshWarmURL_Basic 基础返回
func TestGetFreshWarmURL_Basic(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	injectWarmSlot(t, sel, 1, "m", "https://u1.example.com", now.Add(-2*time.Minute))

	url, age, ok := sel.GetFreshWarmURL(1, "m", now)
	if !ok {
		t.Fatal("expected warm hit")
	}
	if url != "https://u1.example.com" {
		t.Fatalf("unexpected url: %s", url)
	}
	if age < time.Minute || age > 3*time.Minute {
		t.Fatalf("unexpected age: %v", age)
	}
}

// TestGetFreshWarmURL_Expired 超 TTL 时不返回
func TestGetFreshWarmURL_Expired(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	// 45 分钟前，超过 warmEntryTTL (30min)
	injectWarmSlot(t, sel, 1, "m", "https://u1.example.com", now.Add(-45*time.Minute))

	_, _, ok := sel.GetFreshWarmURL(1, "m", now)
	if ok {
		t.Fatal("expected miss for expired warm")
	}
}

// TestGetFreshWarmURL_CooledDownSkipped URL 冷却时跳过该 slot
func TestGetFreshWarmURL_CooledDownSkipped(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	injectWarmSlot(t, sel, 1, "m", "https://u1.example.com", now.Add(-1*time.Minute))
	injectURLCooldown(t, sel, 1, "https://u1.example.com", now.Add(1*time.Minute))

	_, _, ok := sel.GetFreshWarmURL(1, "m", now)
	if ok {
		t.Fatal("expected miss when URL in cooldown")
	}
}

// TestListWarmBoostCandidates_TierAndState 覆盖档位判断和 affinity 遮蔽状态
func TestListWarmBoostCandidates_TierAndState(t *testing.T) {
	s := &Server{
		channelAffinity: NewChannelAffinity(),
		urlSelector:     NewURLSelector(),
	}

	now := time.Now()

	// chA: model-x 有 2 分钟 warm → 强档 + effective（无亲和）
	injectWarmSlot(t, s.urlSelector, 1, "model-x", "https://a.example.com", now.Add(-2*time.Minute))

	// chB: model-y 有 20 分钟 warm → 弱档 + effective
	injectWarmSlot(t, s.urlSelector, 2, "model-y", "https://b.example.com", now.Add(-20*time.Minute))

	// chC: model-z 有 3 分钟 warm，但 model-z 亲和活跃在 chC 自己 → masked
	injectWarmSlot(t, s.urlSelector, 3, "model-z", "https://c.example.com", now.Add(-3*time.Minute))
	s.channelAffinity.Set("model-z", 3)

	// chD: model-q 有 40 分钟 warm（超 TTL）→ 被过滤不出现
	injectWarmSlot(t, s.urlSelector, 4, "model-q", "https://d.example.com", now.Add(-40*time.Minute))

	list := s.ListWarmBoostCandidates(now)

	// 预期 3 条：model-x/strong/effective, model-y/weak/effective, model-z/strong/masked
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(list), list)
	}

	byModel := make(map[string]WarmBoostCandidateStatus, len(list))
	for _, it := range list {
		byModel[it.Model] = it
	}

	if got := byModel["model-x"]; got.Tier != "strong" || got.BoostProb != warmBoostProbStrong || !got.Effective || got.AffinityActive {
		t.Fatalf("model-x: expected strong+effective, got %+v", got)
	}
	if got := byModel["model-y"]; got.Tier != "weak" || got.BoostProb != warmBoostProbWeak || !got.Effective || got.AffinityActive {
		t.Fatalf("model-y: expected weak+effective, got %+v", got)
	}
	if got := byModel["model-z"]; got.Tier != "strong" || got.Effective || !got.AffinityActive || got.AffinityChannelID != 3 {
		t.Fatalf("model-z: expected strong+masked+affinity_ch=3, got %+v", got)
	}
	if _, ok := byModel["model-q"]; ok {
		t.Fatal("model-q with stale warm should be filtered")
	}
}

// TestListWarmBoostCandidates_Empty 未初始化 urlSelector 时返回 nil，避免 panic
func TestListWarmBoostCandidates_Empty(t *testing.T) {
	s := &Server{channelAffinity: NewChannelAffinity()}
	list := s.ListWarmBoostCandidates(time.Now())
	if list != nil {
		t.Fatalf("expected nil, got %+v", list)
	}
}

// TestGetFreshWarmURL_CooledDownFallsBackToNext 第一个 URL 冷却时能回退到下一个可用 slot
func TestGetFreshWarmURL_CooledDownFallsBackToNext(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	// 先插入老的 url2，再插入新的 url1（ [0]=url1 最新，[1]=url2 其次 ）
	injectWarmSlot(t, sel, 1, "m", "https://u2.example.com", now.Add(-3*time.Minute))
	injectWarmSlot(t, sel, 1, "m", "https://u1.example.com", now.Add(-1*time.Minute))
	// 把最新的 u1 放冷却
	injectURLCooldown(t, sel, 1, "https://u1.example.com", now.Add(1*time.Minute))

	url, _, ok := sel.GetFreshWarmURL(1, "m", now)
	if !ok {
		t.Fatal("expected fallback to u2")
	}
	if url != "https://u2.example.com" {
		t.Fatalf("expected u2 as fallback, got %s", url)
	}
}
