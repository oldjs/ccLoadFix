package app

import (
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

const (
	latencySourceUnknown = "unknown"
	latencySourceProbe   = "probe"
	latencySourceTTFB    = "ttfb"

	defaultEffectiveLatencyMS = 500.0
	softAffinityMultiplier    = 1.5
)

// sortedURL 排序后的 URL 条目。
type sortedURL struct {
	url string
	idx int
}

type selectorCandidate struct {
	url               string
	idx               int
	realTTFB          float64
	probeLatency      float64
	effectiveLatency  float64
	latencySource     string
	successRate       float64
	cooldownActive    bool
	slowIsolated      bool
	noThinkingBlocked bool
	cooled            bool
	affinity          bool
}

func normalizeSelectorLatencyMS(ms float64) float64 {
	// 无效值 → 当作未知（500ms默认）
	// 地板1ms：正常HTTP请求不可能<1ms，低于此值说明测量异常
	// [FIX] 2026-04: 原地板100ms导致所有低延迟URL被clamp到500ms，
	// 反而比200ms的URL更"慢"——权重函数是 1/latency，500>200 意味着权重更低。
	// 生产环境中低延迟本地代理、同机房URL都会被错误惩罚。
	if ms < 1 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return defaultEffectiveLatencyMS
	}
	return ms
}

func penalizeSlowTTFBMS(ms float64) float64 {
	ms = normalizeSelectorLatencyMS(ms)
	switch {
	case ms >= 4000:
		return ms * 5
	case ms >= 2500:
		return ms * 3
	case ms >= 1200:
		return ms * 2
	default:
		return ms
	}
}

func candidateBaseWeight(c selectorCandidate) float64 {
	latency := c.effectiveLatency
	if latency <= 0 {
		latency = defaultEffectiveLatencyMS
	}
	return 1.0 / latency
}

func candidateScore(c selectorCandidate) float64 {
	successRate := c.successRate
	if successRate <= 0 || math.IsNaN(successRate) || math.IsInf(successRate, 0) {
		successRate = 0.05
	}
	if successRate < 0.05 {
		successRate = 0.05
	}
	if successRate > 1 {
		successRate = 1
	}
	score := candidateBaseWeight(c) * successRate
	if c.affinity {
		score *= softAffinityMultiplier
	}
	return score
}

// SelectURL 从候选 URL 中选一个最优的，不带模型亲和性。
func (s *URLSelector) SelectURL(channelID int64, urls []string) (string, int) {
	return s.SelectURLForModel(channelID, "", urls)
}

// urlSuccessRate 计算 URL 成功率（调用方已持有读锁）
func urlSuccessRate(sh *urlShard, key urlKey) float64 {
	rc, ok := sh.requests[key]
	if !ok || (rc.success+rc.failure) == 0 {
		return 1.0
	}
	return float64(rc.success) / float64(rc.success+rc.failure)
}

// effectiveLatencyInShard 获取有效延迟（调用方已持有读锁）
func effectiveLatencyInShard(sh *urlShard, key urlKey) (float64, string, bool) {
	if e, ok := sh.latencies[key]; ok && e != nil {
		return penalizeSlowTTFBMS(e.value), latencySourceTTFB, true
	}
	if e, ok := sh.probeLatencies[key]; ok && e != nil {
		return normalizeSelectorLatencyMS(e.value), latencySourceProbe, true
	}
	return -1, latencySourceUnknown, false
}

// buildCandidate 构建单个候选（调用方已持有读锁）
func buildCandidate(sh *urlShard, channelID int64, model, rawURL string, idx int, now time.Time) selectorCandidate {
	key := urlKey{channelID: channelID, url: rawURL}
	c := selectorCandidate{
		url:              rawURL,
		idx:              idx,
		realTTFB:         -1,
		probeLatency:     -1,
		effectiveLatency: -1,
		latencySource:    latencySourceUnknown,
		successRate:      urlSuccessRate(sh, key),
	}

	if e, ok := sh.latencies[key]; ok && e != nil {
		c.realTTFB = normalizeSelectorLatencyMS(e.value)
	}
	if e, ok := sh.probeLatencies[key]; ok && e != nil {
		c.probeLatency = normalizeSelectorLatencyMS(e.value)
	}
	if latency, source, known := effectiveLatencyInShard(sh, key); known {
		c.effectiveLatency = latency
		c.latencySource = source
	}

	if cd, ok := sh.cooldowns[key]; ok && now.Before(cd.until) {
		c.cooldownActive = true
		c.cooled = true
	}
	if until, ok := sh.slowIsolations[key]; ok && now.Before(until) {
		c.slowIsolated = true
		c.cooled = true
	}
	if model != "" {
		umk := urlModelKey{channelID: channelID, url: rawURL, model: model}
		if expiry, ok := sh.noThinkingBlklist[umk]; ok && now.Before(expiry) {
			c.noThinkingBlocked = true
			c.cooled = true
		}
	}

	return c
}

