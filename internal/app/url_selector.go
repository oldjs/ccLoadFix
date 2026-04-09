package app

import (
	"context"
	"math"
	"net"
	"sync"
	"time"
)

const (
	defaultURLSelectorCleanupInterval = time.Hour
	defaultURLSelectorLatencyMaxAge   = 24 * time.Hour
	defaultURLSelectorProbeTimeout    = 5 * time.Second
	slowTTFBIsolationThreshold        = 4 * time.Second
	slowTTFBSevereThreshold           = 10 * time.Second
)

// urlKey 标识渠道+URL的组合
type urlKey struct {
	channelID int64
	url       string
}

// modelAffinityKey 标识渠道+模型的组合，用于亲和性路由
type modelAffinityKey struct {
	channelID int64
	model     string
}

// affinityEntry 模型亲和性条目：记住上次成功的URL
type affinityEntry struct {
	url      string
	lastUsed time.Time
}

// urlModelKey 标识 (渠道, URL, 模型) 三元组，用于 thinking 黑名单
type urlModelKey struct {
	channelID int64
	url       string
	model     string
}

// ewmaValue 指数加权移动平均值
type ewmaValue struct {
	value    float64 // 当前EWMA值（毫秒）
	lastSeen time.Time
}

// urlCooldownState URL冷却状态
type urlCooldownState struct {
	until            time.Time
	consecutiveFails int
}

// urlRequestCount URL调用计数（内存）
type urlRequestCount struct {
	success                  int64
	failure                  int64
	consecutiveModelNotFound int // 连续"没这个模型"次数，成功后清零
}

// URLSelector 基于EWMA延迟、成功率和模型亲和性选择最优URL
type URLSelector struct {
	mu                sync.RWMutex
	latencies         map[urlKey]*ewmaValue // 真实请求的 TTFB EWMA
	probeLatencies    map[urlKey]*ewmaValue // 探测出来的 RTT 种子，只在没真实 TTFB 时兜底
	cooldowns         map[urlKey]urlCooldownState
	slowIsolations    map[urlKey]time.Time
	requests          map[urlKey]*urlRequestCount
	affinities        map[modelAffinityKey]*affinityEntry // 模型亲和性：上次成功的URL
	noThinkingBlklist map[urlModelKey]time.Time           // thinking黑名单：(URL,模型)→过期时间
	probing           map[urlKey]time.Time
	alpha             float64       // EWMA权重因子
	cooldownBase      time.Duration // 基础冷却时间
	cooldownMax       time.Duration // 最大冷却时间
	probeTimeout      time.Duration
	probeDial         func(ctx context.Context, network, address string) (net.Conn, error)
	// 低频清理调度，避免 map 长期只增不减。
	cleanupInterval time.Duration
	latencyMaxAge   time.Duration
	nextCleanup     time.Time
}

func normalizeLatencyMS(ttfb time.Duration) float64 {
	ms := float64(ttfb) / float64(time.Millisecond)
	if ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return 0.1
	}
	return ms
}

func (s *URLSelector) upsertLatencyLocked(latencyMap map[urlKey]*ewmaValue, key urlKey, ms float64, now time.Time) {
	if e, ok := latencyMap[key]; ok {
		e.value = s.alpha*ms + (1-s.alpha)*e.value
		e.lastSeen = now
		return
	}
	latencyMap[key] = &ewmaValue{value: ms, lastSeen: now}
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	now := time.Now()
	return &URLSelector{
		latencies:         make(map[urlKey]*ewmaValue),
		probeLatencies:    make(map[urlKey]*ewmaValue),
		cooldowns:         make(map[urlKey]urlCooldownState),
		slowIsolations:    make(map[urlKey]time.Time),
		requests:          make(map[urlKey]*urlRequestCount),
		affinities:        make(map[modelAffinityKey]*affinityEntry),
		noThinkingBlklist: make(map[urlModelKey]time.Time),
		probing:           make(map[urlKey]time.Time),
		alpha:             0.3,
		cooldownBase:      2 * time.Minute,
		cooldownMax:       48 * time.Hour, // 死URL最长冷却48小时
		probeTimeout:      defaultURLSelectorProbeTimeout,
		probeDial:         (&net.Dialer{}).DialContext,
		cleanupInterval:   defaultURLSelectorCleanupInterval,
		latencyMaxAge:     defaultURLSelectorLatencyMaxAge,
		nextCleanup:       now.Add(defaultURLSelectorCleanupInterval),
	}
}

