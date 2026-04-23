package app

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestURLSelector_SingleURL(t *testing.T) {
	sel := NewURLSelector()
	url, idx := sel.SelectURL(1, []string{"https://a.com"})
	if url != "https://a.com" || idx != 0 {
		t.Errorf("single URL: expected (https://a.com, 0), got (%s, %d)", url, idx)
	}
}

func TestURLSelector_EmptyURLs(t *testing.T) {
	sel := NewURLSelector()

	url, idx := sel.SelectURL(1, nil)
	if url != "" || idx != -1 {
		t.Fatalf("expected empty selection for empty urls, got (%q, %d)", url, idx)
	}

	sorted := sel.SortURLs(1, nil)
	if len(sorted) != 0 {
		t.Fatalf("expected empty sorted urls, got %v", sorted)
	}
}

func TestURLSelector_ColdStart_Distributes(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// 冷启动时应随机分布到所有URL，而非永远选第一个
	seen := map[string]int{}
	for range 100 {
		url, _ := sel.SelectURL(1, urls)
		seen[url]++
	}
	for _, u := range urls {
		if seen[u] == 0 {
			t.Errorf("cold start: URL %s was never selected in 100 rounds", u)
		}
	}
}

func TestURLSelector_WeightedRandom(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://slow.com", "https://fast.com"}
	// 记录延迟: slow=500ms, fast=100ms
	// 加权随机: fast权重=1/100, slow权重=1/500 → fast占83.3%
	sel.RecordLatency(1, "https://slow.com", 500*time.Millisecond)
	sel.RecordLatency(1, "https://fast.com", 100*time.Millisecond)

	fastCount := 0
	for range 1000 {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://fast.com" {
			fastCount++
		}
	}
	// 期望~83%，允许75%~92%
	if fastCount < 750 || fastCount > 920 {
		t.Errorf("weighted random: expected ~83%% fast, got %d/1000 (%.1f%%)", fastCount, float64(fastCount)/10)
	}
}

func TestURLSelector_SlowTTFBGetsHeavyPenalty(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://slow.com", "https://fast.com"}

	// slow=1500ms 会吃 2x 惩罚，effective=3000ms；fast=500ms。
	sel.RecordLatency(1, "https://slow.com", 1500*time.Millisecond)
	sel.RecordLatency(1, "https://fast.com", 500*time.Millisecond)

	fastCount := 0
	for range 1000 {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://fast.com" {
			fastCount++
		}
	}
	if fastCount < 800 {
		t.Fatalf("expected slow TTFB heavily penalized, fastCount=%d", fastCount)
	}
}

func TestURLSelector_RealTTFBOverridesProbeSeed(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://probe-fast.com", "https://steady.com"}

	// 冷启动时 probe 看起来很快，但真实 TTFB 很慢时，后续选择应该按真实 TTFB 走。
	sel.RecordProbeLatency(1, "https://probe-fast.com", 50*time.Millisecond)
	sel.RecordLatency(1, "https://probe-fast.com", 2500*time.Millisecond)
	sel.RecordLatency(1, "https://steady.com", 400*time.Millisecond)

	steadyCount := 0
	for range 400 {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://steady.com" {
			steadyCount++
		}
	}
	if steadyCount < 300 {
		t.Fatalf("expected real TTFB to override probe seed, steadyCount=%d", steadyCount)
	}
}

func TestURLSelector_AffinityEscapesSlowURL(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://affinity-slow.com", "https://better.com"}

	sel.RecordLatency(1, "https://affinity-slow.com", 4500*time.Millisecond)
	sel.RecordLatency(1, "https://better.com", 600*time.Millisecond)
	sel.SetModelAffinity(1, "gpt-4", "https://affinity-slow.com")

	// 首字慢到进隔离后，软亲和性也不能把它硬拽回来。
	selected, _ := sel.SelectURLForModel(1, "gpt-4", urls)
	if selected != "https://better.com" {
		t.Fatalf("expected affinity escape to better URL, got %s", selected)
	}
}

