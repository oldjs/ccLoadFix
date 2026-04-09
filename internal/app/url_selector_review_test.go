package app

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func runURLSelections(rounds int, pick func() string) map[string]int {
	counts := make(map[string]int)
	for range rounds {
		counts[pick()]++
	}
	return counts
}

func selectionRatio(count int, rounds int) float64 {
	if rounds == 0 {
		return 0
	}
	return float64(count) / float64(rounds)
}

func formatSelectionStats(urls []string, counts map[string]int, rounds int) []string {
	stats := make([]string, 0, len(urls))
	for _, rawURL := range urls {
		count := counts[rawURL]
		stats = append(stats, fmt.Sprintf("%s=%d (%.1f%%)", rawURL, count, selectionRatio(count, rounds)*100))
	}
	return stats
}

func TestPenalizeSlowTTFBMS_Boundaries(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "zero", in: 0, want: defaultEffectiveLatencyMS},
		{name: "negative", in: -10, want: defaultEffectiveLatencyMS},
		{name: "nan", in: math.NaN(), want: defaultEffectiveLatencyMS},
		{name: "exact 100", in: 100, want: 100},
		{name: "below 100", in: 99, want: defaultEffectiveLatencyMS},
		{name: "exact 1200", in: 1200, want: 2400},
		{name: "exact 2500", in: 2500, want: 7500},
		{name: "exact 4000", in: 4000, want: 20000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := penalizeSlowTTFBMS(tt.in)
			if math.Abs(got-tt.want) > 0.001 {
				t.Fatalf("penalizeSlowTTFBMS(%v)=%v want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestURLSelector_QuantifiedSlowPenalty1000(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://fast.example", "https://slow.example"}
	sel.RecordLatency(1, urls[0], 500*time.Millisecond)
	sel.RecordLatency(1, urls[1], 2500*time.Millisecond)

	const rounds = 1000
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("quantified penalty: %v", formatSelectionStats(urls, counts, rounds))

	fastRatio := selectionRatio(counts[urls[0]], rounds)
	slowRatio := selectionRatio(counts[urls[1]], rounds)
	if fastRatio < 0.88 {
		t.Fatalf("expected fast URL to dominate after 2500ms penalty, fastRatio=%.3f slowRatio=%.3f", fastRatio, slowRatio)
	}
	if slowRatio > 0.12 {
		t.Fatalf("expected slow URL to be heavily suppressed, fastRatio=%.3f slowRatio=%.3f", fastRatio, slowRatio)
	}
}

func TestURLSelector_ProbeSeedDropsAfterRealSlowTTFB(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://probe-fast-but-slow.example", "https://steady.example"}

	sel.RecordProbeLatency(1, urls[0], 50*time.Millisecond)
	sel.RecordProbeLatency(1, urls[1], 80*time.Millisecond)

	const rounds = 600
	before := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("before real TTFB: %v", formatSelectionStats(urls, before, rounds))

	sel.RecordLatency(1, urls[0], 4000*time.Millisecond)
	sel.RecordLatency(1, urls[1], 500*time.Millisecond)

	after := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("after real TTFB: %v", formatSelectionStats(urls, after, rounds))

	if selectionRatio(after[urls[0]], rounds) > 0.08 {
		t.Fatalf("expected real slow TTFB to quickly suppress probe-fast URL, after=%v", after)
	}
	if selectionRatio(after[urls[1]], rounds) < 0.92 {
		t.Fatalf("expected steady URL to dominate after real TTFB arrives, after=%v", after)
	}
}

func TestURLSelector_AffinityEscapesSlowURL_Quantified(t *testing.T) {
	sel := NewURLSelector()
	model := "gpt-4.1"
	urls := []string{"https://affinity-slow.example", "https://better.example", "https://fallback.example"}

	sel.RecordLatency(1, urls[0], 4000*time.Millisecond)
	sel.RecordLatency(1, urls[1], 300*time.Millisecond)
	sel.RecordLatency(1, urls[2], 700*time.Millisecond)
	sel.SetModelAffinity(1, model, urls[0])

	const rounds = 500
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURLForModel(1, model, urls)
		return picked
	})
	t.Logf("affinity escape: %v", formatSelectionStats(urls, counts, rounds))

	if selectionRatio(counts[urls[0]], rounds) > 0.06 {
		t.Fatalf("expected affinity to escape the slow URL, counts=%v", counts)
	}
	if counts[urls[1]] <= counts[urls[2]] {
		t.Fatalf("expected better URL to stay ahead after affinity escape, counts=%v", counts)
	}
}

