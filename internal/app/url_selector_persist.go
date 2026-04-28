package app

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// ============================================================================
// URLSelector 持久化：路由热数据落盘 / 启动恢复
//
// 写入策略：每 30 秒全量快照 → DELETE + 批量 INSERT 在同一事务里替换。
// 业务唯一性由内存 map 保证；DB 表无主键，避免 InnoDB utf8mb4 长 URL 字段
// 在主键上的长度限制问题。
//
// 关停时再做一次最后的同步刷盘，避免最后一窗口的数据完全丢失。
// ============================================================================

// affinityPayload affinity kind 的 JSON 载荷
type affinityPayload struct {
	URL      string `json:"url"`
	LastUsed int64  `json:"last_used"` // unix 秒
}

// warmSlotPayload warm kind 的 slot JSON 载荷
type warmSlotPayload struct {
	URL         string `json:"url"`
	LastSuccess int64  `json:"last_success"` // unix 秒
}

// snapshotEntries 扫所有 shard，把当前未过期状态扁平化为 entries 切片
// 调用过程中按 shard 加 RLock，跨 shard 之间不互斥
func (s *URLSelector) snapshotEntries(now time.Time) []model.URLRuntimeState {
	var entries []model.URLRuntimeState
	nowUnix := now.Unix()

	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		entries = appendShardEntries(entries, sh, now, nowUnix, s.latencyMaxAge)
		sh.mu.RUnlock()
	}
	return entries
}

// appendShardEntries 把单个 shard 的状态追加到 entries
// 调用方需持有 sh.mu 读锁
func appendShardEntries(out []model.URLRuntimeState, sh *urlShard, now time.Time, nowUnix int64, latencyMaxAge time.Duration) []model.URLRuntimeState {
	// EWMA 太老的不持久化（跟 gcShard 行为对齐）
	latencyCutoff := time.Time{}
	if latencyMaxAge > 0 {
		latencyCutoff = now.Add(-latencyMaxAge)
	}

	// 真实请求 EWMA
	for k, ewma := range sh.latencies {
		if ewma == nil || ewma.lastSeen.IsZero() {
			continue
		}
		if !latencyCutoff.IsZero() && ewma.lastSeen.Before(latencyCutoff) {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			URL:       k.url,
			Kind:      model.URLRuntimeKindLatency,
			EWMAms:    ewma.value,
			UpdatedAt: ewma.lastSeen.Unix(),
		})
	}

	// 探测 RTT 种子
	for k, ewma := range sh.probeLatencies {
		if ewma == nil || ewma.lastSeen.IsZero() {
			continue
		}
		if !latencyCutoff.IsZero() && ewma.lastSeen.Before(latencyCutoff) {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			URL:       k.url,
			Kind:      model.URLRuntimeKindProbeLatency,
			EWMAms:    ewma.value,
			UpdatedAt: ewma.lastSeen.Unix(),
		})
	}

	// URL 级冷却（过期的不持久化）
	for k, cd := range sh.cooldowns {
		if !now.Before(cd.until) {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID:        k.channelID,
			URL:              k.url,
			Kind:             model.URLRuntimeKindCooldown,
			ExpiresAt:        cd.until.Unix(),
			ConsecutiveFails: int64(cd.consecutiveFails),
			UpdatedAt:        nowUnix,
		})
	}

	// 慢 TTFB 隔离
	for k, until := range sh.slowIsolations {
		if !now.Before(until) {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			URL:       k.url,
			Kind:      model.URLRuntimeKindSlowIso,
			ExpiresAt: until.Unix(),
			UpdatedAt: nowUnix,
		})
	}

	// thinking 黑名单（带 model 维度）
	for k, expiry := range sh.noThinkingBlklist {
		if !now.Before(expiry) {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			URL:       k.url,
			Model:     k.model,
			Kind:      model.URLRuntimeKindNoThinking,
			ExpiresAt: expiry.Unix(),
			UpdatedAt: nowUnix,
		})
	}

	// (channel, model) 维度的亲和（24h 自然过期，跟 gcShard 对齐）
	affinityCutoff := now.Add(-24 * time.Hour)
	for k, aff := range sh.affinities {
		if aff == nil || aff.url == "" || aff.lastUsed.Before(affinityCutoff) {
			continue
		}
		payload, err := json.Marshal(affinityPayload{
			URL:      aff.url,
			LastUsed: aff.lastUsed.Unix(),
		})
		if err != nil {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			Model:     k.model,
			Kind:      model.URLRuntimeKindAffinity,
			Payload:   string(payload),
			UpdatedAt: aff.lastUsed.Unix(),
		})
	}

	// warm 备选 slot 列表
	warmCutoff := now.Add(-warmEntryTTL)
	for k, entry := range sh.warms {
		if entry == nil || entry.count == 0 {
			continue
		}
		slots := make([]warmSlotPayload, 0, entry.count)
		for i := uint8(0); i < entry.count; i++ {
			s := entry.slots[i]
			if s.url == "" || s.lastSuccess.Before(warmCutoff) {
				continue
			}
			slots = append(slots, warmSlotPayload{
				URL:         s.url,
				LastSuccess: s.lastSuccess.Unix(),
			})
		}
		if len(slots) == 0 {
			continue
		}
		payload, err := json.Marshal(slots)
		if err != nil {
			continue
		}
		out = append(out, model.URLRuntimeState{
			ChannelID: k.channelID,
			Model:     k.model,
			Kind:      model.URLRuntimeKindWarm,
			Payload:   string(payload),
			UpdatedAt: nowUnix,
		})
	}

	return out
}