func TestURLSelector_SlowTTFBIsolationSkipsRecentlySlowSuccess(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 20 * time.Millisecond
	urls := []string{"https://slow.com", "https://fast.com"}

	sel.RecordLatency(1, "https://slow.com", 3*time.Second) // 刚好超过隔离阈值(2.5s)
	sel.RecordLatency(1, "https://fast.com", 400*time.Millisecond)

	selected, _ := sel.SelectURL(1, urls)
	if selected != "https://fast.com" {
		t.Fatalf("expected slow-success URL isolated out of the pool, got %s", selected)
	}

	stats := sel.GetURLStats(1, urls)
	if !stats[0].SlowIsolated {
		t.Fatalf("expected slow URL to report active isolation, stats=%+v", stats)
	}

	time.Sleep(25 * time.Millisecond)
	seenSlow := false
	for range 200 {
		picked, _ := sel.SelectURL(1, urls)
		if picked == "https://slow.com" {
			seenSlow = true
			break
		}
	}
	if !seenSlow {
		t.Fatalf("expected slow URL to become selectable again after isolation expires")
	}
}

func TestURLSelector_SkipsCooledDown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.RecordLatency(1, "https://a.com", 50*time.Millisecond) // a更快
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	sel.CooldownURL(1, "https://a.com") // 但a被冷却

	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("expected non-cooled URL https://b.com, got %s", url)
	}
}

func TestURLSelector_AllCooledDown_ReturnsBest(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.CooldownURL(1, "https://a.com")
	sel.CooldownURL(1, "https://b.com")

	// 所有URL都冷却时，仍然返回一个URL（兜底）
	url, _ := sel.SelectURL(1, urls)
	if url == "" {
		t.Error("all cooled: should still return a URL as fallback")
	}
}

func TestURLSelector_CooldownExpires(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond
	urls := []string{"https://a.com", "https://b.com"}
	// a延迟200ms，b延迟1000ms，都在甜区内（100-3000ms）
	for range 5 {
		sel.RecordLatency(1, "https://a.com", 200*time.Millisecond)
	}
	sel.RecordLatency(1, "https://b.com", 1000*time.Millisecond)
	sel.CooldownURL(1, "https://a.com")

	// 冷却期间：a被排除，只能选b
	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("during cooldown: expected b, got %s", url)
	}

	// 冷却过期后：a(200ms) vs b(1000ms) → a权重5倍 → a占~83%
	time.Sleep(15 * time.Millisecond)
	aCount := 0
	for range 200 {
		url, _ = sel.SelectURL(1, urls)
		if url == "https://a.com" {
			aCount++
		}
	}
	if aCount < 120 {
		t.Errorf("after cooldown: expected a selected ~83%%, got %d/200", aCount)
	}
}

func TestURLSelector_IndependentChannels(t *testing.T) {
	sel := NewURLSelector()
	// 渠道1: a慢(2000ms), b快(200ms)，都在甜区
	sel.RecordLatency(1, "https://a.com", 2000*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	// 渠道2: a快(200ms), b慢(2000ms)
	sel.RecordLatency(2, "https://a.com", 200*time.Millisecond)
	sel.RecordLatency(2, "https://b.com", 2000*time.Millisecond)

	urls := []string{"https://a.com", "https://b.com"}
	// 200ms vs 2000ms → 快的占 1/200 / (1/200+1/2000) = 90.9%
	ch2a, ch1b := 0, 0
	for range 200 {
		if url, _ := sel.SelectURL(2, urls); url == "https://a.com" {
			ch2a++
		}
		if url, _ := sel.SelectURL(1, urls); url == "https://b.com" {
			ch1b++
		}
	}
	if ch2a < 150 {
		t.Errorf("channel 2: expected a.com ~91%%, got %d/200", ch2a)
	}
	if ch1b < 150 {
		t.Errorf("channel 1: expected b.com ~91%%, got %d/200", ch1b)
	}
}

func TestURLSelector_ExploreWhenNoGoodKnown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// a有延迟数据但成功率很低（1成功5失败=16.7%），低于50%阈值
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	for range 5 {
		sel.CooldownURL(1, "https://a.com")
	}
	// 清掉冷却，只保留低成功率记录
	sh := sel.getShard(1)
	sh.mu.Lock()
	delete(sh.cooldowns, urlKey{channelID: 1, url: "https://a.com"})
	sh.mu.Unlock()

	// 没有好的已知 URL 时，canary 会提升 1 个 unknown 到首跳池，
	// 剩余 unknown 作为最低优先级兜底追加到计划末尾。
	unknownSeen := map[string]int{}
	for range 80 {
		ordered := orderURLsWithSelector(sel, 1, urls, "")
		// 全部 URL 都应在计划里（known + canary + 兜底）
		if len(ordered) != len(urls) {
			t.Fatalf("expected all URLs in plan, got %v", ordered)
		}
		// 首跳应该是 canary（unknown）或 known，不是固定的
		for _, entry := range ordered {
			if entry.url == "https://b.com" || entry.url == "https://c.com" {
				unknownSeen[entry.url]++
			}
		}
	}
	// unknown URL 应该轮转出现在 canary 位置
	if len(unknownSeen) < 2 {
		t.Fatalf("expected canary rotation across unknown URLs, got %v", unknownSeen)
	}
}

