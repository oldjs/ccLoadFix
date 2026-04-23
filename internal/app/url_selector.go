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
	defaultURLSelectorProbeTimeout    = 3 * time.Second
	slowTTFBIsolationThreshold        = 2500 * time.Millisecond
	slowTTFBSevereThreshold           = 6 * time.Second

	// urlShardCount 按 channelID 分片数量，同一渠道的操作锁同一片
	urlShardCount = 16
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

// urlShard 按 channelID 分片的状态单元
// 同一渠道的所有URL状态在同一个分片里，不同渠道互不竞争
type urlShard struct {
	mu                sync.RWMutex
	latencies         map[urlKey]*ewmaValue               // 真实请求的 TTFB EWMA
	probeLatencies    map[urlKey]*ewmaValue               // 探测出来的 RTT 种子
	cooldowns         map[urlKey]urlCooldownState         // URL冷却
	slowIsolations    map[urlKey]time.Time                // 慢TTFB隔离
	requests          map[urlKey]*urlRequestCount         // 调用计数
	affinities        map[modelAffinityKey]*affinityEntry // 模型亲和性
	warms             map[modelAffinityKey]*warmEntry     // 模型 warm 备选（主亲和失效时的首跳候选）
	noThinkingBlklist map[urlModelKey]time.Time           // thinking黑名单
	probing           map[urlKey]time.Time                // 正在探测的URL
	nextCleanup       time.Time                           // 本分片下次清理时间
}

// URLSelector 基于EWMA延迟、成功率和模型亲和性选择最优URL
// 按 channelID 分片：不同渠道的操作完全无锁竞争
type URLSelector struct {
	shards [urlShardCount]urlShard

	// 配置（只读，初始化后不变）
	alpha           float64       // EWMA权重因子
	cooldownBase    time.Duration // 基础冷却时间
	cooldownMax     time.Duration // 最大冷却时间
	probeTimeout    time.Duration
	probeDial       func(ctx context.Context, network, address string) (net.Conn, error)
	cleanupInterval time.Duration
	latencyMaxAge   time.Duration
}

func normalizeLatencyMS(ttfb time.Duration) float64 {
	ms := float64(ttfb) / float64(time.Millisecond)
	if ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return 0.1
	}
	return ms
}

// getShard 按 channelID 定位分片
func (s *URLSelector) getShard(channelID int64) *urlShard {
	return &s.shards[uint64(channelID)%urlShardCount]
}

// upsertLatency 更新延迟EWMA（调用方已持有写锁）
func (s *URLSelector) upsertLatency(latencyMap map[urlKey]*ewmaValue, key urlKey, ms float64, now time.Time) {
	if e, ok := latencyMap[key]; ok {
		e.value = s.alpha*ms + (1-s.alpha)*e.value
		e.lastSeen = now
		return
	}
	latencyMap[key] = &ewmaValue{value: ms, lastSeen: now}
}

func initShard(sh *urlShard) {
	sh.latencies = make(map[urlKey]*ewmaValue)
	sh.probeLatencies = make(map[urlKey]*ewmaValue)
	sh.cooldowns = make(map[urlKey]urlCooldownState)
	sh.slowIsolations = make(map[urlKey]time.Time)
	sh.requests = make(map[urlKey]*urlRequestCount)
	sh.affinities = make(map[modelAffinityKey]*affinityEntry)
	sh.warms = make(map[modelAffinityKey]*warmEntry)
	sh.noThinkingBlklist = make(map[urlModelKey]time.Time)
	sh.probing = make(map[urlKey]time.Time)
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	now := time.Now()
	sel := &URLSelector{
		alpha:           0.3,
		cooldownBase:    45 * time.Second,
		cooldownMax:     4 * time.Hour, // 死URL最长冷却4小时
		probeTimeout:    defaultURLSelectorProbeTimeout,
		probeDial:       (&net.Dialer{}).DialContext,
		cleanupInterval: defaultURLSelectorCleanupInterval,
		latencyMaxAge:   defaultURLSelectorLatencyMaxAge,
	}
	for i := range sel.shards {
		initShard(&sel.shards[i])
		sel.shards[i].nextCleanup = now.Add(defaultURLSelectorCleanupInterval)
	}
	return sel
}

