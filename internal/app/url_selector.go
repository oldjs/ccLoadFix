package app

import (
	"context"
	"errors"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"slices"
	"sync"
	"time"
)

const (
	defaultURLSelectorCleanupInterval = time.Hour
	defaultURLSelectorLatencyMaxAge   = 24 * time.Hour
	defaultURLSelectorProbeTimeout    = 5 * time.Second
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
	mu           sync.RWMutex
	latencies    map[urlKey]*ewmaValue
	cooldowns    map[urlKey]urlCooldownState
	requests     map[urlKey]*urlRequestCount
	affinities        map[modelAffinityKey]*affinityEntry // 模型亲和性：上次成功的URL
	noThinkingBlklist map[urlModelKey]time.Time          // thinking黑名单：(URL,模型)→过期时间
	probing           map[urlKey]time.Time
	alpha        float64       // EWMA权重因子
	cooldownBase time.Duration // 基础冷却时间
	cooldownMax  time.Duration // 最大冷却时间
	probeTimeout time.Duration
	probeDial    func(ctx context.Context, network, address string) (net.Conn, error)
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

func (s *URLSelector) upsertLatencyLocked(key urlKey, ms float64, now time.Time) {
	if e, ok := s.latencies[key]; ok {
		e.value = s.alpha*ms + (1-s.alpha)*e.value
		e.lastSeen = now
		return
	}
	s.latencies[key] = &ewmaValue{value: ms, lastSeen: now}
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	now := time.Now()
	return &URLSelector{
		latencies:       make(map[urlKey]*ewmaValue),
		cooldowns:       make(map[urlKey]urlCooldownState),
		requests:        make(map[urlKey]*urlRequestCount),
		affinities:        make(map[modelAffinityKey]*affinityEntry),
		noThinkingBlklist: make(map[urlModelKey]time.Time),
		probing:         make(map[urlKey]time.Time),
		alpha:           0.3,
		cooldownBase:    2 * time.Minute,
		cooldownMax:     48 * time.Hour, // 死URL最长冷却48小时
		probeTimeout:    defaultURLSelectorProbeTimeout,
		probeDial:       (&net.Dialer{}).DialContext,
		cleanupInterval: defaultURLSelectorCleanupInterval,
		latencyMaxAge:   defaultURLSelectorLatencyMaxAge,
		nextCleanup:     now.Add(defaultURLSelectorCleanupInterval),
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
				delete(s.requests, key)
			}
		}
	}

	for key, cooldown := range s.cooldowns {
		if !now.Before(cooldown.until) {
			delete(s.cooldowns, key)
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
	for key := range s.cooldowns {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.cooldowns, key)
		}
	}
	for key := range s.requests {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.requests, key)
		}
	}
}

// RemoveChannel 移除指定渠道的全部 URL 状态（含亲和性）。
func (s *URLSelector) RemoveChannel(channelID int64) {
	s.PruneChannel(channelID, nil)

	// 清理该渠道的所有亲和性
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.affinities {
		if key.channelID == channelID {
			delete(s.affinities, key)
		}
	}
}

// SelectURL 从候选URL中选择最优的（不带模型亲和性）
// 返回选中的URL和在原列表中的索引
func (s *URLSelector) SelectURL(channelID int64, urls []string) (string, int) {
	return s.SelectURLForModel(channelID, "", urls)
}

// urlSuccessRateLocked 计算URL的成功率（需持有读锁）
// 无请求记录返回1.0（乐观假设），避免惩罚新URL
func (s *URLSelector) urlSuccessRateLocked(key urlKey) float64 {
	rc, ok := s.requests[key]
	if !ok || (rc.success+rc.failure) == 0 {
		return 1.0 // 没数据就乐观点
	}
	return float64(rc.success) / float64(rc.success+rc.failure)
}