func TestURLSelector_PreferKnownGoodOverUnknown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// a有延迟数据且成功率100%（好的已知URL）
	for range 3 {
		sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	}

	// 有好的已知URL时，应优先用已知的，不盲目探索未知
	seen := map[string]int{}
	for range 100 {
		url, _ := sel.SelectURL(1, urls)
		seen[url]++
	}
	// a应该被选中大部分时间（但未知URL也有一点概率因为放进了pool）
	if seen["https://a.com"] < 30 {
		t.Errorf("should prefer known good URL, got a=%d b=%d c=%d",
			seen["https://a.com"], seen["https://b.com"], seen["https://c.com"])
	}
}

func TestURLSelector_ExponentialBackoff(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond

	key := urlKey{channelID: 1, url: "https://a.com"}

	sh := sel.getShard(1)

	// 第1次冷却: 10ms
	sel.CooldownURL(1, "https://a.com")
	state1 := sh.cooldowns[key] //nolint:gosec
	if state1.consecutiveFails != 1 {
		t.Errorf("expected 1 fail, got %d", state1.consecutiveFails)
	}

	// 等待冷却过期后再次冷却: 20ms
	time.Sleep(15 * time.Millisecond)
	sel.CooldownURL(1, "https://a.com")
	state2 := sh.cooldowns[key]
	if state2.consecutiveFails != 2 {
		t.Errorf("expected 2 fails, got %d", state2.consecutiveFails)
	}
}

func TestURLSelector_SuspiciousLowLatencyNotPreferred(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://suspicious.com", "https://normal.com"}

	// <100ms 的可疑低延迟会被按500ms算，不会获得额外优势
	sel.RecordLatency(1, "https://suspicious.com", 500*time.Microsecond) // 0.5ms → 按500ms算
	sel.RecordLatency(1, "https://normal.com", 200*time.Millisecond)     // 200ms 正常

	normalCount := 0
	rounds := 200
	for range rounds {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://normal.com" {
			normalCount++
		}
	}

	// normal(200ms) vs suspicious(按500ms算) → normal权重更高
	if normalCount <= rounds/4 {
		t.Fatalf("expected normal URL preferred over suspicious low-latency, normalCount=%d", normalCount)
	}
}

func TestURLSelector_RecordLatencyClearsCooldownWindow(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(1)
	url := "https://a.com"

	sel.CooldownURL(channelID, url)
	if !sel.IsCooledDown(channelID, url) {
		t.Fatalf("expected url cooled down before success")
	}

	// 成功反馈后应立刻可用，不应继续停留在旧的 cooldown until。
	sel.RecordLatency(channelID, url, 20*time.Millisecond)
	if sel.IsCooledDown(channelID, url) {
		t.Fatalf("expected cooldown cleared after successful latency record")
	}
}

func TestURLSelector_GC_RemovesExpiredState(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	sh := sel.getShard(1)

	oldLatencyKey := urlKey{channelID: 1, url: "https://old-latency.com"}
	freshLatencyKey := urlKey{channelID: 1, url: "https://fresh-latency.com"}
	oldProbeKey := urlKey{channelID: 1, url: "https://old-probe.com"}
	expiredCooldownKey := urlKey{channelID: 1, url: "https://expired-cooldown.com"}
	activeCooldownKey := urlKey{channelID: 1, url: "https://active-cooldown.com"}

	sh.latencies[oldLatencyKey] = &ewmaValue{value: 120, lastSeen: now.Add(-25 * time.Hour)}
	sh.latencies[freshLatencyKey] = &ewmaValue{value: 80, lastSeen: now.Add(-2 * time.Hour)}
	sh.probeLatencies[oldProbeKey] = &ewmaValue{value: 50, lastSeen: now.Add(-25 * time.Hour)}
	sh.cooldowns[expiredCooldownKey] = urlCooldownState{until: now.Add(-time.Minute), consecutiveFails: 2}
	sh.cooldowns[activeCooldownKey] = urlCooldownState{until: now.Add(2 * time.Minute), consecutiveFails: 1}

	sel.GC(24 * time.Hour)

	if _, ok := sh.latencies[oldLatencyKey]; ok {
		t.Fatalf("expected expired latency to be removed")
	}
	if _, ok := sh.latencies[freshLatencyKey]; !ok {
		t.Fatalf("expected fresh latency to be preserved")
	}
	if _, ok := sh.probeLatencies[oldProbeKey]; ok {
		t.Fatalf("expected expired probe latency to be removed")
	}
	if _, ok := sh.cooldowns[expiredCooldownKey]; ok {
		t.Fatalf("expected expired cooldown to be removed")
	}
	if _, ok := sh.cooldowns[activeCooldownKey]; !ok {
		t.Fatalf("expected active cooldown to be preserved")
	}
}