// buildCandidates 构建所有候选（调用方已持有读锁）
func buildCandidates(sh *urlShard, channelID int64, model string, urls []string, now time.Time) []selectorCandidate {
	candidates := make([]selectorCandidate, len(urls))
	for i, rawURL := range urls {
		candidates[i] = buildCandidate(sh, channelID, model, rawURL, i, now)
	}
	return candidates
}

func shouldExploreUnknown(known []selectorCandidate) bool {
	for _, c := range known {
		if c.successRate > 0.5 {
			return false
		}
	}
	return true
}

func splitCandidatesByAvailability(candidates []selectorCandidate) ([]selectorCandidate, []selectorCandidate) {
	available := make([]selectorCandidate, 0, len(candidates))
	cooledFallback := make([]selectorCandidate, 0, len(candidates))
	slowFallback := make([]selectorCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.noThinkingBlocked {
			continue
		}
		if c.slowIsolated {
			slowFallback = append(slowFallback, c)
			continue
		}
		if c.cooldownActive {
			cooledFallback = append(cooledFallback, c)
			continue
		}
		available = append(available, c)
	}
	if len(available) == 0 {
		switch {
		case len(cooledFallback) > 0:
			available = cooledFallback
			cooledFallback = nil
		case len(slowFallback) > 0:
			available = slowFallback
		}
	}
	return available, cooledFallback
}

func splitCandidatesBySource(candidates []selectorCandidate) ([]selectorCandidate, []selectorCandidate, []selectorCandidate) {
	real := make([]selectorCandidate, 0, len(candidates))
	probeOnly := make([]selectorCandidate, 0, len(candidates))
	unknown := make([]selectorCandidate, 0, len(candidates))
	for _, c := range candidates {
		switch c.latencySource {
		case latencySourceTTFB:
			real = append(real, c)
		case latencySourceProbe:
			probeOnly = append(probeOnly, c)
		default:
			unknown = append(unknown, c)
		}
	}
	return real, probeOnly, unknown
}

func weightedRandomCandidate(candidates []selectorCandidate) selectorCandidate {
	totalWeight := 0.0
	weights := make([]float64, len(candidates))
	for i, c := range candidates {
		weights[i] = candidateScore(c)
		totalWeight += weights[i]
	}
	if totalWeight <= 0 || math.IsNaN(totalWeight) || math.IsInf(totalWeight, 0) {
		return candidates[rand.IntN(len(candidates))]
	}
	r := rand.Float64() * totalWeight
	for i, weight := range weights {
		r -= weight
		if r <= 0 {
			return candidates[i]
		}
	}
	return candidates[len(candidates)-1]
}

func removeCandidateByURL(candidates []selectorCandidate, rawURL string) []selectorCandidate {
	trimmed := make([]selectorCandidate, 0, len(candidates))
	removed := false
	for _, c := range candidates {
		if !removed && c.url == rawURL {
			removed = true
			continue
		}
		trimmed = append(trimmed, c)
	}
	return trimmed
}

func sortCandidatesByScore(candidates []selectorCandidate) []selectorCandidate {
	if len(candidates) <= 1 {
		return append([]selectorCandidate(nil), candidates...)
	}
	sorted := append([]selectorCandidate(nil), candidates...)

	// 同分时先打散，别让同权重 URL 永远按原顺序排。
	rand.Shuffle(len(sorted), func(i, j int) {
		sorted[i], sorted[j] = sorted[j], sorted[i]
	})

	slices.SortStableFunc(sorted, func(left, right selectorCandidate) int {
		leftScore, rightScore := candidateScore(left), candidateScore(right)
		if leftScore > rightScore {
			return -1
		}
		if leftScore < rightScore {
			return 1
		}
		if left.successRate > right.successRate {
			return -1
		}
		if left.successRate < right.successRate {
			return 1
		}
		if left.effectiveLatency < right.effectiveLatency {
			return -1
		}
		if left.effectiveLatency > right.effectiveLatency {
			return 1
		}
		return 0
	})
	return sorted
}

func pickCanaryCandidate(candidates []selectorCandidate) selectorCandidate {
	return weightedRandomCandidate(candidates)
}

// markAffinity 标记亲和性候选（调用方已持有读锁）
func markAffinity(sh *urlShard, channelID int64, model string, candidates []selectorCandidate) {
	if model == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}
	aff, ok := sh.affinities[ak]
	if !ok || aff == nil || aff.url == "" {
		return
	}
	for i := range candidates {
		if candidates[i].url == aff.url {
			candidates[i].affinity = true
			return
		}
	}
}

func appendCanaryIfNeeded(ordered []selectorCandidate, primary []selectorCandidate, unknown []selectorCandidate) []selectorCandidate {
	if len(unknown) == 0 {
		return ordered
	}
	if len(primary) > 0 && !shouldExploreUnknown(primary) {
		return ordered
	}
	return append(ordered, pickCanaryCandidate(unknown))
}