func (s *URLSelector) gcLocked(now time.Time, maxAge time.Duration) {
	if maxAge <= 0 {
		maxAge = s.latencyMaxAge
	}
	if maxAge > 0 {
		cutoff := now.Add(-maxAge)
		for key, ewma := range s.latencies {
			if ewma == nil || ewma.lastSeen.IsZero() || ewma.lastSeen.Before(cutoff) {
				delete(s.latencies, key)
				if _, ok := s.probeLatencies[key]; !ok {
					delete(s.requests, key)
				}
			}
		}
		for key, ewma := range s.probeLatencies {
			if ewma == nil || ewma.lastSeen.IsZero() || ewma.lastSeen.Before(cutoff) {
				delete(s.probeLatencies, key)
				if _, ok := s.latencies[key]; !ok {
					delete(s.requests, key)
				}
			}
		}
	}

	for key, cooldown := range s.cooldowns {
		if !now.Before(cooldown.until) {
			delete(s.cooldowns, key)
		}
	}
	for key, until := range s.slowIsolations {
		if !now.Before(until) {
			delete(s.slowIsolations, key)
		}
	}

	// probing 条目正常生命周期极短（<= probeTimeout）。
	// 若因 goroutine 异常未清理而滞留，这里兜底回收，避免该 URL 永远无法被再次探测。
	probeCutoff := now.Add(-2 * s.probeTimeout)
	for key, started := range s.probing {
		if started.Before(probeCutoff) {
			delete(s.probing, key)
		}
	}

	// 清理过期的模型亲和性（超过24小时没用的就扔了）
	affinityCutoff := now.Add(-24 * time.Hour)
	for key, aff := range s.affinities {
		if aff.lastUsed.Before(affinityCutoff) {
			delete(s.affinities, key)
		}
	}

	// 清理过期的 thinking 黑名单
	for key, expiry := range s.noThinkingBlklist {
		if now.After(expiry) {
			delete(s.noThinkingBlklist, key)
		}
	}
}

func (s *URLSelector) maybeCleanupLocked(now time.Time) {
	if s.cleanupInterval <= 0 {
		return
	}
	if !s.nextCleanup.IsZero() && now.Before(s.nextCleanup) {
		return
	}
	s.gcLocked(now, s.latencyMaxAge)
	s.nextCleanup = now.Add(s.cleanupInterval)
}

// GC 手动触发状态清理（用于测试或运维兜底）。
// maxAge 控制 latency 条目的保留时长，cooldown 条目始终按 until 过期清理。
func (s *URLSelector) GC(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now(), maxAge)
}

// PruneChannel 清理指定渠道中不再存在的 URL 状态。
// keepURLs 为空时会移除该渠道全部状态。
func (s *URLSelector) PruneChannel(channelID int64, keepURLs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	keep := make(map[string]struct{}, len(keepURLs))
	for _, u := range keepURLs {
		keep[u] = struct{}{}
	}

	for key := range s.latencies {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.latencies, key)
		}
	}
	for key := range s.probeLatencies {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.probeLatencies, key)
		}
	}
	for key := range s.cooldowns {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.cooldowns, key)
		}
	}
	for key := range s.slowIsolations {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.slowIsolations, key)
		}
	}
	for key := range s.requests {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			// 这个 URL 的延迟状态已经没了，请求统计也一起清掉，别留孤儿数据。
			delete(s.requests, key)
		}
	}
}