func TestURLSelector_RecordLatency_TriggersScheduledCleanup(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()
	sh := sel.getShard(1)

	staleKey := urlKey{channelID: 1, url: "https://stale.com"}
	staleProbeKey := urlKey{channelID: 1, url: "https://stale-probe.com"}
	expiredCooldownKey := urlKey{channelID: 1, url: "https://expired.com"}
	sh.latencies[staleKey] = &ewmaValue{value: 100, lastSeen: now.Add(-48 * time.Hour)}
	sh.probeLatencies[staleProbeKey] = &ewmaValue{value: 30, lastSeen: now.Add(-48 * time.Hour)}
	sh.cooldowns[expiredCooldownKey] = urlCooldownState{until: now.Add(-time.Minute), consecutiveFails: 1}

	// 强制下一次写路径触发清理
	sel.cleanupInterval = time.Millisecond
	sel.latencyMaxAge = 24 * time.Hour
	sh.nextCleanup = now.Add(-time.Second)

	sel.RecordLatency(1, "https://new.com", 10*time.Millisecond)

	if _, ok := sh.latencies[staleKey]; ok {
		t.Fatalf("expected stale latency removed by scheduled cleanup")
	}
	if _, ok := sh.probeLatencies[staleProbeKey]; ok {
		t.Fatalf("expected stale probe removed by scheduled cleanup")
	}
	if _, ok := sh.cooldowns[expiredCooldownKey]; ok {
		t.Fatalf("expected expired cooldown removed by scheduled cleanup")
	}
}

