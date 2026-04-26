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

	// rrWeightScale SmoothWRR 整数权重缩放因子。
	// 权重公式：scale * successRate / effectiveLatency
	// 取 1e6 是为了让极端慢的 URL（penalty 后 40000ms）也能保持权重 25 而非贴底 1，
	// 确保最慢 vs 次慢之间仍保持比例可分辨；同时不会让 totalWeight 在 64bit int 下溢出。
	rrWeightScale = 1e6
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
	// warm 主亲和失效时的首跳候选标记。affinity 未命中时由 markWarmCandidate 打上。
	warm bool
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

// candidateScore 用于 fallback 排序的综合得分。
// 注意：从 2026-04 起不再叠加 affinity 倍增 —— 首跳由 SmoothWRR 决定，affinity 不再
// 通过权重影响选择。score 仅在 sortCandidatesByScore 里给"非首跳"的 URL 排个合理顺序，
// 确保主 URL 失败后 fallback 优先用快且稳的备份。
func candidateScore(c selectorCandidate) float64 {
	return candidateBaseWeight(c) * normalizeSuccessRate(c.successRate)
}

// normalizeSuccessRate 把成功率钳到 [0.05, 1]，避免 0 或异常值
// 0.05 地板：让长期失败的 URL 仍保留少量轮询配额（用于探测复活）
func normalizeSuccessRate(rate float64) float64 {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0.05
	}
	if rate < 0.05 {
		return 0.05
	}
	if rate > 1 {
		return 1
	}
	return rate
}

// computeRRWeight 计算 SmoothWRR 用的整数权重（纯函数，方便 admin 面板复用）。
// 权重 = scale * successRate / effectiveLatency，最小为 1。
// 设计意图：
//   - 低延迟 URL 仍然拿到更高份额（与原加权随机相同的比例），但被周期访问而非概率独占
//   - 长尾慢 URL 即便贴 1 也仍会被选中（每 totalWeight 次轮一次），保证号池利用率
func computeRRWeight(effectiveLatencyMs, successRate float64) int64 {
	if effectiveLatencyMs <= 0 {
		effectiveLatencyMs = defaultEffectiveLatencyMS
	}
	successRate = normalizeSuccessRate(successRate)
	return max(int64(rrWeightScale*successRate/effectiveLatencyMs), 1)
}