func canaryCandidate(primary []selectorCandidate, unknown []selectorCandidate) (selectorCandidate, bool) {
	if len(unknown) == 0 {
		return selectorCandidate{}, false
	}
	if len(primary) > 0 && !shouldExploreUnknown(primary) {
		return selectorCandidate{}, false
	}
	return pickCanaryCandidate(unknown), true
}

func buildPlannedOrder(primary []selectorCandidate) []selectorCandidate {
	if len(primary) == 0 {
		return nil
	}
	first := weightedRandomCandidate(primary)
	ordered := []selectorCandidate{first}
	ordered = append(ordered, sortCandidatesByScore(removeCandidateByURL(primary, first.url))...)
	return ordered
}

// appendRemainingUnknowns 把还没出现在 ordered 里的 unknown URL 追加到末尾。
func appendRemainingUnknowns(ordered []selectorCandidate, unknown []selectorCandidate) []selectorCandidate {
	if len(unknown) == 0 {
		return ordered
	}
	seen := make(map[string]struct{}, len(ordered))
	for _, c := range ordered {
		seen[c.url] = struct{}{}
	}
	for _, c := range unknown {
		if _, ok := seen[c.url]; !ok {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

func appendCooldownFallbacks(ordered []selectorCandidate, cooldownFallback []selectorCandidate) []selectorCandidate {
	if len(cooldownFallback) == 0 {
		return ordered
	}
	// 冷却URL也得按延迟源分层
	real, probeOnly, unknown := splitCandidatesBySource(cooldownFallback)
	ordered = append(ordered, sortCandidatesByScore(real)...)
	ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
	ordered = append(ordered, sortCandidatesByScore(unknown)...)
	return ordered
}

// planWithCanary 把 canary 混入 primary 的首跳池做加权随机
func planWithCanary(primary []selectorCandidate, canary selectorCandidate, extraTiers []selectorCandidate, cooledFallback []selectorCandidate) []selectorCandidate {
	firstPool := append([]selectorCandidate(nil), primary...)
	firstPool = append(firstPool, canary)
	first := weightedRandomCandidate(firstPool)

	ordered := []selectorCandidate{first}
	if first.url != canary.url {
		ordered = append(ordered, sortCandidatesByScore(removeCandidateByURL(primary, first.url))...)
		ordered = append(ordered, sortCandidatesByScore(extraTiers)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = append(ordered, canary)
	} else {
		ordered = append(ordered, sortCandidatesByScore(primary)...)
		ordered = append(ordered, sortCandidatesByScore(extraTiers)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
	}
	return ordered
}

// planCandidates 规划候选URL的尝试顺序（调用方已持有读锁）
func planCandidates(sh *urlShard, channelID int64, model string, urls []string, now time.Time) []selectorCandidate {
	candidates := buildCandidates(sh, channelID, model, urls, now)
	markAffinity(sh, channelID, model, candidates)
	active, cooledFallback := splitCandidatesByAvailability(candidates)
	real, probeOnly, unknown := splitCandidatesBySource(active)

	ordered := make([]selectorCandidate, 0, len(active))
	switch {
	case len(real) > 0:
		if canary, ok := canaryCandidate(real, unknown); ok {
			planned := planWithCanary(real, canary, probeOnly, cooledFallback)
			return appendRemainingUnknowns(planned, unknown)
		}
		ordered = append(ordered, buildPlannedOrder(real)...)
		ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, real, unknown)
		ordered = appendRemainingUnknowns(ordered, unknown)
	case len(probeOnly) > 0:
		if canary, ok := canaryCandidate(probeOnly, unknown); ok {
			planned := planWithCanary(probeOnly, canary, nil, cooledFallback)
			return appendRemainingUnknowns(planned, unknown)
		}
		ordered = append(ordered, buildPlannedOrder(probeOnly)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, probeOnly, unknown)
		ordered = appendRemainingUnknowns(ordered, unknown)
	case len(unknown) > 0:
		ordered = append(ordered, buildPlannedOrder(unknown)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
	}

	return ordered
}

func candidatesToSortedURLs(candidates []selectorCandidate) []sortedURL {
	result := make([]sortedURL, len(candidates))
	for i, c := range candidates {
		result[i] = sortedURL{url: c.url, idx: c.idx}
	}
	return result
}

// planURLsForModel 规划URL的尝试顺序（自动按 channelID 取分片锁）
func (s *URLSelector) planURLsForModel(channelID int64, model string, urls []string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}

	now := time.Now()
	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	return candidatesToSortedURLs(planCandidates(sh, channelID, model, urls, now))
}

// SelectURLForModel 从候选 URL 中选一个最优的
func (s *URLSelector) SelectURLForModel(channelID int64, model string, urls []string) (string, int) {
	planned := s.planURLsForModel(channelID, model, urls)
	if len(planned) == 0 {
		return "", -1
	}
	return planned[0].url, planned[0].idx
}

// SortURLs 返回不带模型亲和性的计划顺序
func (s *URLSelector) SortURLs(channelID int64, urls []string) []sortedURL {
	return s.planURLsForModel(channelID, "", urls)
}