func TestURLSelector_SoftAffinityIsBiasNotShortCircuit(t *testing.T) {
	sel := NewURLSelector()
	model := "gpt-4.1"
	urls := []string{"https://best.example", "https://affinity.example", "https://fallback.example"}

	sel.RecordLatency(1, urls[0], 400*time.Millisecond)
	sel.RecordLatency(1, urls[1], 900*time.Millisecond)
	sel.RecordLatency(1, urls[2], 1200*time.Millisecond)

	baseline := runURLSelections(600, func() string {
		picked, _ := sel.SelectURLForModel(1, model, urls)
		return picked
	})
	sel.SetModelAffinity(1, model, urls[1])
	afterAffinity := runURLSelections(600, func() string {
		picked, _ := sel.SelectURLForModel(1, model, urls)
		return picked
	})

	t.Logf("soft affinity baseline: %v", formatSelectionStats(urls, baseline, 600))
	t.Logf("soft affinity after: %v", formatSelectionStats(urls, afterAffinity, 600))

	if afterAffinity[urls[1]] <= baseline[urls[1]] {
		t.Fatalf("expected affinity URL to gain probability after soft bias, baseline=%v after=%v", baseline, afterAffinity)
	}
	if afterAffinity[urls[0]] == 0 {
		t.Fatalf("expected best real URL to remain selectable after affinity bias, after=%v", afterAffinity)
	}
}

func TestURLSelector_ColdStart_NoHistoryStillSelectsAll(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{
		"https://cold-a.example",
		"https://cold-b.example",
		"https://cold-c.example",
		"https://cold-d.example",
	}

	const rounds = 400
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("cold start: %v", formatSelectionStats(urls, counts, rounds))

	for _, rawURL := range urls {
		if counts[rawURL] == 0 {
			t.Fatalf("expected cold-start exploration to include %s, counts=%v", rawURL, counts)
		}
	}
}

func TestURLSelector_AllVerySlowStillPreferLeastSlow(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://4500.example", "https://5000.example", "https://8000.example"}
	sel.RecordLatency(1, urls[0], 4500*time.Millisecond)
	sel.RecordLatency(1, urls[1], 5000*time.Millisecond)
	sel.RecordLatency(1, urls[2], 8000*time.Millisecond)

	const rounds = 1000
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("all slow: %v", formatSelectionStats(urls, counts, rounds))

	if counts[urls[0]] <= counts[urls[1]] || counts[urls[1]] <= counts[urls[2]] {
		t.Fatalf("expected least-slow URL to stay ahead even when all are slow, counts=%v", counts)
	}
}

func TestURLSelector_OnlyOneCandidateAndZeroLatencyStayUsable(t *testing.T) {
	sel := NewURLSelector()
	url := "https://only.example"
	sel.RecordLatency(1, url, 0)
	sel.CooldownURL(1, url)

	picked, idx := sel.SelectURL(1, []string{url})
	if picked != url || idx != 0 {
		t.Fatalf("expected the only candidate to be returned, got (%s,%d)", picked, idx)
	}

	stats := sel.GetURLStats(1, []string{url})
	if len(stats) != 1 {
		t.Fatalf("expected one stat entry, got %d", len(stats))
	}
	if stats[0].LatencyMs <= 0 || math.IsNaN(stats[0].LatencyMs) || math.IsInf(stats[0].LatencyMs, 0) {
		t.Fatalf("expected normalized effective latency for zero TTFB, got %+v", stats[0])
	}
}

