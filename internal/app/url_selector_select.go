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
	affinityEscapeMultiplier  = 2.0
)

// sortedURL 排序后的 URL 条目。
type sortedURL struct {
	url string
	idx int
}

type selectorCandidate struct {
	url              string
	idx              int
	realTTFB         float64
	probeLatency     float64
	effectiveLatency float64
	latencySource    string
	successRate      float64
	cooled           bool
}

func (c selectorCandidate) hasLatency() bool {
	return c.latencySource != latencySourceUnknown
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
	case ms > 4000:
		return ms * 5
	case ms > 2500:
		return ms * 3
	case ms > 1200:
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
	return candidateBaseWeight(c) * successRate
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
		c.cooled = true
	}
	if model != "" {
		umk := urlModelKey{channelID: channelID, url: rawURL, model: model}
		if expiry, ok := s.noThinkingBlklist[umk]; ok && now.Before(expiry) {
			c.cooled = true
		}
	}

	return c
}

func (s *URLSelector) buildCandidatesLocked(channelID int64, model string, urls []string, now time.Time) ([]selectorCandidate, map[string]int) {
	urlIndex := make(map[string]int, len(urls))
	candidates := make([]selectorCandidate, len(urls))
	for i, rawURL := range urls {
		urlIndex[rawURL] = i
		candidates[i] = s.buildCandidateLocked(channelID, model, rawURL, i, now)
	}
	return candidates, urlIndex
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
	cooled := make([]selectorCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.cooled {
			cooled = append(cooled, c)
			continue
		}
		available = append(available, c)
	}
	if len(available) == 0 {
		available = cooled
	}
	return available, cooled
}

func splitCandidatesByKnownness(candidates []selectorCandidate) ([]selectorCandidate, []selectorCandidate) {
	known := make([]selectorCandidate, 0, len(candidates))
	unknown := make([]selectorCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.hasLatency() {
			known = append(known, c)
			continue
		}
		unknown = append(unknown, c)
	}
	return known, unknown
}

func (s *URLSelector) shouldUseAffinityLocked(aff selectorCandidate, available []selectorCandidate) bool {
	if aff.cooled {
		return false
	}
	if aff.realTTFB < 0 {
		return true
	}

	bestAlternative := math.MaxFloat64
	for _, c := range available {
		if c.url == aff.url || c.cooled || !c.hasLatency() {
			continue
		}
		if c.effectiveLatency < bestAlternative {
			bestAlternative = c.effectiveLatency
		}
	}
	if bestAlternative == math.MaxFloat64 {
		return true
	}
	return aff.realTTFB <= bestAlternative*affinityEscapeMultiplier
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

// SelectURLForModel 从候选 URL 中选一个最优的，带模型亲和性。
func (s *URLSelector) SelectURLForModel(channelID int64, model string, urls []string) (string, int) {
	if len(urls) == 0 {
		return "", -1
	}
	if len(urls) == 1 {
		return urls[0], 0
	}

	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	candidates, urlIndex := s.buildCandidatesLocked(channelID, model, urls, now)
	available, _ := splitCandidatesByAvailability(candidates)

	if model != "" {
		ak := modelAffinityKey{channelID: channelID, model: model}
		if aff, ok := s.affinities[ak]; ok {
			if idx, found := urlIndex[aff.url]; found {
				candidate := candidates[idx]
				if s.shouldUseAffinityLocked(candidate, available) {
					return candidate.url, candidate.idx
				}
			}
		}
	}

	known, unknown := splitCandidatesByKnownness(available)
	hasGoodKnown := !shouldExploreUnknown(known)
	if len(unknown) > 0 && !hasGoodKnown {
		pick := unknown[rand.IntN(len(unknown))]
		return pick.url, pick.idx
	}

	pool := known
	if !hasGoodKnown && len(unknown) > 0 {
		pool = append(pool, unknown...)
	}
	if len(pool) == 0 {
		if len(unknown) > 0 {
			pick := unknown[rand.IntN(len(unknown))]
			return pick.url, pick.idx
		}
		return urls[0], 0
	}

	pick := weightedRandomCandidate(pool)
	return pick.url, pick.idx
}

// SortURLs 返回按统一评分排序的 URL 列表，用于故障切换顺序。
func (s *URLSelector) SortURLs(channelID int64, urls []string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}

	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	candidates, _ := s.buildCandidatesLocked(channelID, "", urls, now)

	// 同分时先打散，别总是把同一个 URL 固定排前面。
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})

	slices.SortStableFunc(candidates, func(left, right selectorCandidate) int {
		if left.cooled != right.cooled {
			if !left.cooled {
				return -1
			}
			return 1
		}

		leftKnown, rightKnown := left.hasLatency(), right.hasLatency()
		if leftKnown != rightKnown {
			if leftKnown {
				return -1
			}
			return 1
		}

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

	result := make([]sortedURL, len(candidates))
	for i, c := range candidates {
		result[i] = sortedURL{url: c.url, idx: c.idx}
	}
	return result
}