// RemoveChannel 移除指定渠道的全部 URL 状态（含亲和性）。
func (s *URLSelector) RemoveChannel(channelID int64) {
	s.PruneChannel(channelID, nil)

	// 清理该渠道的所有亲和性和 thinking 黑名单
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.affinities {
		if key.channelID == channelID {
			delete(s.affinities, key)
		}
	}
	for key := range s.noThinkingBlklist {
		if key.channelID == channelID {
			delete(s.noThinkingBlklist, key)
		}
	}
}

// RecordLatency 记录URL的首字节时间，更新EWMA
func (s *URLSelector) RecordLatency(channelID int64, rawURL string, ttfb time.Duration) {
	key := urlKey{channelID: channelID, url: rawURL}
	ms := normalizeLatencyMS(ttfb)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	s.upsertLatencyLocked(s.latencies, key, ms, now)
	s.recordSuccessLocked(key)
	s.applySlowTTFBIsolationLocked(key, ttfb, now)
}

// MarkURLSuccess 只标记这个 URL 已经恢复可用，不写真实 TTFB。
func (s *URLSelector) MarkURLSuccess(channelID int64, rawURL string) {
	key := urlKey{channelID: channelID, url: rawURL}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)
	s.recordSuccessLocked(key)
}

func (s *URLSelector) recordSuccessLocked(key urlKey) {
	// 成功请求：清掉冷却，让 URL 立刻恢复可用。
	delete(s.cooldowns, key)

	// 真成功之后把成功计数补上，同时把连续 model not found 清零。
	if rc := s.requests[key]; rc != nil {
		rc.success++
		rc.consecutiveModelNotFound = 0
	} else {
		s.requests[key] = &urlRequestCount{success: 1}
	}
}

func (s *URLSelector) applySlowTTFBIsolationLocked(key urlKey, ttfb time.Duration, now time.Time) {
	duration := s.slowTTFBIsolationDuration(ttfb)
	if duration <= 0 {
		delete(s.slowIsolations, key)
		return
	}
	s.slowIsolations[key] = now.Add(duration)
}

func (s *URLSelector) slowTTFBIsolationDuration(ttfb time.Duration) time.Duration {
	if ttfb < slowTTFBIsolationThreshold {
		return 0
	}
	base := s.cooldownBase
	if base <= 0 {
		base = 2 * time.Minute
	}
	if ttfb >= slowTTFBSevereThreshold {
		return base * 2
	}
	return base
}

// RecordProbeLatency 记录探测出来的 RTT 种子。
// 这里只更新 probe 数据，不算真实成功，也不清冷却。
func (s *URLSelector) RecordProbeLatency(channelID int64, rawURL string, latency time.Duration) {
	key := urlKey{channelID: channelID, url: rawURL}
	ms := normalizeLatencyMS(latency)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)
	s.upsertLatencyLocked(s.probeLatencies, key, ms, now)
}

// RecordModelNotFound 记录一次"URL没有这个模型"
// 连续达到阈值后触发冷却（说明这个URL对所有模型都不行）
// 返回 true 表示达到阈值、已触发冷却
func (s *URLSelector) RecordModelNotFound(channelID int64, rawURL string, threshold int) bool {
	key := urlKey{channelID: channelID, url: rawURL}

	s.mu.Lock()
	defer s.mu.Unlock()

	rc := s.requests[key]
	if rc == nil {
		rc = &urlRequestCount{}
		s.requests[key] = rc
	}
	rc.consecutiveModelNotFound++

	if rc.consecutiveModelNotFound >= threshold {
		// 达到阈值，这个URL对啥模型都不行，冷却它
		rc.consecutiveModelNotFound = 0 // 冷却后重置，解冻后再给机会
		// 直接操作 cooldowns map（已持有写锁）
		now := time.Now()
		cd := s.cooldowns[key]
		cd.consecutiveFails++
		multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
		duration := time.Duration(float64(s.cooldownBase) * multiplier)
		if duration > s.cooldownMax {
			duration = s.cooldownMax
		}
		cd.until = now.Add(duration)
		s.cooldowns[key] = cd
		rc.failure++
		return true
	}
	return false
}