// gcShard 清理单个分片的过期数据（调用方已持有写锁）
func (s *URLSelector) gcShard(sh *urlShard, now time.Time, maxAge time.Duration) {
	if maxAge <= 0 {
		maxAge = s.latencyMaxAge
	}
	if maxAge > 0 {
		cutoff := now.Add(-maxAge)
		for key, ewma := range sh.latencies {
			if ewma == nil || ewma.lastSeen.IsZero() || ewma.lastSeen.Before(cutoff) {
				delete(sh.latencies, key)
			}
		}
		for key, ewma := range sh.probeLatencies {
			if ewma == nil || ewma.lastSeen.IsZero() || ewma.lastSeen.Before(cutoff) {
				delete(sh.probeLatencies, key)
			}
		}
	}

	for key, cooldown := range sh.cooldowns {
		if !now.Before(cooldown.until) {
			delete(sh.cooldowns, key)
		}
	}
	for key, until := range sh.slowIsolations {
		if !now.Before(until) {
			delete(sh.slowIsolations, key)
		}
	}

	// probing 条目正常生命周期极短（<= probeTimeout）。
	// 若因 goroutine 异常未清理而滞留，这里兜底回收。
	probeCutoff := now.Add(-2 * s.probeTimeout)
	for key, started := range sh.probing {
		if started.Before(probeCutoff) {
			delete(sh.probing, key)
		}
	}

	// 清理过期的模型亲和性（超过24小时没用的就扔了）
	affinityCutoff := now.Add(-24 * time.Hour)
	for key, aff := range sh.affinities {
		if aff.lastUsed.Before(affinityCutoff) {
			delete(sh.affinities, key)
		}
	}

	// 清理过期的 thinking 黑名单
	for key, expiry := range sh.noThinkingBlklist {
		if now.After(expiry) {
			delete(sh.noThinkingBlklist, key)
		}
	}

	// 清理全部过期的 warm 备选（所有 slot 都超 TTL）
	gcWarms(sh, now)
}

// maybeCleanupShard 检查分片是否需要清理（调用方已持有写锁）
func (s *URLSelector) maybeCleanupShard(sh *urlShard, now time.Time) {
	if s.cleanupInterval <= 0 {
		return
	}
	if !sh.nextCleanup.IsZero() && now.Before(sh.nextCleanup) {
		return
	}
	s.gcShard(sh, now, s.latencyMaxAge)
	sh.nextCleanup = now.Add(s.cleanupInterval)
}

// GC 手动触发全部分片的状态清理
func (s *URLSelector) GC(maxAge time.Duration) {
	now := time.Now()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		s.gcShard(sh, now, maxAge)
		sh.mu.Unlock()
	}
}

// PruneChannel 清理指定渠道中不再存在的 URL 状态。
// keepURLs 为空时会移除该渠道全部状态。
func (s *URLSelector) PruneChannel(channelID int64, keepURLs []string) {
	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	keep := make(map[string]struct{}, len(keepURLs))
	for _, u := range keepURLs {
		keep[u] = struct{}{}
	}

	for key := range sh.latencies {
		if key.channelID == channelID {
			if _, ok := keep[key.url]; !ok {
				delete(sh.latencies, key)
			}
		}
	}
	for key := range sh.probeLatencies {
		if key.channelID == channelID {
			if _, ok := keep[key.url]; !ok {
				delete(sh.probeLatencies, key)
			}
		}
	}
	for key := range sh.cooldowns {
		if key.channelID == channelID {
			if _, ok := keep[key.url]; !ok {
				delete(sh.cooldowns, key)
			}
		}
	}
	for key := range sh.slowIsolations {
		if key.channelID == channelID {
			if _, ok := keep[key.url]; !ok {
				delete(sh.slowIsolations, key)
			}
		}
	}
	for key := range sh.requests {
		if key.channelID == channelID {
			if _, ok := keep[key.url]; !ok {
				delete(sh.requests, key)
			}
		}
	}

	// warm 备选跟着 URL 变更剔除已删除的 URL
	pruneWarmsForChannel(sh, channelID, keep)
}