// LoadFromStore 启动时一次性恢复内存状态
// 过期数据自动跳过；JSON 反序列化失败的条目会被静默丢弃，不阻塞启动
func (s *URLSelector) LoadFromStore(ctx context.Context, store storage.Store) (int, error) {
	if store == nil {
		return 0, nil
	}
	entries, err := store.URLRuntimeStateLoadAll(ctx)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	now := time.Now()

	// 按 shard 分组，每个 shard 一次锁批量恢复
	perShard := make(map[int][]model.URLRuntimeState)
	for _, e := range entries {
		idx := int(uint64(e.ChannelID) % urlShardCount)
		perShard[idx] = append(perShard[idx], e)
	}

	loaded := 0
	for idx, list := range perShard {
		sh := &s.shards[idx]
		sh.mu.Lock()
		for _, e := range list {
			if s.applyLoadedEntry(sh, e, now) {
				loaded++
			}
		}
		sh.mu.Unlock()
	}
	return loaded, nil
}

// applyLoadedEntry 单条 entry 写回内存，返回是否实际加载（过期或解析失败返回 false）
// 调用方需持有 sh.mu 写锁
func (s *URLSelector) applyLoadedEntry(sh *urlShard, e model.URLRuntimeState, now time.Time) bool {
	switch e.Kind {
	case model.URLRuntimeKindLatency:
		if e.URL == "" {
			return false
		}
		ts := time.Unix(e.UpdatedAt, 0)
		if s.latencyMaxAge > 0 && ts.Before(now.Add(-s.latencyMaxAge)) {
			return false
		}
		sh.latencies[urlKey{channelID: e.ChannelID, url: e.URL}] = &ewmaValue{
			value:    e.EWMAms,
			lastSeen: ts,
		}
		return true

	case model.URLRuntimeKindProbeLatency:
		if e.URL == "" {
			return false
		}
		ts := time.Unix(e.UpdatedAt, 0)
		if s.latencyMaxAge > 0 && ts.Before(now.Add(-s.latencyMaxAge)) {
			return false
		}
		sh.probeLatencies[urlKey{channelID: e.ChannelID, url: e.URL}] = &ewmaValue{
			value:    e.EWMAms,
			lastSeen: ts,
		}
		return true

	case model.URLRuntimeKindCooldown:
		if e.URL == "" {
			return false
		}
		until := time.Unix(e.ExpiresAt, 0)
		if !now.Before(until) {
			return false
		}
		sh.cooldowns[urlKey{channelID: e.ChannelID, url: e.URL}] = urlCooldownState{
			until:            until,
			consecutiveFails: int(e.ConsecutiveFails),
		}
		return true

	case model.URLRuntimeKindSlowIso:
		if e.URL == "" {
			return false
		}
		until := time.Unix(e.ExpiresAt, 0)
		if !now.Before(until) {
			return false
		}
		sh.slowIsolations[urlKey{channelID: e.ChannelID, url: e.URL}] = until
		return true

	case model.URLRuntimeKindNoThinking:
		if e.URL == "" || e.Model == "" {
			return false
		}
		until := time.Unix(e.ExpiresAt, 0)
		if !now.Before(until) {
			return false
		}
		sh.noThinkingBlklist[urlModelKey{channelID: e.ChannelID, url: e.URL, model: e.Model}] = until
		return true

	case model.URLRuntimeKindAffinity:
		if e.Model == "" || e.Payload == "" {
			return false
		}
		var p affinityPayload
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil || p.URL == "" {
			return false
		}
		lastUsed := time.Unix(p.LastUsed, 0)
		// 24h 过期的不恢复
		if lastUsed.Before(now.Add(-24 * time.Hour)) {
			return false
		}
		sh.affinities[modelAffinityKey{channelID: e.ChannelID, model: e.Model}] = &affinityEntry{
			url:      p.URL,
			lastUsed: lastUsed,
		}
		return true

	case model.URLRuntimeKindWarm:
		if e.Model == "" || e.Payload == "" {
			return false
		}
		var slots []warmSlotPayload
		if err := json.Unmarshal([]byte(e.Payload), &slots); err != nil || len(slots) == 0 {
			return false
		}
		warmCutoff := now.Add(-warmEntryTTL)
		we := &warmEntry{}
		for _, sl := range slots {
			if sl.URL == "" || we.count >= warmCapacity {
				continue
			}
			ts := time.Unix(sl.LastSuccess, 0)
			if ts.Before(warmCutoff) {
				continue
			}
			we.slots[we.count] = warmSlot{url: sl.URL, lastSuccess: ts}
			we.count++
		}
		if we.count == 0 {
			return false
		}
		sh.warms[modelAffinityKey{channelID: e.ChannelID, model: e.Model}] = we
		return true
	}
	return false
}