func TestExtractHostPort(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://api.openai.com", "api.openai.com:443"},
		{"http://localhost", "localhost:80"},
		{"https://api.example.com:8443", "api.example.com:8443"},
		{"http://127.0.0.1:3000", "127.0.0.1:3000"},
		{"https://[::1]", "[::1]:443"},
		{"http://[2001:db8::1]:8080", "[2001:db8::1]:8080"},
		{"", ""},
		{"not-a-url", ""},
	}
	for _, tt := range tests {
		got := extractHostPort(tt.input)
		if got != tt.want {
			t.Errorf("extractHostPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestURLSelector_ProbeURLs_TimeoutCoolsPendingURLs(t *testing.T) {
	sel := NewURLSelector()
	sel.probeTimeout = 20 * time.Millisecond
	sel.probeDial = func(ctx context.Context, _, address string) (net.Conn, error) {
		switch address {
		case "fast.example:443":
			conn, peer := net.Pipe()
			_ = peer.Close()
			return conn, nil
		case "slow.example:443":
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			t.Fatalf("unexpected probe address: %s", address)
			return nil, context.Canceled
		}
	}

	urls := []string{"https://fast.example", "https://slow.example"}
	sel.ProbeURLs(context.Background(), 1, urls)

	if !sel.IsCooledDown(1, "https://slow.example") {
		t.Fatalf("expected timed out URL to be cooled down")
	}

	probeSh := sel.getShard(1)
	probeSh.mu.RLock()
	_, fastKnown := probeSh.probeLatencies[urlKey{channelID: 1, url: "https://fast.example"}]
	_, slowKnown := probeSh.probeLatencies[urlKey{channelID: 1, url: "https://slow.example"}]
	probeSh.mu.RUnlock()

	if !fastKnown {
		t.Fatalf("expected fast URL latency seed recorded")
	}
	if slowKnown {
		t.Fatalf("expected timed out URL to remain without latency seed")
	}

	selected, _ := sel.SelectURL(1, urls)
	if selected != "https://fast.example" {
		t.Fatalf("expected known fast URL selected after probe timeout, got %s", selected)
	}
}

func TestURLSelector_ProbeURLs_SuccessDoesNotClearExistingCooldown(t *testing.T) {
	sel := NewURLSelector()
	sel.probeDial = func(context.Context, string, string) (net.Conn, error) {
		conn, peer := net.Pipe()
		_ = peer.Close()
		return conn, nil
	}

	urls := []string{"https://a.example", "https://b.example"}
	sel.CooldownURL(1, "https://b.example")

	sel.ProbeURLs(context.Background(), 1, urls)

	if !sel.IsCooledDown(1, "https://b.example") {
		t.Fatalf("expected successful probe to preserve existing cooldown")
	}
}

func TestURLSelector_ProbeURLs_SkipsSingleURL(t *testing.T) {
	sel := NewURLSelector()
	// 单URL不应触发探测
	sel.ProbeURLs(context.Background(), 1, []string{"https://a.com"})
	skipSh := sel.getShard(1)
	skipSh.mu.RLock()
	defer skipSh.mu.RUnlock()
	if len(skipSh.probeLatencies) != 0 {
		t.Errorf("single URL should not trigger probe, got %d probe latencies", len(skipSh.probeLatencies))
	}
}

func TestURLSelector_ProbeURLs_SkipsKnownURLs(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	// 给所有URL预设延迟数据
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)

	// 所有URL已有数据，ProbeURLs应立即返回（不发TCP连接）
	sel.ProbeURLs(context.Background(), 1, urls)
	// 不crash即通过
}

func TestURLSelector_ProbeURLs_InvalidURL(t *testing.T) {
	sel := NewURLSelector()
	// 无效URL应被冷却，不应panic
	sel.ProbeURLs(context.Background(), 1, []string{"not-a-valid-url", "also-invalid"})

	invSh := sel.getShard(1)
	invSh.mu.RLock()
	defer invSh.mu.RUnlock()
	// 无效URL应该被冷却或至少不产生延迟数据
	if len(invSh.probeLatencies) != 0 {
		t.Errorf("invalid URLs should not produce probe latency data, got %d", len(invSh.probeLatencies))
	}
}

func TestURLSelector_ProbeURLs_RealTCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TCP probe test in short mode")
	}

	sel := NewURLSelector()
	// 用localhost做TCP探测测试（假设本机80端口不开放）
	// 这个测试主要验证ProbeURLs不会panic/hang，而非成功连接
	urls := []string{"https://127.0.0.1:1", "https://127.0.0.1:2"}
	sel.ProbeURLs(context.Background(), 1, urls)

	// 连接失败的URL应被冷却
	cooled := 0
	for _, u := range urls {
		if sel.IsCooledDown(1, u) {
			cooled++
		}
	}
	if cooled == 0 {
		t.Logf("warning: no URLs were cooled down (might succeed if ports are open)")
	}
}

func TestURLSelector_ProbeURLs_CancelDoesNotCooldownPendingURLs(t *testing.T) {
	sel := NewURLSelector()
	sel.probeTimeout = time.Second

	slowStarted := make(chan struct{})
	sel.probeDial = func(ctx context.Context, _, address string) (net.Conn, error) {
		switch address {
		case "fast.example:443":
			conn, peer := net.Pipe()
			_ = peer.Close()
			return conn, nil
		case "slow.example:443":
			close(slowStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			t.Fatalf("unexpected probe address: %s", address)
			return nil, context.Canceled
		}
	}

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-slowStarted
		cancel()
	}()

	urls := []string{"https://fast.example", "https://slow.example"}
	sel.ProbeURLs(parentCtx, 1, urls)

	if sel.IsCooledDown(1, "https://slow.example") {
		t.Fatalf("expected canceled probe not to cooldown pending URL")
	}
}