// RemoveChannel 移除指定渠道的全部 URL 状态（含亲和性）。
func (s *URLSelector) RemoveChannel(channelID int64) {
	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	// 清理所有 urlKey 维度的状态
	for key := range sh.latencies {
		if key.channelID == channelID {
			delete(sh.latencies, key)
		}
	}
	for key := range sh.probeLatencies {
		if key.channelID == channelID {
			delete(sh.probeLatencies, key)
		}
	}
	for key := range sh.cooldowns {
		if key.channelID == channelID {
			delete(sh.cooldowns, key)
		}
	}
	for key := range sh.slowIsolations {
		if key.channelID == channelID {
			delete(sh.slowIsolations, key)
		}
	}
	for key := range sh.requests {
		if key.channelID == channelID {
			delete(sh.requests, key)
		}
	}

	// 清理该渠道的亲和性和 thinking 黑名单
	for key := range sh.affinities {
		if key.channelID == channelID {
			delete(sh.affinities, key)
		}
	}
	for key := range sh.noThinkingBlklist {
		if key.channelID == channelID {
			delete(sh.noThinkingBlklist, key)
		}
	}

	// 清理 warm 备选
	removeWarmsForChannel(sh, channelID)
}

// recordSuccessInShard 记录成功（调用方已持有写锁）
func recordSuccessInShard(sh *urlShard, key urlKey) {
	// 成功请求：清掉冷却，让 URL 立刻恢复可用
	delete(sh.cooldowns, key)

	if rc := sh.requests[key]; rc != nil {
		rc.success++
		rc.consecutiveModelNotFound = 0
	} else {
		sh.requests[key] = &urlRequestCount{success: 1}
	}
}