// candidateRRWeight 把候选转换成 SmoothWRR 用的整数权重
func candidateRRWeight(c selectorCandidate) int64 {
	return computeRRWeight(c.effectiveLatency, c.successRate)
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

// smoothWRRPick 用 SmoothWRR 从候选里选一个。balancer 为 nil 时退化为按 score 取最高（兜底，不应在生产发生）
func smoothWRRPick(balancer *URLSmoothWeightedRR, channelID int64, candidates []selectorCandidate) selectorCandidate {
	if len(candidates) == 0 {
		return selectorCandidate{}
	}
	if len(candidates) == 1 {
		if balancer != nil {
			balancer.Select(channelID, []string{candidates[0].url}, []int64{1})
		}
		return candidates[0]
	}
	if balancer == nil {
		// 兜底路径：找 score 最高的；同分时取索引小的（确定性）
		bestIdx := 0
		bestScore := candidateScore(candidates[0])
		for i := 1; i < len(candidates); i++ {
			s := candidateScore(candidates[i])
			if s > bestScore {
				bestScore = s
				bestIdx = i
			}
		}
		return candidates[bestIdx]
	}
	urls := make([]string, len(candidates))
	weights := make([]int64, len(candidates))
	for i, c := range candidates {
		urls[i] = c.url
		weights[i] = candidateRRWeight(c)
	}
	selected := balancer.Select(channelID, urls, weights)
	for _, c := range candidates {
		if c.url == selected {
			return c
		}
	}
	// SmoothWRR 没匹配上（理论不会发生）—— 退到第一个候选
	return candidates[0]
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

// pickCanaryCandidate 从 unknown 候选里选一个做探索探测。
// 没历史数据时所有 unknown 的权重相同（默认 500ms 等价），SmoothWRR 退化为均匀轮询，
// 保证号池里所有未被探过的 URL 都能轮到，配合 buildCandidates 的逻辑形成完整探索。
func pickCanaryCandidate(balancer *URLSmoothWeightedRR, channelID int64, candidates []selectorCandidate) selectorCandidate {
	return smoothWRRPick(balancer, channelID, candidates)
}

// markAffinity 标记亲和性候选（调用方已持有读锁）。返回是否命中，便于上层决定是否再尝试 warm 备选。
func markAffinity(sh *urlShard, channelID int64, model string, candidates []selectorCandidate) bool {
	if model == "" {
		return false
	}
	ak := modelAffinityKey{channelID: channelID, model: model}
	aff, ok := sh.affinities[ak]
	if !ok || aff == nil || aff.url == "" {
		return false
	}
	for i := range candidates {
		if candidates[i].url == aff.url {
			candidates[i].affinity = true
			return true
		}
	}
	return false
}

func appendCanaryIfNeeded(ordered []selectorCandidate, primary []selectorCandidate, unknown []selectorCandidate, balancer *URLSmoothWeightedRR, channelID int64) []selectorCandidate {
	if len(unknown) == 0 {
		return ordered
	}
	if len(primary) > 0 && !shouldExploreUnknown(primary) {
		return ordered
	}
	return append(ordered, pickCanaryCandidate(balancer, channelID, unknown))
}

func canaryCandidate(primary []selectorCandidate, unknown []selectorCandidate, balancer *URLSmoothWeightedRR, channelID int64) (selectorCandidate, bool) {
	if len(unknown) == 0 {
		return selectorCandidate{}, false
	}
	if len(primary) > 0 && !shouldExploreUnknown(primary) {
		return selectorCandidate{}, false
	}
	return pickCanaryCandidate(balancer, channelID, unknown), true
}

func buildPlannedOrder(primary []selectorCandidate, balancer *URLSmoothWeightedRR, channelID int64) []selectorCandidate {
	if len(primary) == 0 {
		return nil
	}
	first := pickFirstHop(primary, balancer, channelID)
	ordered := []selectorCandidate{first}
	ordered = append(ordered, sortCandidatesByScore(removeCandidateByURL(primary, first.url))...)
	return ordered
}

// pickFirstHop 用平滑加权轮询（SmoothWRR）选首跳。
// 2026-04 重构：彻底去掉 affinity / warm 对首跳的影响。
//   - 此前的"亲和硬选 + 1.5x 加权"在生产环境造成大号池只用几个 URL 的扎堆问题
//   - 改用 SmoothWRR 后，所有可用 URL 按 1/EWMA 权重比例被周期访问，号池利用率显著提升
//
// affinity / warm 字段仍由 markAffinity / markWarmCandidate 标记，但仅用于 admin 状态展示，
// 不再驱动选择逻辑。
func pickFirstHop(primary []selectorCandidate, balancer *URLSmoothWeightedRR, channelID int64) selectorCandidate {
	return smoothWRRPick(balancer, channelID, primary)
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

// planWithCanary 把 canary 混入 primary 的首跳池，整池走 SmoothWRR 选首跳。
// canary 的权重由它自己的 candidateRRWeight 决定（unknown URL 会拿到默认 500ms 对应的权重），
// 在 SmoothWRR 下天然会被周期选中，无需特殊提权。
func planWithCanary(primary []selectorCandidate, canary selectorCandidate, extraTiers []selectorCandidate, cooledFallback []selectorCandidate, balancer *URLSmoothWeightedRR, channelID int64) []selectorCandidate {
	firstPool := append([]selectorCandidate(nil), primary...)
	firstPool = append(firstPool, canary)
	first := smoothWRRPick(balancer, channelID, firstPool)

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
// balancer：SmoothWRR 决定首跳；nil 时退化为按 score 排序的兜底逻辑
func planCandidates(sh *urlShard, balancer *URLSmoothWeightedRR, channelID int64, model string, urls []string, now time.Time) []selectorCandidate {
	candidates := buildCandidates(sh, channelID, model, urls, now)
	// affinity / warm 仍然标记到 candidate 字段，但仅供 admin 状态展示，不影响选择
	if !markAffinity(sh, channelID, model, candidates) {
		markWarmCandidate(sh, channelID, model, candidates, now)
	}
	active, cooledFallback := splitCandidatesByAvailability(candidates)
	real, probeOnly, unknown := splitCandidatesBySource(active)

	ordered := make([]selectorCandidate, 0, len(active))
	switch {
	case len(real) > 0:
		if canary, ok := canaryCandidate(real, unknown, balancer, channelID); ok {
			planned := planWithCanary(real, canary, probeOnly, cooledFallback, balancer, channelID)
			return appendRemainingUnknowns(planned, unknown)
		}
		ordered = append(ordered, buildPlannedOrder(real, balancer, channelID)...)
		ordered = append(ordered, sortCandidatesByScore(probeOnly)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, real, unknown, balancer, channelID)
		ordered = appendRemainingUnknowns(ordered, unknown)
	case len(probeOnly) > 0:
		if canary, ok := canaryCandidate(probeOnly, unknown, balancer, channelID); ok {
			planned := planWithCanary(probeOnly, canary, nil, cooledFallback, balancer, channelID)
			return appendRemainingUnknowns(planned, unknown)
		}
		ordered = append(ordered, buildPlannedOrder(probeOnly, balancer, channelID)...)
		ordered = appendCooldownFallbacks(ordered, cooledFallback)
		ordered = appendCanaryIfNeeded(ordered, probeOnly, unknown, balancer, channelID)
		ordered = appendRemainingUnknowns(ordered, unknown)
	case len(unknown) > 0:
		ordered = append(ordered, buildPlannedOrder(unknown, balancer, channelID)...)
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

	return candidatesToSortedURLs(planCandidates(sh, s.urlBalancer, channelID, model, urls, now))
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