// SetModelAffinity 记录模型亲和性：该模型在这个渠道上次用哪个URL成功了
func (s *URLSelector) SetModelAffinity(channelID int64, model, rawURL string) {
	if model == "" || rawURL == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.affinities[ak] = &affinityEntry{
		url:      rawURL,
		lastUsed: time.Now(),
	}
}

// ClearModelAffinity 清除模型亲和性（URL失败时调用）
// 只有当前亲和URL和失败URL一致时才清除，防止误清
func (s *URLSelector) ClearModelAffinity(channelID int64, model, failedURL string) {
	if model == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	s.mu.Lock()
	defer s.mu.Unlock()

	if aff, ok := s.affinities[ak]; ok && aff.url == failedURL {
		delete(s.affinities, ak)
	}
}

// GetModelAffinity 查询模型亲和性URL（用于外部排序）
func (s *URLSelector) GetModelAffinity(channelID int64, model string) (string, bool) {
	if model == "" {
		return "", false
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	s.mu.RLock()
	defer s.mu.RUnlock()

	aff, ok := s.affinities[ak]
	if !ok {
		return "", false
	}
	return aff.url, true
}

// MarkNoThinking 标记某URL对某模型不提供thinking，加入黑名单（一周过期）
// 同时清除该模型的亲和性，防止亲和性指向黑名单URL
func (s *URLSelector) MarkNoThinking(channelID int64, rawURL, model string) {
	key := urlModelKey{channelID: channelID, url: rawURL, model: model}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.noThinkingBlklist[key] = time.Now().Add(7 * 24 * time.Hour)

	// 清除指向这个URL的亲和性
	ak := modelAffinityKey{channelID: channelID, model: model}
	if aff, ok := s.affinities[ak]; ok && aff.url == rawURL {
		delete(s.affinities, ak)
	}
}

// IsNoThinking 检查某URL对某模型是否在 thinking 黑名单中
func (s *URLSelector) IsNoThinking(channelID int64, rawURL, model string) bool {
	key := urlModelKey{channelID: channelID, url: rawURL, model: model}

	s.mu.RLock()
	defer s.mu.RUnlock()

	expiry, ok := s.noThinkingBlklist[key]
	return ok && time.Now().Before(expiry)
}

// NoThinkingEntry 黑名单条目（给前端/API用）
type NoThinkingEntry struct {
	URL       string `json:"url"`
	Model     string `json:"model"`
	ExpiresAt string `json:"expires_at"` // RFC3339
}

// GetNoThinkingList 获取指定渠道的 thinking 黑名单列表
func (s *URLSelector) GetNoThinkingList(channelID int64) []NoThinkingEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var list []NoThinkingEntry
	for key, expiry := range s.noThinkingBlklist {
		if key.channelID == channelID && now.Before(expiry) {
			list = append(list, NoThinkingEntry{
				URL:       key.url,
				Model:     key.model,
				ExpiresAt: expiry.Format(time.RFC3339),
			})
		}
	}
	return list
}

// ClearNoThinking 清除指定渠道的 thinking 黑名单（全部或指定 URL+model）
func (s *URLSelector) ClearNoThinking(channelID int64, rawURL, model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cleared := 0
	for key := range s.noThinkingBlklist {
		if key.channelID != channelID {
			continue
		}
		// 指定了 URL 或 model 就精确匹配，没指定就全清
		if rawURL != "" && key.url != rawURL {
			continue
		}
		if model != "" && key.model != model {
			continue
		}
		delete(s.noThinkingBlklist, key)
		cleared++
	}
	return cleared
}

// CooldownURL 对URL施加指数退避冷却
func (s *URLSelector) CooldownURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	cd := s.cooldowns[key]
	cd.consecutiveFails++

	// 指数退避: base * 2^(fails-1), 上限 max
	multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
	duration := time.Duration(float64(s.cooldownBase) * multiplier)
	if duration > s.cooldownMax {
		duration = s.cooldownMax
	}

	cd.until = now.Add(duration)
	s.cooldowns[key] = cd

	// 递增失败计数
	if rc := s.requests[key]; rc != nil {
		rc.failure++
	} else {
		s.requests[key] = &urlRequestCount{failure: 1}
	}
}