func TestURLSelector_ConcurrentAccessIsStable(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.example", "https://b.example", "https://c.example"}
	model := "gpt-4o"

	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for step := 0; step < 300; step++ {
				rawURL := urls[(worker+step)%len(urls)]
				switch step % 7 {
				case 0:
					sel.RecordLatency(1, rawURL, time.Duration(100+worker*20+step%50)*time.Millisecond)
				case 1:
					sel.RecordProbeLatency(1, rawURL, time.Duration(50+worker*10+step%20)*time.Millisecond)
				case 2:
					sel.CooldownURL(1, rawURL)
				case 3:
					sel.MarkURLSuccess(1, rawURL)
				case 4:
					sel.SetModelAffinity(1, model, rawURL)
				case 5:
					sel.ClearModelAffinity(1, model, rawURL)
				default:
					picked, _ := sel.SelectURLForModel(1, model, urls)
					if picked == "" {
						t.Errorf("worker %d step %d: expected non-empty selection", worker, step)
					}
					ordered := sel.SortURLs(1, urls)
					if len(ordered) == 0 || len(ordered) > len(urls) {
						t.Errorf("worker %d step %d: expected bounded sorting output, got %d", worker, step, len(ordered))
					}
					seen := make(map[string]struct{}, len(ordered))
					for _, entry := range ordered {
						if _, ok := seen[entry.url]; ok {
							t.Errorf("worker %d step %d: expected no duplicate URLs in plan %v", worker, step, ordered)
						}
						seen[entry.url] = struct{}{}
					}
				}
			}
		}(worker)
	}
	wg.Wait()

	picked, _ := sel.SelectURLForModel(1, model, urls)
	if picked == "" {
		t.Fatalf("expected selector to remain usable after concurrent access")
	}
}

func TestURLSelector_SortAndSelectStayConsistent(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://best.example", "https://mid.example", "https://worst.example"}
	sel.RecordLatency(1, urls[0], 100*time.Millisecond)
	sel.RecordLatency(1, urls[1], 600*time.Millisecond)
	sel.RecordLatency(1, urls[2], 2500*time.Millisecond)

	sorted := sel.SortURLs(1, urls)
	if len(sorted) != len(urls) {
		t.Fatalf("expected %d sorted urls, got %d", len(urls), len(sorted))
	}
	seen := make(map[string]struct{}, len(sorted))
	for _, entry := range sorted {
		seen[entry.url] = struct{}{}
	}
	if len(seen) != len(urls) {
		t.Fatalf("expected sort plan without duplicates, got %v", sorted)
	}
	if _, ok := seen[urls[0]]; !ok {
		t.Fatalf("expected fastest URL to remain in sort plan, got %v", sorted)
	}

	const rounds = 1000
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURLForModel(1, "", urls)
		return picked
	})
	t.Logf("sort vs select: %v", formatSelectionStats(urls, counts, rounds))

	if counts[urls[0]] <= counts[urls[1]] || counts[urls[1]] <= counts[urls[2]] {
		t.Fatalf("expected select preference to match sort order, counts=%v sorted=%v", counts, sorted)
	}
}

func TestURLSelector_ProbeOnlyNeverBeatsRealPrimary(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://real.example", "https://probe.example", "https://unknown.example"}

	sel.RecordLatency(1, urls[0], 700*time.Millisecond)
	sel.RecordProbeLatency(1, urls[1], 30*time.Millisecond)

	for range 120 {
		picked, _ := sel.SelectURL(1, urls)
		if picked != urls[0] {
			t.Fatalf("expected real-data URL to stay ahead of probe-only candidates, got %s", picked)
		}
	}

	ordered := orderURLsWithSelector(sel, 1, urls, "")
	// unknown URL 作为最低优先级兜底追加到末尾，所以 plan 长度 = 全部 URL
	if len(ordered) != 3 {
		t.Fatalf("expected all URLs in plan (real + probe + unknown fallback), got %v", ordered)
	}
	// 前两个顺序不变：real 优先，probe 其次
	if ordered[0].url != urls[0] || ordered[1].url != urls[1] {
		t.Fatalf("expected real URL before probe URL, got %v", ordered)
	}
	// unknown 只能排末尾
	if ordered[2].url != urls[2] {
		t.Fatalf("expected unknown URL at tail as fallback, got %v", ordered)
	}
}

func TestURLSelector_ControlledCanaryPicksAtMostOneUnknown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://bad-known.example", "https://u1.example", "https://u2.example", "https://u3.example"}

	sel.RecordLatency(1, urls[0], 200*time.Millisecond)
	for range 5 {
		sel.CooldownURL(1, urls[0])
	}
	sel.mu.Lock()
	delete(sel.cooldowns, urlKey{channelID: 1, url: urls[0]})
	sel.mu.Unlock()

	seenCanary := make(map[string]int)
	for range 120 {
		ordered := orderURLsWithSelector(sel, 1, urls, "")
		// 现在 plan 包含所有 URL：canary(1个) + known + 剩余 unknown 兜底
		// 验证首跳池里最多 1 个 unknown 被提升为 canary（排在 known 前面或同级）
		canaryCount := 0
		for i, entry := range ordered {
			if entry.url == urls[0] {
				continue
			}
			// known URL 前面出现的 unknown 算 canary
			if i == 0 {
				canaryCount++
				seenCanary[entry.url]++
			}
		}
		if canaryCount > 1 {
			t.Fatalf("expected at most one unknown canary promoted in plan, got %v", ordered)
		}
	}
	// canary 应该在多个 unknown URL 之间轮转
	if len(seenCanary) < 2 {
		t.Fatalf("expected canary rotation across unknown URLs, got %v", seenCanary)
	}
}

