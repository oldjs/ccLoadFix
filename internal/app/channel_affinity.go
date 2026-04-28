package app

import (
	"context"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// ChannelAffinity 渠道级软亲和：per-model 记住上次成功的渠道，下次优先走它
// 设计要点：
//   - 纯内存，不持久化，重启清零
//   - 仅在同优先级桶（effPriorityBucket）内生效，不跨桶提升
//   - TTL 到期自动失效，失败时立即清除
type ChannelAffinity struct {
	mu         sync.RWMutex
	affinities map[string]*channelAffinityEntry // key = model name
}

type channelAffinityEntry struct {
	channelID int64
	updatedAt time.Time
}

// ChannelAffinityStatus admin API 返回的亲和状态
type ChannelAffinityStatus struct {
	Model     string `json:"model"`
	ChannelID int64  `json:"channel_id"`
	AgeMs     int64  `json:"age_ms"`        // 距离建立多久（毫秒）
	TTLMs     int64  `json:"ttl_remain_ms"` // 剩余 TTL（毫秒）
}

// NewChannelAffinity 创建一个新的渠道亲和实例
func NewChannelAffinity() *ChannelAffinity {
	return &ChannelAffinity{
		affinities: make(map[string]*channelAffinityEntry),
	}
}

// Set 成功后记录 model→channel 亲和
func (ca *ChannelAffinity) Set(model string, channelID int64) {
	ca.mu.Lock()
	ca.affinities[model] = &channelAffinityEntry{
		channelID: channelID,
		updatedAt: time.Now(),
	}
	ca.mu.Unlock()
}

// Get 查询某个 model 当前的亲和渠道，返回 channelID 和是否存在
func (ca *ChannelAffinity) Get(model string, ttl time.Duration) (int64, bool) {
	ca.mu.RLock()
	entry, ok := ca.affinities[model]
	ca.mu.RUnlock()

	if !ok {
		return 0, false
	}
	// TTL 过期视为不存在
	if time.Since(entry.updatedAt) > ttl {
		return 0, false
	}
	return entry.channelID, true
}

// Clear 清除指定 model 的亲和（仅当 channelID 匹配时才清）
func (ca *ChannelAffinity) Clear(model string, channelID int64) {
	ca.mu.Lock()
	if entry, ok := ca.affinities[model]; ok && entry.channelID == channelID {
		delete(ca.affinities, model)
	}
	ca.mu.Unlock()
}

// ClearByChannel 清除所有指向该 channel 的亲和（渠道被整体冷却时用）
func (ca *ChannelAffinity) ClearByChannel(channelID int64) {
	ca.mu.Lock()
	for model, entry := range ca.affinities {
		if entry.channelID == channelID {
			delete(ca.affinities, model)
		}
	}
	ca.mu.Unlock()
}

// Cleanup 清理过期条目
func (ca *ChannelAffinity) Cleanup(ttl time.Duration) {
	now := time.Now()
	ca.mu.Lock()
	for model, entry := range ca.affinities {
		if now.Sub(entry.updatedAt) > ttl {
			delete(ca.affinities, model)
		}
	}
	ca.mu.Unlock()
}

// snapshot 把当前未过期的亲和拍成切片（持久化用）
// 按 ttl 过滤，避免把已过期数据写到 DB
func (ca *ChannelAffinity) snapshot(ttl time.Duration) []modelpkg.ChannelAffinityState {
	now := time.Now()
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	out := make([]modelpkg.ChannelAffinityState, 0, len(ca.affinities))
	for model, entry := range ca.affinities {
		if ttl > 0 && now.Sub(entry.updatedAt) > ttl {
			continue
		}
		out = append(out, modelpkg.ChannelAffinityState{
			Model:     model,
			ChannelID: entry.channelID,
			UpdatedAt: entry.updatedAt.Unix(),
		})
	}
	return out
}

// LoadFromStore 启动时一次性恢复
// ttl 为当前配置值；过期的条目跳过不加载
func (ca *ChannelAffinity) LoadFromStore(ctx context.Context, store storage.Store, ttl time.Duration) (int, error) {
	if store == nil {
		return 0, nil
	}
	entries, err := store.ChannelAffinityLoadAll(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()

	ca.mu.Lock()
	defer ca.mu.Unlock()

	loaded := 0
	for _, e := range entries {
		if e.Model == "" || e.ChannelID <= 0 {
			continue
		}
		updated := time.Unix(e.UpdatedAt, 0)
		if ttl > 0 && now.Sub(updated) > ttl {
			continue
		}
		ca.affinities[e.Model] = &channelAffinityEntry{
			channelID: e.ChannelID,
			updatedAt: updated,
		}
		loaded++
	}
	return loaded, nil
}

// ChannelAffinityPersistenceConfig 持久化运行参数
type ChannelAffinityPersistenceConfig struct {
	Store        storage.Store
	Interval     time.Duration
	ShutdownCtx  context.Context
	WaitGroup    *sync.WaitGroup
	FlushTimeout time.Duration
	TTLProvider  func() time.Duration // 动态读取当前 TTL（配置可热改）
}

// StartPersistence 启动周期 flush goroutine
func (ca *ChannelAffinity) StartPersistence(cfg ChannelAffinityPersistenceConfig) {
	if cfg.Store == nil || cfg.Interval <= 0 {
		return
	}
	flushTimeout := cfg.FlushTimeout
	if flushTimeout <= 0 {
		flushTimeout = 10 * time.Second
	}
	ttlFn := cfg.TTLProvider
	if ttlFn == nil {
		ttlFn = func() time.Duration { return 30 * time.Minute }
	}
	if cfg.WaitGroup != nil {
		cfg.WaitGroup.Add(1)
	}
	go func() {
		if cfg.WaitGroup != nil {
			defer cfg.WaitGroup.Done()
		}
		log.Printf("[INFO] ChannelAffinity 持久化已启动（每 %v 全量同步一次）", cfg.Interval)
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-cfg.ShutdownCtx.Done():
				finalCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
				if err := ca.flush(finalCtx, cfg.Store, ttlFn()); err != nil {
					log.Printf("[WARN] ChannelAffinity 关停前刷盘失败: %v", err)
				} else {
					log.Print("[INFO] ChannelAffinity 关停前已同步刷盘")
				}
				cancel()
				return
			case <-ticker.C:
				flushCtx, cancel := context.WithTimeout(cfg.ShutdownCtx, flushTimeout)
				if err := ca.flush(flushCtx, cfg.Store, ttlFn()); err != nil {
					log.Printf("[WARN] ChannelAffinity 周期刷盘失败: %v", err)
				}
				cancel()
			}
		}
	}()
}

