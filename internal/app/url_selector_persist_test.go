package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/testutil"
)

// TestURLSelectorPersist_RoundTrip 验证主流程：写入内存 → snapshot → ReplaceAll → LoadFromStore
// 8 种 kind 都覆盖：latency / probe_latency / cooldown / slow_iso / no_thinking / affinity / warm / requests
func TestURLSelectorPersist_RoundTrip(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// 第一个实例：往内存里写各种状态
	src := NewURLSelector()
	const cid int64 = 42
	const url1 = "https://api.example.com/v1"
	const url2 = "https://backup.example.com/v1"
	const modelName = "claude-sonnet-4"

	src.RecordLatency(cid, url1, 250*time.Millisecond)
	src.RecordProbeLatency(cid, url2, 80*time.Millisecond)
	src.SetModelAffinity(cid, modelName, url1)              // 同时写 affinity + warm
	src.CooldownURL(cid, url2)                              // url2 进冷却
	src.SuspectLowLatencyCooldown(cid, url2, 5*time.Minute) // 复用 cooldowns map
	src.MarkNoThinking(cid, url1, "claude-3-haiku")         // 注意：opus-4-7 会被豁免，得换名字

	// 全量快照写到 DB
	if err := src.flush(ctx, store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// 第二个实例：从 DB 恢复
	dst := NewURLSelector()
	loaded, err := dst.LoadFromStore(ctx, store)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if loaded == 0 {
		t.Fatal("expected loaded > 0, got 0")
	}

	// 校验各项状态都被恢复
	shSrc := src.getShard(cid)
	shDst := dst.getShard(cid)

	shSrc.mu.RLock()
	shDst.mu.RLock()
	defer shSrc.mu.RUnlock()
	defer shDst.mu.RUnlock()

	// latency
	if e := shDst.latencies[urlKey{channelID: cid, url: url1}]; e == nil {
		t.Fatal("latency for url1 missing after reload")
	} else if e.value <= 0 {
		t.Fatalf("latency value not preserved: %v", e.value)
	}

	// probe_latency
	if e := shDst.probeLatencies[urlKey{channelID: cid, url: url2}]; e == nil {
		t.Fatal("probe latency for url2 missing after reload")
	}

	// cooldown
	if cd, ok := shDst.cooldowns[urlKey{channelID: cid, url: url2}]; !ok {
		t.Fatal("cooldown for url2 missing after reload")
	} else if !time.Now().Before(cd.until) {
		t.Fatalf("cooldown already expired: %v", cd.until)
	}

	// no_thinking
	if expiry, ok := shDst.noThinkingBlklist[urlModelKey{channelID: cid, url: url1, model: "claude-3-haiku"}]; !ok {
		t.Fatal("noThinking blacklist entry missing after reload")
	} else if !time.Now().Before(expiry) {
		t.Fatalf("noThinking already expired: %v", expiry)
	}

	// affinity
	ak := modelAffinityKey{channelID: cid, model: modelName}
	if aff, ok := shDst.affinities[ak]; !ok {
		t.Fatal("affinity entry missing after reload")
	} else if aff.url != url1 {
		t.Fatalf("affinity url mismatch: got %q want %q", aff.url, url1)
	}

	// warm（应该跟随 SetModelAffinity 一起写入）
	if entry, ok := shDst.warms[ak]; !ok || entry == nil || entry.count == 0 {
		t.Fatal("warm entry missing or empty after reload")
	} else if entry.slots[0].url != url1 {
		t.Fatalf("warm slot[0] url mismatch: got %q want %q", entry.slots[0].url, url1)
	}

	// requests：RecordLatency 给 url1 计了 success=1，CooldownURL 给 url2 计了 failure=1
	// 重启后 successRate 权重才不会归零
	if rc := shDst.requests[urlKey{channelID: cid, url: url1}]; rc == nil {
		t.Fatal("requests for url1 missing after reload")
	} else if rc.success != 1 || rc.failure != 0 {
		t.Fatalf("url1 requests: got success=%d failure=%d, want 1/0", rc.success, rc.failure)
	}
	if rc := shDst.requests[urlKey{channelID: cid, url: url2}]; rc == nil {
		t.Fatal("requests for url2 missing after reload")
	} else if rc.failure != 1 {
		t.Fatalf("url2 requests failure: got %d, want >=1", rc.failure)
	}
}

// TestURLSelectorPersist_RequestsSkippedWithoutLatency 没 latency/probeLatency 数据的 URL，
// 它累计的 requests 不应被持久化（避免给已被 GC 的死 URL 留累计计数）
func TestURLSelectorPersist_RequestsSkippedWithoutLatency(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	src := NewURLSelector()
	const cid int64 = 11
	const url = "https://orphan.example.com"

	// 手动只写 requests，不写 latencies/probeLatencies —— 模拟 latency 已被 GC 但 requests 残留
	sh := src.getShard(cid)
	sh.mu.Lock()
	sh.requests[urlKey{channelID: cid, url: url}] = &urlRequestCount{
		success: 100,
		failure: 5,
	}
	sh.mu.Unlock()

	if err := src.flush(ctx, store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	dst := NewURLSelector()
	if _, err := dst.LoadFromStore(ctx, store); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	shDst := dst.getShard(cid)
	shDst.mu.RLock()
	defer shDst.mu.RUnlock()

	if rc := shDst.requests[urlKey{channelID: cid, url: url}]; rc != nil {
		t.Errorf("orphan requests should not be reloaded, got success=%d failure=%d", rc.success, rc.failure)
	}
}

// TestURLSelectorPersist_ExpiredEntriesSkipped 已过期的 cooldown / no_thinking 不会被恢复
func TestURLSelectorPersist_ExpiredEntriesSkipped(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	src := NewURLSelector()
	const cid int64 = 7
	const url = "https://expired.example.com"

	// 手动塞一个 1 小时前就过期的 cooldown 条目
	sh := src.getShard(cid)
	sh.mu.Lock()
	sh.cooldowns[urlKey{channelID: cid, url: url}] = urlCooldownState{
		until:            time.Now().Add(-1 * time.Hour),
		consecutiveFails: 3,
	}
	// 一个还没过期的也塞进来
	const liveURL = "https://live.example.com"
	sh.cooldowns[urlKey{channelID: cid, url: liveURL}] = urlCooldownState{
		until:            time.Now().Add(10 * time.Minute),
		consecutiveFails: 1,
	}
	sh.mu.Unlock()

	// 注意：snapshotEntries 会过滤过期项，所以过期的根本不会写到 DB
	if err := src.flush(ctx, store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	dst := NewURLSelector()
	if _, err := dst.LoadFromStore(ctx, store); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	shDst := dst.getShard(cid)
	shDst.mu.RLock()
	defer shDst.mu.RUnlock()

	if _, ok := shDst.cooldowns[urlKey{channelID: cid, url: url}]; ok {
		t.Error("expired cooldown should not be reloaded")
	}
	if _, ok := shDst.cooldowns[urlKey{channelID: cid, url: liveURL}]; !ok {
		t.Error("live cooldown should be reloaded")
	}
}

// TestChannelAffinityPersist_RoundTrip ChannelAffinity 持久化往返
func TestChannelAffinityPersist_RoundTrip(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	src := NewChannelAffinity()
	src.Set("model-a", 100)
	src.Set("model-b", 200)
	src.Set("model-c", 300)

	if err := src.flush(ctx, store, 30*time.Minute); err != nil {
		t.Fatalf("flush: %v", err)
	}

	dst := NewChannelAffinity()
	loaded, err := dst.LoadFromStore(ctx, store, 30*time.Minute)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if loaded != 3 {
		t.Fatalf("expected 3 entries loaded, got %d", loaded)
	}

	if cid, ok := dst.Get("model-a", 30*time.Minute); !ok || cid != 100 {
		t.Errorf("model-a: got (%d, %v), want (100, true)", cid, ok)
	}
	if cid, ok := dst.Get("model-b", 30*time.Minute); !ok || cid != 200 {
		t.Errorf("model-b: got (%d, %v), want (200, true)", cid, ok)
	}
	if cid, ok := dst.Get("model-c", 30*time.Minute); !ok || cid != 300 {
		t.Errorf("model-c: got (%d, %v), want (300, true)", cid, ok)
	}
}

// TestChannelAffinityPersist_TTLFilteredOnLoad 加载时按当前 TTL 过滤过期条目
func TestChannelAffinityPersist_TTLFilteredOnLoad(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	src := NewChannelAffinity()
	src.Set("model-fresh", 1) // 当前时间设置，肯定不过期

	// 手动塞一个 2 小时前的旧条目
	src.mu.Lock()
	src.affinities["model-stale"] = &channelAffinityEntry{
		channelID: 99,
		updatedAt: time.Now().Add(-2 * time.Hour),
	}
	src.mu.Unlock()

	// 用 30min TTL 写入 → "model-stale" 被过滤掉，不会进 DB
	if err := src.flush(ctx, store, 30*time.Minute); err != nil {
		t.Fatalf("flush: %v", err)
	}

	dst := NewChannelAffinity()
	if _, err := dst.LoadFromStore(ctx, store, 30*time.Minute); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	if _, ok := dst.Get("model-fresh", 30*time.Minute); !ok {
		t.Error("fresh affinity should be loaded")
	}
	if _, ok := dst.Get("model-stale", 30*time.Minute); ok {
		t.Error("stale affinity should NOT be loaded (filtered at flush time)")
	}
}
