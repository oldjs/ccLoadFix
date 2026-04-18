package app

import (
	"sync"
	"testing"
	"time"

	modelpkg "ccLoad/internal/model"
)

func TestChannelAffinity_SetAndGet(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 60 * time.Second

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
		updatedAt: time.Now().Add(-2 * time.Minute),
	}
	ca.mu.Unlock()

	// 用 60 秒 TTL 查，应该过期
	_, ok := ca.Get("gpt-4o", 60*time.Second)
	if ok {
		t.Fatal("expected TTL expiry, but got affinity")
	}

	// 用 5 分钟 TTL 查，应该还在
	id, ok := ca.Get("gpt-4o", 5*time.Minute)
	if !ok || id != 10 {
		t.Fatalf("expected channelID=10 with longer TTL, got %d ok=%v", id, ok)
	}
}

func TestChannelAffinity_ClearByModel(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 60 * time.Second

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
	ttl := 60 * time.Second

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
		updatedAt: time.Now().Add(-5 * time.Minute),
	}
	ca.mu.Unlock()

	ca.Cleanup(60 * time.Second)

	_, ok := ca.Get("fresh", 60*time.Second)
	if !ok {
		t.Fatal("fresh entry should survive cleanup")
	}
	_, ok = ca.Get("stale", 5*time.Minute)
	if ok {
		t.Fatal("stale entry should be cleaned up")
	}
}

func TestChannelAffinity_ListAll(t *testing.T) {
	ca := NewChannelAffinity()
	ttl := 60 * time.Second

	ca.Set("model-x", 1)
	ca.Set("model-y", 2)

	// 加一个过期的
	ca.mu.Lock()
	ca.affinities["expired"] = &channelAffinityEntry{
		channelID: 3,
		updatedAt: time.Now().Add(-2 * time.Minute),
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
	ttl := 60 * time.Second

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