func (ca *ChannelAffinity) flush(ctx context.Context, store storage.Store, ttl time.Duration) error {
	entries := ca.snapshot(ttl)
	return store.ChannelAffinityReplaceAll(ctx, entries)
}

// ListAll 返回所有未过期的亲和状态（admin API 用）
func (ca *ChannelAffinity) ListAll(ttl time.Duration) []ChannelAffinityStatus {
	now := time.Now()
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	result := make([]ChannelAffinityStatus, 0, len(ca.affinities))
	for model, entry := range ca.affinities {
		age := now.Sub(entry.updatedAt)
		if age > ttl {
			continue // 过期的不返回
		}
		result = append(result, ChannelAffinityStatus{
			Model:     model,
			ChannelID: entry.channelID,
			AgeMs:     age.Milliseconds(),
			TTLMs:     (ttl - age).Milliseconds(),
		})
	}
	return result
}

// applyChannelAffinity 在候选列表上应用渠道亲和
// 仅在 top priority bucket 内把亲和渠道 swap 到 position 0
// 不修改跨桶排序，不影响非亲和场景
func (s *Server) applyChannelAffinity(candidates []*modelpkg.Config, model string) []*modelpkg.Config {
	if len(candidates) <= 1 || s.channelAffinity == nil {
		return candidates
	}

	// 检查开关
	if s.configService != nil && !s.configService.GetBool("channel_affinity_enabled", true) {
		return candidates
	}

	// 读取 TTL
	ttlSec := 1800
	if s.configService != nil {
		ttlSec = s.configService.GetInt("channel_affinity_ttl_seconds", 1800)
	}
	if ttlSec <= 0 {
		return candidates
	}
	ttl := time.Duration(ttlSec) * time.Second

	// 查询亲和
	affinityID, ok := s.channelAffinity.Get(model, ttl)
	if !ok {
		return candidates
	}

	// 概率灰度：不是每个请求都应用亲和
	if s.configService != nil {
		prob := s.configService.GetFloat("channel_affinity_probability", 1.0)
		if prob < 1.0 && rand.Float64() >= prob {
			return candidates
		}
	}

	// 找 top bucket 边界，只在 top bucket 内做 swap
	topBucket := s.getEffPriorityBucket(candidates[0])

	for i, cfg := range candidates {
		// 已经出了 top bucket，亲和渠道不在最优桶里，放弃
		if s.getEffPriorityBucket(cfg) != topBucket {
			break
		}
		if cfg.ID == affinityID {
			if i != 0 {
				candidates[0], candidates[i] = candidates[i], candidates[0]
			}
			return candidates
		}
	}

	return candidates
}

// getEffPriorityBucket 获取渠道的有效优先级桶
// 复用 selector_balancer 的分桶逻辑
func (s *Server) getEffPriorityBucket(cfg *modelpkg.Config) int64 {
	if s.healthCache != nil && s.healthCache.Config().Enabled {
		stats := s.healthCache.GetHealthStats(cfg.ID)
		ep := s.calculateEffectivePriority(cfg, stats, s.healthCache.Config())
		return effPriorityBucket(ep)
	}
	// 健康分关闭时直接用 basePriority
	return int64(cfg.Priority)
}