// SelectURLForModel 从候选URL中选择最优的（带模型亲和性）
// model为空时退化为无亲和性选择
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

	type candidate struct {
		url         string
		idx         int
		latency     float64 // -1 表示无数据
		successRate float64 // 成功率 0-1
		cooled      bool
	}

	// 构建URL索引，方便亲和性查找
	urlIndex := make(map[string]int, len(urls))
	candidates := make([]candidate, len(urls))
	for i, u := range urls {
		urlIndex[u] = i
		key := urlKey{channelID: channelID, url: u}
		c := candidate{url: u, idx: i, latency: -1, successRate: 1.0}

		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		// thinking 黑名单：该URL对该模型不提供thinking，视为不可用
		if model != "" {
			umk := urlModelKey{channelID: channelID, url: u, model: model}
			if expiry, ok := s.noThinkingBlklist[umk]; ok && now.Before(expiry) {
				c.cooled = true
			}
		}
		c.successRate = s.urlSuccessRateLocked(key)
		candidates[i] = c
	}

	// 亲和性优先：上次成功的URL还没冷却就直接用
	if model != "" {
		ak := modelAffinityKey{channelID: channelID, model: model}
		if aff, ok := s.affinities[ak]; ok {
			if idx, found := urlIndex[aff.url]; found {
				c := candidates[idx]
				if !c.cooled {
					return c.url, c.idx
				}
			}
		}
	}

	// 分离可用和冷却中的候选
	var available, cooled []candidate
	for _, c := range candidates {
		if c.cooled {
			cooled = append(cooled, c)
		} else {
			available = append(available, c)
		}
	}

	// 全冷却兜底
	if len(available) == 0 {
		available = cooled
	}

	// 分已知和未知
	var unknown, known []candidate
	for _, c := range available {
		if c.latency < 0 {
			unknown = append(unknown, c)
		} else {
			known = append(known, c)
		}
	}

	// 只有在没有任何已知好URL时才探索未知URL
	// 否则优先用已知的（避免有好URL不用，跑去试未知的）
	hasGoodKnown := false
	for _, c := range known {
		if c.successRate > 0.5 {
			hasGoodKnown = true
			break
		}
	}
	if len(unknown) > 0 && !hasGoodKnown {
		pick := unknown[rand.IntN(len(unknown))]
		return pick.url, pick.idx
	}

	// 有好URL时只用已知的；没好URL时全放一起碰运气
	pool := known
	if !hasGoodKnown && len(unknown) > 0 {
		pool = append(pool, unknown...)
	}
	if len(pool) == 0 {
		// 啥都没有但有未知URL，随机选一个探探
		if len(unknown) > 0 {
			pick := unknown[rand.IntN(len(unknown))]
			return pick.url, pick.idx
		}
		return urls[0], 0
	}

	// 加权随机: weight = 1/latency，但惩罚两个极端
	// <100ms 可疑（掺水/没思考），>3s 太慢，100-3000ms 是甜区
	totalWeight := 0.0
	weights := make([]float64, len(pool))
	for i, c := range pool {
		latency := c.latency
		if latency <= 0 || math.IsNaN(latency) || math.IsInf(latency, 0) {
			latency = 500 // 未知URL给保守默认值
		}
		// 可疑低延迟：<100ms 按500ms算，不给额外优势
		if latency < 100 {
			latency = 500
		}
		// 超慢：>3000ms 额外惩罚3倍，进一步压低权重
		if latency > 3000 {
			latency *= 3
		}
		weights[i] = 1.0 / latency
		totalWeight += weights[i]
	}
	if totalWeight <= 0 || math.IsNaN(totalWeight) || math.IsInf(totalWeight, 0) {
		pick := pool[rand.IntN(len(pool))]
		return pick.url, pick.idx
	}
	r := rand.Float64() * totalWeight
	for i, w := range weights {
		r -= w
		if r <= 0 {
			return pool[i].url, pool[i].idx
		}
	}
	return pool[len(pool)-1].url, pool[len(pool)-1].idx
}