// IsCooledDown 检查URL是否在冷却中
func (s *URLSelector) IsCooledDown(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	cd, ok := s.cooldowns[key]
	if ok && now.Before(cd.until) {
		return true
	}
	until, ok := s.slowIsolations[key]
	return ok && now.Before(until)
}

// ClearChannelCooldowns 清除指定渠道所有URL的冷却状态和失败计数
func (s *URLSelector) ClearChannelCooldowns(channelID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cleared := 0
	for key := range s.cooldowns {
		if key.channelID == channelID {
			delete(s.cooldowns, key)
			cleared++
		}
	}
	for key := range s.slowIsolations {
		if key.channelID == channelID {
			delete(s.slowIsolations, key)
			cleared++
		}
	}
	// 同时重置失败计数，给这些URL一个干净的起点
	for key, rc := range s.requests {
		if key.channelID == channelID && rc != nil {
			rc.failure = 0
		}
	}
	return cleared
}

// URLStat 单个URL的运行时状态快照
type URLStat struct {
	URL                string  `json:"url"`
	LatencyMs          float64 `json:"latency_ms"`           // 兼容老前端：这里返回选择器实际使用的有效延迟
	TTFBLatencyMs      float64 `json:"ttfb_latency_ms"`      // 真实请求的 TTFB EWMA，-1 表示无数据
	ProbeLatencyMs     float64 `json:"probe_latency_ms"`     // 探测 RTT EWMA，-1 表示无数据
	EffectiveLatencyMs float64 `json:"effective_latency_ms"` // 应用惩罚后的最终评分延迟
	LatencySource      string  `json:"latency_source"`       // ttfb / probe / unknown
	CooledDown         bool    `json:"cooled_down"`
	CooldownRemainMs   int64   `json:"cooldown_remain_ms"`
	SlowIsolated       bool    `json:"slow_isolated"`
	SlowIsolationMs    int64   `json:"slow_isolation_ms"`
	Requests           int64   `json:"requests"`
	Failures           int64   `json:"failures"`
	Weight             float64 `json:"weight,omitempty"` // 动态选择权重，反映该URL被选中的相对概率
}

// GetURLStats 返回指定渠道各URL的运行时状态（延迟、冷却）
func (s *URLSelector) GetURLStats(channelID int64, urls []string) []URLStat {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make([]URLStat, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		st := URLStat{URL: u, LatencyMs: -1, TTFBLatencyMs: -1, ProbeLatencyMs: -1, EffectiveLatencyMs: -1, LatencySource: latencySourceUnknown}

		if e, ok := s.latencies[key]; ok {
			st.TTFBLatencyMs = e.value
		}
		if e, ok := s.probeLatencies[key]; ok {
			st.ProbeLatencyMs = e.value
		}
		if latency, source, known := s.effectiveLatencyLocked(key); known {
			st.LatencyMs = latency
			st.EffectiveLatencyMs = latency
			st.LatencySource = source
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			st.CooledDown = true
			st.CooldownRemainMs = cd.until.Sub(now).Milliseconds()
		}
		if until, ok := s.slowIsolations[key]; ok && now.Before(until) {
			st.SlowIsolated = true
			st.SlowIsolationMs = until.Sub(now).Milliseconds()
			if !st.CooledDown {
				st.CooledDown = true
				st.CooldownRemainMs = st.SlowIsolationMs
			}
		}
		if rc, ok := s.requests[key]; ok {
			st.Requests = rc.success
			st.Failures = rc.failure
		}

		// 计算动态权重：(1/有效延迟) * 成功率，跟 candidateScore 逻辑一致
		effLat := st.EffectiveLatencyMs
		if effLat <= 0 {
			effLat = defaultEffectiveLatencyMS
		}
		successRate := 1.0
		if total := st.Requests + st.Failures; total > 0 {
			successRate = float64(st.Requests) / float64(total)
		}
		st.Weight = (1.0 / effLat) * successRate

		stats[i] = st
	}
	return stats
}