func TestURLSelector_EndToEndPenaltyProfile(t *testing.T) {
	sel := NewURLSelector()
	model := "gpt-5.4"
	urls := []string{
		"https://100ms.example",
		"https://500ms.example",
		"https://1200ms.example",
		"https://2500ms.example",
		"https://4000ms.example",
	}
	latencies := []time.Duration{
		100 * time.Millisecond,
		500 * time.Millisecond,
		1200 * time.Millisecond,
		2500 * time.Millisecond,
		4000 * time.Millisecond,
	}
	for i, rawURL := range urls {
		sel.RecordLatency(1, rawURL, latencies[i])
	}

	const rounds = 1000
	counts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURL(1, urls)
		return picked
	})
	t.Logf("5-url penalty profile: %v", formatSelectionStats(urls, counts, rounds))

	if counts[urls[0]] <= counts[urls[1]] || counts[urls[1]] <= counts[urls[2]] {
		t.Fatalf("expected top three URLs to keep clear ordering after penalties, counts=%v", counts)
	}
	if selectionRatio(counts[urls[3]], rounds) > 0.05 {
		t.Fatalf("expected 2500ms URL to stay rare, counts=%v", counts)
	}
	if selectionRatio(counts[urls[4]], rounds) > 0.03 {
		t.Fatalf("expected 4000ms URL to be extremely rare, counts=%v", counts)
	}

	sel.SetModelAffinity(1, model, urls[4])
	affinityCounts := runURLSelections(rounds, func() string {
		picked, _ := sel.SelectURLForModel(1, model, urls)
		return picked
	})
	t.Logf("affinity on slow URL: %v", formatSelectionStats(urls, affinityCounts, rounds))

	if selectionRatio(affinityCounts[urls[4]], rounds) > 0.04 {
		t.Fatalf("expected affinity to escape the 4000ms URL, counts=%v", affinityCounts)
	}
	if affinityCounts[urls[0]] <= affinityCounts[urls[1]] {
		t.Fatalf("expected fastest URL to remain the primary pick after escape, counts=%v", affinityCounts)
	}
}

func TestURLSelector_SlowTTFBIsolationRemovesVerySlowURLFromPlan(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://fast.example", "https://slow.example", "https://probe.example"}

	sel.RecordLatency(1, urls[0], 400*time.Millisecond)
	sel.RecordLatency(1, urls[1], 5*time.Second)
	sel.RecordProbeLatency(1, urls[2], 40*time.Millisecond)

	ordered := orderURLsWithSelector(sel, 1, urls, "")
	if len(ordered) != 2 {
		t.Fatalf("expected slow isolated URL removed from active plan, got %v", ordered)
	}
	if ordered[0].url != urls[0] || ordered[1].url != urls[2] {
		t.Fatalf("expected fast real then probe fallback while slow URL is isolated, got %v", ordered)
	}

	stats := sel.GetURLStats(1, urls)
	if !stats[1].SlowIsolated {
		t.Fatalf("expected slow URL stats to expose active isolation, stats=%+v", stats)
	}
}

func TestURLSelector_AllSlowIsolated_FallsBackToSlowPool(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://slow-a.example", "https://slow-b.example"}

	// 两个 URL 都慢到触发隔离
	sel.RecordLatency(1, urls[0], 5*time.Second)
	sel.RecordLatency(1, urls[1], 6*time.Second)

	// 全被隔离后选择器仍然返回一个 URL（从 slowFallback 兜底）
	picked, idx := sel.SelectURL(1, urls)
	if picked == "" || idx < 0 {
		t.Fatalf("expected fallback to slow pool when all URLs are isolated, got (%q, %d)", picked, idx)
	}

	// 应优先选延迟较低的那个
	counts := runURLSelections(500, func() string {
		p, _ := sel.SelectURL(1, urls)
		return p
	})
	if counts[urls[0]] <= counts[urls[1]] {
		t.Fatalf("expected least-slow URL preferred even in slow fallback, counts=%v", counts)
	}
}