// RecordLatency 记录URL的首字节时间，更新EWMA
func (s *URLSelector) RecordLatency(channelID int64, rawURL string, ttfb time.Duration) {
	key := urlKey{channelID: channelID, url: rawURL}
	ms := normalizeLatencyMS(ttfb)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	s.upsertLatencyLocked(key, ms, now)

	// 成功请求：清除冷却状态，立即恢复可用
	delete(s.cooldowns, key)

	// 成功了：递增成功计数，清零连续 model not found
	if rc := s.requests[key]; rc != nil {
		rc.success++
		rc.consecutiveModelNotFound = 0
	} else {
		s.requests[key] = &urlRequestCount{success: 1}
	}
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
	cd, ok := s.cooldowns[key]
	return ok && time.Now().Before(cd.until)
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
	URL              string  `json:"url"`
	LatencyMs        float64 `json:"latency_ms"`         // EWMA延迟（毫秒），-1表示无数据
	CooledDown       bool    `json:"cooled_down"`        // 是否在冷却中
	CooldownRemainMs int64   `json:"cooldown_remain_ms"` // 剩余冷却时间（毫秒）
	Requests         int64   `json:"requests"`           // 成功调用次数
	Failures         int64   `json:"failures"`           // 失败调用次数
}

// GetURLStats 返回指定渠道各URL的运行时状态（延迟、冷却）
func (s *URLSelector) GetURLStats(channelID int64, urls []string) []URLStat {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make([]URLStat, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		st := URLStat{URL: u, LatencyMs: -1}

		if e, ok := s.latencies[key]; ok {
			st.LatencyMs = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			st.CooledDown = true
			st.CooldownRemainMs = cd.until.Sub(now).Milliseconds()
		}
		if rc, ok := s.requests[key]; ok {
			st.Requests = rc.success
			st.Failures = rc.failure
		}
		stats[i] = st
	}
	return stats
}

// sortedURL 排序后的URL条目
type sortedURL struct {
	url string
	idx int
}