// applySlowTTFBInShard 应用慢TTFB隔离（调用方已持有写锁）
func (s *URLSelector) applySlowTTFBInShard(sh *urlShard, key urlKey, ttfb time.Duration, now time.Time) {
	duration := s.slowTTFBIsolationDuration(ttfb)
	if duration <= 0 {
		delete(sh.slowIsolations, key)
		return
	}
	sh.slowIsolations[key] = now.Add(duration)
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

// RecordLatency 记录URL的首字节时间，更新EWMA
func (s *URLSelector) RecordLatency(channelID int64, rawURL string, ttfb time.Duration) {
	key := urlKey{channelID: channelID, url: rawURL}
	ms := normalizeLatencyMS(ttfb)

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	s.maybeCleanupShard(sh, now)

	s.upsertLatency(sh.latencies, key, ms, now)
	recordSuccessInShard(sh, key)
	s.applySlowTTFBInShard(sh, key, ttfb, now)
}

// MarkURLSuccess 只标记这个 URL 已经恢复可用，不写真实 TTFB。
func (s *URLSelector) MarkURLSuccess(channelID int64, rawURL string) {
	key := urlKey{channelID: channelID, url: rawURL}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	s.maybeCleanupShard(sh, now)
	recordSuccessInShard(sh, key)
}

// RecordProbeLatency 记录探测出来的 RTT 种子。
func (s *URLSelector) RecordProbeLatency(channelID int64, rawURL string, latency time.Duration) {
	key := urlKey{channelID: channelID, url: rawURL}
	ms := normalizeLatencyMS(latency)

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	s.maybeCleanupShard(sh, now)
	s.upsertLatency(sh.probeLatencies, key, ms, now)
}

// RecordModelNotFound 记录一次"URL没有这个模型"
// 连续达到阈值后触发冷却
func (s *URLSelector) RecordModelNotFound(channelID int64, rawURL string, threshold int) bool {
	key := urlKey{channelID: channelID, url: rawURL}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	rc := sh.requests[key]
	if rc == nil {
		rc = &urlRequestCount{}
		sh.requests[key] = rc
	}
	rc.consecutiveModelNotFound++

	if rc.consecutiveModelNotFound >= threshold {
		rc.consecutiveModelNotFound = 0
		now := time.Now()
		cd := sh.cooldowns[key]
		cd.consecutiveFails++
		multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
		duration := time.Duration(float64(s.cooldownBase) * multiplier)
		if duration > s.cooldownMax {
			duration = s.cooldownMax
		}
		cd.until = now.Add(duration)
		sh.cooldowns[key] = cd
		rc.failure++
		// 累计"没这个模型"达到阈值进冷却，同步剥离 warm
		removeWarmURLInChannel(sh, channelID, rawURL)
		return true
	}
	return false
}

// SetModelAffinity 记录模型亲和性，同一临界区内顺带刷新 warm 备选。
// warm 写路径内嵌进来，省一次锁；low-latency guard 由调用方在 SetModelAffinity
// 之前过滤，warm 自然继承 guard 保护。
func (s *URLSelector) SetModelAffinity(channelID int64, model, rawURL string) {
	if model == "" || rawURL == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	sh.affinities[ak] = &affinityEntry{
		url:      rawURL,
		lastUsed: now,
	}
	// 刷入 warm list 作为主亲和失效时的首跳候选
	pushWarmInShard(sh, ak, rawURL, now)
}

// ClearModelAffinity 清除模型亲和性（URL失败时调用）
func (s *URLSelector) ClearModelAffinity(channelID int64, model, failedURL string) {
	if model == "" {
		return
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if aff, ok := sh.affinities[ak]; ok && aff.url == failedURL {
		delete(sh.affinities, ak)
	}
}

// GetModelAffinity 查询模型亲和性URL
func (s *URLSelector) GetModelAffinity(channelID int64, model string) (string, bool) {
	if model == "" {
		return "", false
	}
	ak := modelAffinityKey{channelID: channelID, model: model}

	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	aff, ok := sh.affinities[ak]
	if !ok {
		return "", false
	}
	return aff.url, true
}

// MarkNoThinking 标记某URL对某模型不提供thinking，加入黑名单（一周过期）
func (s *URLSelector) MarkNoThinking(channelID int64, rawURL, model string) {
	key := urlModelKey{channelID: channelID, url: rawURL, model: model}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sh.noThinkingBlklist[key] = time.Now().Add(7 * 24 * time.Hour)

	// 清除指向这个URL的亲和性
	ak := modelAffinityKey{channelID: channelID, model: model}
	if aff, ok := sh.affinities[ak]; ok && aff.url == rawURL {
		delete(sh.affinities, ak)
	}
	// 黑名单是 (URL, model) 粒度，只清这一对的 warm，别误伤其他 model
	removeWarmURLForModelOnly(sh, ak, rawURL)
}

// IsNoThinking 检查某URL对某模型是否在 thinking 黑名单中
func (s *URLSelector) IsNoThinking(channelID int64, rawURL, model string) bool {
	key := urlModelKey{channelID: channelID, url: rawURL, model: model}

	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	expiry, ok := sh.noThinkingBlklist[key]
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
	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	now := time.Now()
	var list []NoThinkingEntry
	for key, expiry := range sh.noThinkingBlklist {
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

// ClearNoThinking 清除指定渠道的 thinking 黑名单
func (s *URLSelector) ClearNoThinking(channelID int64, rawURL, model string) int {
	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cleared := 0
	for key := range sh.noThinkingBlklist {
		if key.channelID != channelID {
			continue
		}
		if rawURL != "" && key.url != rawURL {
			continue
		}
		if model != "" && key.model != model {
			continue
		}
		delete(sh.noThinkingBlklist, key)
		cleared++
	}
	return cleared
}

// SuspectLowLatencyCooldown 对可疑低延迟URL施加固定时长冷却
// 与 CooldownURL 的区别：不累加 consecutiveFails、不计入 failure
// 用途：流式请求首字节异常快时隔离URL，避免诱导渠道污染亲和与选路权重
func (s *URLSelector) SuspectLowLatencyCooldown(channelID int64, rawURL string, duration time.Duration) {
	if duration <= 0 {
		return
	}
	key := urlKey{channelID: channelID, url: rawURL}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	s.maybeCleanupShard(sh, now)

	// 固定时长，不累加失败计数；若已有更长冷却则保留较长的
	cd := sh.cooldowns[key]
	until := now.Add(duration)
	if until.After(cd.until) {
		cd.until = until
	}
	sh.cooldowns[key] = cd

	// URL 被低延迟守卫隔离了，同步从 warm 里摘出去
	removeWarmURLInChannel(sh, channelID, rawURL)
}

// CooldownURL 对URL施加指数退避冷却
func (s *URLSelector) CooldownURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}

	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now()
	s.maybeCleanupShard(sh, now)

	cd := sh.cooldowns[key]
	cd.consecutiveFails++

	// 指数退避: base * 2^(fails-1), 上限 max
	multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
	duration := time.Duration(float64(s.cooldownBase) * multiplier)
	if duration > s.cooldownMax {
		duration = s.cooldownMax
	}

	cd.until = now.Add(duration)
	sh.cooldowns[key] = cd

	// 递增失败计数
	if rc := sh.requests[key]; rc != nil {
		rc.failure++
	} else {
		sh.requests[key] = &urlRequestCount{failure: 1}
	}

	// URL 进冷却，从全部 model 的 warm 里剥掉，避免下次又被选为首跳
	removeWarmURLInChannel(sh, channelID, url)
}

// IsCooledDown 检查URL是否在冷却中
func (s *URLSelector) IsCooledDown(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	now := time.Now()
	cd, ok := sh.cooldowns[key]
	if ok && now.Before(cd.until) {
		return true
	}
	until, ok := sh.slowIsolations[key]
	return ok && now.Before(until)
}

// ClearChannelCooldowns 清除指定渠道所有URL的冷却状态和失败计数
func (s *URLSelector) ClearChannelCooldowns(channelID int64) int {
	sh := s.getShard(channelID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cleared := 0
	for key := range sh.cooldowns {
		if key.channelID == channelID {
			delete(sh.cooldowns, key)
			cleared++
		}
	}
	for key := range sh.slowIsolations {
		if key.channelID == channelID {
			delete(sh.slowIsolations, key)
			cleared++
		}
	}
	// 重置失败计数
	for key, rc := range sh.requests {
		if key.channelID == channelID && rc != nil {
			rc.failure = 0
		}
	}
	return cleared
}

// URLStat 单个URL的运行时状态快照
type URLStat struct {
	URL                string  `json:"url"`
	LatencyMs          float64 `json:"latency_ms"`
	TTFBLatencyMs      float64 `json:"ttfb_latency_ms"`
	ProbeLatencyMs     float64 `json:"probe_latency_ms"`
	EffectiveLatencyMs float64 `json:"effective_latency_ms"`
	LatencySource      string  `json:"latency_source"`
	CooledDown         bool    `json:"cooled_down"`
	CooldownRemainMs   int64   `json:"cooldown_remain_ms"`
	SlowIsolated       bool    `json:"slow_isolated"`
	SlowIsolationMs    int64   `json:"slow_isolation_ms"`
	Requests           int64   `json:"requests"`
	Failures           int64   `json:"failures"`
	Weight             float64 `json:"weight"`
}

// GetURLStats 返回指定渠道各URL的运行时状态
func (s *URLSelector) GetURLStats(channelID int64, urls []string) []URLStat {
	now := time.Now()
	sh := s.getShard(channelID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	stats := make([]URLStat, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		st := URLStat{URL: u, LatencyMs: -1, TTFBLatencyMs: -1, ProbeLatencyMs: -1, EffectiveLatencyMs: -1, LatencySource: latencySourceUnknown}

		if e, ok := sh.latencies[key]; ok {
			st.TTFBLatencyMs = e.value
		}
		if e, ok := sh.probeLatencies[key]; ok {
			st.ProbeLatencyMs = e.value
		}
		if latency, source, known := effectiveLatencyInShard(sh, key); known {
			st.LatencyMs = latency
			st.EffectiveLatencyMs = latency
			st.LatencySource = source
		}
		if cd, ok := sh.cooldowns[key]; ok && now.Before(cd.until) {
			st.CooledDown = true
			st.CooldownRemainMs = cd.until.Sub(now).Milliseconds()
		}
		if until, ok := sh.slowIsolations[key]; ok && now.Before(until) {
			st.SlowIsolated = true
			st.SlowIsolationMs = until.Sub(now).Milliseconds()
			if !st.CooledDown {
				st.CooledDown = true
				st.CooldownRemainMs = st.SlowIsolationMs
			}
		}
		if rc, ok := sh.requests[key]; ok {
			st.Requests = rc.success
			st.Failures = rc.failure
		}

		// 计算动态权重
		effLat := st.EffectiveLatencyMs
		if effLat <= 0 {
			effLat = defaultEffectiveLatencyMS
		}
		successRate := 1.0
		if total := st.Requests + st.Failures; total > 0 {
			successRate = float64(st.Requests) / float64(total)
		}
		if successRate < 0.05 {
			successRate = 0.05
		}
		st.Weight = (1.0 / effLat) * successRate

		stats[i] = st
	}
	return stats
}