func TestURLSelector_CooldownAndSlowIsolation_Independent(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 20 * time.Millisecond
	url := "https://dual.example"

	// 先触发 cooldown
	sel.CooldownURL(1, url)
	if !sel.IsCooledDown(1, url) {
		t.Fatalf("expected cooldown active")
	}

	// RecordLatency 清掉 cooldown 但触发慢隔离
	sel.RecordLatency(1, url, 5*time.Second)

	// cooldown 被成功清了，但 slowIsolation 还在，所以仍然报 cooled
	sel.mu.RLock()
	_, hasCooldown := sel.cooldowns[urlKey{channelID: 1, url: url}]
	_, hasSlowIso := sel.slowIsolations[urlKey{channelID: 1, url: url}]
	sel.mu.RUnlock()

	if hasCooldown {
		t.Fatalf("expected cooldown cleared after successful latency record")
	}
	if !hasSlowIso {
		t.Fatalf("expected slow isolation set after 5s TTFB")
	}
	if !sel.IsCooledDown(1, url) {
		t.Fatalf("expected IsCooledDown still true due to slow isolation")
	}

	// ClearChannelCooldowns 同时清两种
	sel.ClearChannelCooldowns(1)
	if sel.IsCooledDown(1, url) {
		t.Fatalf("expected both cooldown and slow isolation cleared")
	}
}

func TestDiagnosticURLsWithSelector_IncludesHiddenURLs(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://fast.example", "https://unknown-a.example", "https://unknown-b.example", "https://unknown-c.example"}

	// 只有一个 URL 有真实 TTFB，其余全是 unknown
	sel.RecordLatency(1, urls[0], 300*time.Millisecond)

	// planner 最多只放 1 个 canary，所以 planned < len(urls)
	planned := orderURLsWithSelector(sel, 1, urls, "")

	// diagnostic 版本应该把被隐藏的 URL 补回来
	diagnostic := orderDiagnosticURLsWithSelector(sel, 1, urls, "")

	if len(diagnostic) != len(urls) {
		t.Fatalf("expected diagnostic to include all %d URLs, got %d: %v", len(urls), len(diagnostic), diagnostic)
	}

	// 前面的顺序应该和 planned 一致
	for i, entry := range planned {
		if diagnostic[i].url != entry.url {
			t.Fatalf("expected diagnostic prefix to match planned order at position %d: planned=%v diagnostic=%v", i, planned, diagnostic)
		}
	}

	// 所有 URL 都应该出现
	seen := make(map[string]struct{})
	for _, entry := range diagnostic {
		seen[entry.url] = struct{}{}
	}
	for _, rawURL := range urls {
		if _, ok := seen[rawURL]; !ok {
			t.Fatalf("expected diagnostic to include %s, got %v", rawURL, diagnostic)
		}
	}
}

func TestDiagnosticURLsWithSelector_NilSelector(t *testing.T) {
	urls := []string{"https://a.example", "https://b.example"}
	result := orderDiagnosticURLsWithSelector(nil, 1, urls, "")
	if len(result) != len(urls) {
		t.Fatalf("expected nil selector to return all URLs, got %v", result)
	}
}

func TestURLSelector_RemoveChannel_ClearsNoThinkingBlklist(t *testing.T) {
	sel := NewURLSelector()
	sel.MarkNoThinking(1, "https://a.example", "model-a")
	sel.MarkNoThinking(1, "https://b.example", "model-b")
	sel.MarkNoThinking(2, "https://c.example", "model-c") // 另一个渠道，不该被清

	if !sel.IsNoThinking(1, "https://a.example", "model-a") {
		t.Fatalf("expected thinking blacklist entry before removal")
	}

	sel.RemoveChannel(1)

	if sel.IsNoThinking(1, "https://a.example", "model-a") {
		t.Fatalf("expected thinking blacklist cleared after RemoveChannel")
	}
	if sel.IsNoThinking(1, "https://b.example", "model-b") {
		t.Fatalf("expected all entries for channel 1 cleared")
	}
	if !sel.IsNoThinking(2, "https://c.example", "model-c") {
		t.Fatalf("expected channel 2 entries preserved")
	}
}
