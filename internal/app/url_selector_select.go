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

func (c selectorCandidate) hasLatency() bool {
	return c.latencySource != latencySourceUnknown
}

func (c selectorCandidate) hasRealTTFB() bool {
	return c.latencySource == latencySourceTTFB
}

func normalizeSelectorLatencyMS(ms float64) float64 {
	if ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return defaultEffectiveLatencyMS
	}
	if ms < 100 {
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

// urlSuccessRateLocked 计算 URL 成功率。
func (s *URLSelector) urlSuccessRateLocked(key urlKey) float64 {
	rc, ok := s.requests[key]
	if !ok || (rc.success+rc.failure) == 0 {
		return 1.0
	}
	return float64(rc.success) / float64(rc.success+rc.failure)
}

func (s *URLSelector) effectiveLatencyLocked(key urlKey) (float64, string, bool) {
	if e, ok := s.latencies[key]; ok && e != nil {
		return penalizeSlowTTFBMS(e.value), latencySourceTTFB, true
	}
	if e, ok := s.probeLatencies[key]; ok && e != nil {
		return normalizeSelectorLatencyMS(e.value), latencySourceProbe, true
	}
	return -1, latencySourceUnknown, false
}

func (s *URLSelector) buildCandidateLocked(channelID int64, model, rawURL string, idx int, now time.Time) selectorCandidate {
	key := urlKey{channelID: channelID, url: rawURL}
	c := selectorCandidate{
		url:              rawURL,
		idx:              idx,
		realTTFB:         -1,
		probeLatency:     -1,
		effectiveLatency: -1,
		latencySource:    latencySourceUnknown,
		successRate:      s.urlSuccessRateLocked(key),
	}

	if e, ok := s.latencies[key]; ok && e != nil {
		c.realTTFB = normalizeSelectorLatencyMS(e.value)
	}
	if e, ok := s.probeLatencies[key]; ok && e != nil {
		c.probeLatency = normalizeSelectorLatencyMS(e.value)
	}
	if latency, source, known := s.effectiveLatencyLocked(key); known {
		c.effectiveLatency = latency
		c.latencySource = source
	}

	if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
		c.cooldownActive = true
		c.cooled = true
	}
	if until, ok := s.slowIsolations[key]; ok && now.Before(until) {
		c.slowIsolated = true
		c.cooled = true
	}
	if model != "" {
		umk := urlModelKey{channelID: channelID, url: rawURL, model: model}
		if expiry, ok := s.noThinkingBlklist[umk]; ok && now.Before(expiry) {
			c.noThinkingBlocked = true
			c.cooled = true
		}
	}

	return c
}

func (s *URLSelector) buildCandidatesLocked(channelID int64, model string, urls []string, now time.Time) []selectorCandidate {
	candidates := make([]selectorCandidate, len(urls))
	for i, rawURL := range urls {
		candidates[i] = s.buildCandidateLocked(channelID, model, rawURL, i, now)
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

func (s *URLSelector) markAffinityLocked(channelID int64, model string, candidates []selectorCandidate) {
	if model == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}
	aff, ok := s.affinities[ak]
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

func appendCooldownFallbacks(ordered []selectorCandidate, cooldownFallback []selectorCandidate) []selectorCandidate {
	if len(cooldownFallback) == 0 {
		return ordered
	}
	real, probeOnly, _ := splitCandidatesBySource(cooldownFallback)
	ordered = append(ordered, sortCandidatesByScore(real)...)
	ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
	return ordered
}

func (s *URLSelector) planCandidatesLocked(channelID int64, model string, urls []string, now time.Time) []selectorCandidate {
	candidates := s.buildCandidatesLocked(channelID, model, urls, now)
	s.markAffinityLocked(channelID, model, candidates)
	active, cooledFallback := splitCandidatesByAvailability(candidates)
	real, probeOnly, unknown := splitCandidatesBySource(active)

	ordered := make([]selectorCandidate, 0, len(active))
	switch {
	case len(real) > 0:
		if canary, ok := canaryCandidate(real, unknown); ok {
			firstPool := append([]selectorCandidate(nil), real...)
			firstPool = append(firstPool, canary)
			first := weightedRandomCandidate(firstPool)
			ordered = append(ordered, first)
			if first.url != canary.url {
				ordered = append(ordered, sortCandidatesByScore(removeCandidateByURL(real, first.url))...)
				ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
				ordered = appendCooldownFallbacks(ordered, cooledFallback)
				ordered = append(ordered, canary)
				return ordered
			}
			ordered = append(ordered, sortCandidatesByScore(real)...)
			ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
			ordered = appendCooldownFallbacks(ordered, cooledFallback)
			return ordered
		}
		ordered = append(ordered, buildPlannedOrder(real)...)
		ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, real, unknown)
	case len(probeOnly) > 0:
		if canary, ok := canaryCandidate(probeOnly, unknown); ok {
			firstPool := append([]selectorCandidate(nil), probeOnly...)
			firstPool = append(firstPool, canary)
			first := weightedRandomCandidate(firstPool)
			ordered = append(ordered, first)
			if first.url != canary.url {
				ordered = append(ordered, sortCandidatesByScore(removeCandidateByURL(probeOnly, first.url))...)
				ordered = appendCooldownFallbacks(ordered, cooledFallback)
				ordered = append(ordered, canary)
				return ordered
			}
			ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
			ordered = appendCooldownFallbacks(ordered, cooledFallback)
			return ordered
		}
		ordered = append(ordered, buildPlannedOrder(probeOnly)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, probeOnly, unknown)
	case len(unknown) > 0:
		ordered = append(ordered, pickCanaryCandidate(unknown))
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

func (s *URLSelector) planURLsForModel(channelID int64, model string, urls []string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}

	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	return candidatesToSortedURLs(s.planCandidatesLocked(channelID, model, urls, now))
}

// SelectURLForModel 从候选 URL 中选一个最优的，模型亲和性只作为软偏置。
func (s *URLSelector) SelectURLForModel(channelID int64, model string, urls []string) (string, int) {
	planned := s.planURLsForModel(channelID, model, urls)
	if len(planned) == 0 {
		return "", -1
	}
	return planned[0].url, planned[0].idx
}

// SortURLs 返回不带模型亲和性的计划顺序，用于诊断或非模型场景。
func (s *URLSelector) SortURLs(channelID int64, urls []string) []sortedURL {
	return s.planURLsForModel(channelID, "", urls)
}