// SortURLs 返回按综合得分排序的全部URL列表（非冷却URL优先，用于故障切换遍历）
// 排序综合考虑：成功率 > 冷却状态 > EWMA延迟
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

	type candidate struct {
		url         string
		idx         int
		latency     float64
		successRate float64
		cooled      bool
	}

	candidates := make([]candidate, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		c := candidate{url: u, idx: i, latency: -1, successRate: 1.0}
		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		c.successRate = s.urlSuccessRateLocked(key)
		candidates[i] = c
	}

	// 先随机打乱（同分时打散）
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	// 排序优先级：非冷却 > 冷却，同组内高成功率优先，同成功率按延迟升序
	slices.SortStableFunc(candidates, func(ci, cj candidate) int {
		// 非冷却优先
		if ci.cooled != cj.cooled {
			if !ci.cooled {
				return -1
			}
			return 1
		}
		// 已知 vs 未知：有延迟数据的优先，未知的排后面
		iKnown, jKnown := ci.latency >= 0, cj.latency >= 0
		if iKnown != jKnown {
			if iKnown {
				return -1 // 有数据的优先（改掉原来的探索优先）
			}
			return 1
		}
		if !iKnown {
			return 0 // 都未探索：保持随机顺序
		}
		// 延迟低的优先
		if ci.latency < cj.latency {
			return -1
		}
		if ci.latency > cj.latency {
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

// extractHostPort 从URL字符串提取 host:port，用于TCP连接测试。
// 如果URL中没有端口，根据scheme自动补全（https→443, http→80）。
func extractHostPort(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return ""
		}
	}
	return net.JoinHostPort(host, port)
}

// ProbeURLs 对无延迟数据的URL做并行TCP连接探测，记录连接耗时作为初始EWMA。
// 设计目标：多URL渠道首次被选中时，避免随机选到网络延迟高的URL。
//
// TCP连接时间反映纯网络延迟（DNS+TCP握手），与模型推理时间无关，
// 因此不会误杀推理模型的长首字节等待。
//
// 探测结果仅作为初始EWMA种子，后续真实请求的TTFB会纳入EWMA并逐步校准。
func (s *URLSelector) ProbeURLs(parentCtx context.Context, channelID int64, urls []string) {
	if len(urls) <= 1 {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	// 原子筛选+占位，避免并发请求重复探测同一URL。
	s.mu.Lock()
	now := time.Now()
	s.maybeCleanupLocked(now)
	unknowns := make([]string, 0, len(urls))
	for _, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		if _, known := s.latencies[key]; known {
			continue
		}
		if _, inFlight := s.probing[key]; inFlight {
			continue
		}
		s.probing[key] = now
		unknowns = append(unknowns, u)
	}
	s.mu.Unlock()

	if len(unknowns) == 0 {
		return // 所有URL已有数据
	}

	probeTimeout := s.probeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultURLSelectorProbeTimeout
	}

	// 并行TCP连接探测（默认总超时5s，可被调用方context更早打断）
	ctx, cancel := context.WithTimeout(parentCtx, probeTimeout)
	defer cancel()

	type probeResult struct {
		url     string
		latency time.Duration
		err     error
	}

	results := make(chan probeResult, len(unknowns))
	pending := make(map[string]struct{}, len(unknowns))
	clearProbing := func(probedURL string) {
		key := urlKey{channelID: channelID, url: probedURL}
		s.mu.Lock()
		delete(s.probing, key)
		s.mu.Unlock()
	}
	for _, u := range unknowns {
		pending[u] = struct{}{}
		go func(rawURL string) {
			host := extractHostPort(rawURL)
			if host == "" {
				results <- probeResult{url: rawURL, err: net.UnknownNetworkError("invalid URL")}
				return
			}

			start := time.Now()
			conn, err := s.probeDial(ctx, "tcp", host)
			if err != nil {
				results <- probeResult{url: rawURL, err: err}
				return
			}
			_ = conn.Close()
			results <- probeResult{url: rawURL, latency: time.Since(start)}
		}(u)
	}

	// 收集结果
	probed := 0
	failed := 0
	handleResult := func(r probeResult) {
		if _, ok := pending[r.url]; !ok {
			return
		}
		delete(pending, r.url)
		defer clearProbing(r.url)
		if r.err != nil {
			// 请求取消/服务关闭导致的探测中断不应污染URL冷却状态。
			if errors.Is(r.err, context.Canceled) {
				return
			}
			s.CooldownURL(channelID, r.url)
			failed++
			return
		}
		latency := r.latency
		if latency <= 0 {
			latency = time.Millisecond
		}
		key := urlKey{channelID: channelID, url: r.url}
		s.mu.Lock()
		now := time.Now()
		s.maybeCleanupLocked(now)
		s.upsertLatencyLocked(key, normalizeLatencyMS(latency), now)
		s.mu.Unlock()
		probed++
	}

	for range len(unknowns) {
		select {
		case r := <-results:
			handleResult(r)
		case <-ctx.Done():
			// 超时/取消：先吸收已完成结果，再把剩余未完成URL标记为冷却，避免继续以unknown优先被选中。
			ctxErr := ctx.Err()
			shouldCooldownPending := errors.Is(ctxErr, context.DeadlineExceeded)
			for {
				select {
				case r := <-results:
					handleResult(r)
				default:
					for pendingURL := range pending {
						clearProbing(pendingURL)
						if shouldCooldownPending {
							s.CooldownURL(channelID, pendingURL)
							failed++
						}
					}
					log.Printf("[PROBE] TCP探测提前结束(%v)，已完成=%d/%d", ctxErr, probed+failed, len(unknowns))
					if probed > 0 || failed > 0 {
						log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
					}
					return
				}
			}
		}
	}

	if probed > 0 || failed > 0 {
		log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
	}
}