// URLSelectorPersistenceConfig 持久化运行参数
type URLSelectorPersistenceConfig struct {
	Store        storage.Store
	Interval     time.Duration   // flush 间隔，0 或负值表示禁用周期 flush
	ShutdownCtx  context.Context // 取消时退出 goroutine 并做最后一次同步刷盘
	WaitGroup    *sync.WaitGroup // Server 用来等待 goroutine 退出
	FlushTimeout time.Duration   // 单次刷盘超时，0 用默认 10s
}

// StartPersistence 启动周期 flush goroutine。Store 为 nil 或 Interval<=0 时直接 no-op。
func (s *URLSelector) StartPersistence(cfg URLSelectorPersistenceConfig) {
	if cfg.Store == nil || cfg.Interval <= 0 {
		return
	}
	flushTimeout := cfg.FlushTimeout
	if flushTimeout <= 0 {
		flushTimeout = 10 * time.Second
	}
	if cfg.WaitGroup != nil {
		cfg.WaitGroup.Add(1)
	}
	go func() {
		if cfg.WaitGroup != nil {
			defer cfg.WaitGroup.Done()
		}
		log.Printf("[INFO] URLSelector 持久化已启动（每 %v 全量同步一次）", cfg.Interval)
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-cfg.ShutdownCtx.Done():
				// 关停后用独立 context 做最后一次刷盘，避免被 baseCtx 取消打断
				finalCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
				if err := s.flush(finalCtx, cfg.Store); err != nil {
					log.Printf("[WARN] URLSelector 关停前刷盘失败: %v", err)
				} else {
					log.Print("[INFO] URLSelector 关停前已同步刷盘")
				}
				cancel()
				return
			case <-ticker.C:
				flushCtx, cancel := context.WithTimeout(cfg.ShutdownCtx, flushTimeout)
				if err := s.flush(flushCtx, cfg.Store); err != nil {
					log.Printf("[WARN] URLSelector 周期刷盘失败: %v", err)
				}
				cancel()
			}
		}
	}()
}

// flush 把当前所有 shard 状态全量替换到 DB
func (s *URLSelector) flush(ctx context.Context, store storage.Store) error {
	entries := s.snapshotEntries(time.Now())
	return store.URLRuntimeStateReplaceAll(ctx, entries)
}