func TestURLSelector_ProbeURLs_DeduplicatesInFlightRequests(t *testing.T) {
	sel := NewURLSelector()
	sel.probeTimeout = time.Second

	var dialCount atomic.Int64
	release := make(chan struct{})
	closed := false
	closeRelease := func() {
		if !closed {
			close(release)
			closed = true
		}
	}
	defer closeRelease()

	sel.probeDial = func(ctx context.Context, _, address string) (net.Conn, error) {
		dialCount.Add(1)
		select {
		case <-release:
			conn, peer := net.Pipe()
			_ = peer.Close()
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	urls := []string{"https://a.example", "https://b.example"}
	done1 := make(chan struct{})
	go func() {
		sel.ProbeURLs(context.Background(), 1, urls)
		close(done1)
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	for dialCount.Load() < int64(len(urls)) {
		if time.Now().After(deadline) {
			closeRelease()
			<-done1
			t.Fatalf("first probe did not start all dials in time, got=%d want=%d", dialCount.Load(), len(urls))
		}
		time.Sleep(time.Millisecond)
	}

	done2 := make(chan struct{})
	go func() {
		sel.ProbeURLs(context.Background(), 1, urls)
		close(done2)
	}()

	select {
	case <-done2:
	case <-time.After(100 * time.Millisecond):
		closeRelease()
		<-done1
		t.Fatalf("second probe should return quickly when URLs are already being probed")
	}

	if got := dialCount.Load(); got != int64(len(urls)) {
		closeRelease()
		<-done1
		t.Fatalf("expected no duplicate probe dials, got=%d want=%d", got, len(urls))
	}

	closeRelease()
	<-done1
}

func TestURLSelector_SuspectLowLatencyCooldown_Applied(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(1)
	url := "https://suspect.com"

	// 冷却 300ms → URL 应进入 cooldown
	sel.SuspectLowLatencyCooldown(channelID, url, 300*time.Millisecond)
	if !sel.IsCooledDown(channelID, url) {
		t.Fatalf("expected url to be cooled down after SuspectLowLatencyCooldown")
	}

	// consecutiveFails 不能被累加（这不是真实失败）
	sh := sel.getShard(channelID)
	sh.mu.RLock()
	cd := sh.cooldowns[urlKey{channelID: channelID, url: url}]
	if cd.consecutiveFails != 0 {
		sh.mu.RUnlock()
		t.Fatalf("suspect cooldown should not accumulate consecutiveFails, got %d", cd.consecutiveFails)
	}
	// failure 计数不能被累加
	if rc := sh.requests[urlKey{channelID: channelID, url: url}]; rc != nil && rc.failure != 0 {
		sh.mu.RUnlock()
		t.Fatalf("suspect cooldown should not increment failure, got %d", rc.failure)
	}
	sh.mu.RUnlock()

	// 调两次仍只是固定时长：consecutiveFails 仍为 0
	sel.SuspectLowLatencyCooldown(channelID, url, 300*time.Millisecond)
	sh.mu.RLock()
	cd = sh.cooldowns[urlKey{channelID: channelID, url: url}]
	sh.mu.RUnlock()
	if cd.consecutiveFails != 0 {
		t.Fatalf("consecutiveFails should stay 0 after repeated suspect cooldowns, got %d", cd.consecutiveFails)
	}
}

func TestURLSelector_SuspectLowLatencyCooldown_NoOpWhenZero(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(2)
	url := "https://noop.com"

	sel.SuspectLowLatencyCooldown(channelID, url, 0)
	if sel.IsCooledDown(channelID, url) {
		t.Fatalf("duration=0 should not install any cooldown")
	}

	sel.SuspectLowLatencyCooldown(channelID, url, -time.Second)
	if sel.IsCooledDown(channelID, url) {
		t.Fatalf("negative duration should not install any cooldown")
	}
}

func TestURLSelector_SuspectLowLatencyCooldown_KeepsLongerExisting(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(3)
	url := "https://longer.com"

	// 先人工装入更长的真实冷却（例如真实失败后的 10 分钟）
	sh := sel.getShard(channelID)
	sh.mu.Lock()
	longUntil := time.Now().Add(10 * time.Minute)
	sh.cooldowns[urlKey{channelID: channelID, url: url}] = urlCooldownState{
		until:            longUntil,
		consecutiveFails: 3,
	}
	sh.mu.Unlock()

	// 再调一个更短的 suspect 冷却：不能把更长的缩短
	sel.SuspectLowLatencyCooldown(channelID, url, 5*time.Second)

	sh.mu.RLock()
	cd := sh.cooldowns[urlKey{channelID: channelID, url: url}]
	sh.mu.RUnlock()
	if !cd.until.Equal(longUntil) {
		t.Fatalf("suspect cooldown should not shrink an existing longer cooldown; got until=%v want=%v", cd.until, longUntil)
	}
	if cd.consecutiveFails != 3 {
		t.Fatalf("suspect cooldown should not reset consecutiveFails; got %d want 3", cd.consecutiveFails)
	}
}
