// Package storage 提供数据持久化和缓存层的实现。
// 包括 SQLite/MySQL 存储和内存缓存功能。
package storage

import (
	"context"
	"log"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"
)

// channelSnapshot COW 不可变快照，读操作直接返回引用（零拷贝）
// 写操作原子替换整个快照，旧快照由 GC 回收
type channelSnapshot struct {
	byModel map[string][]*modelpkg.Config // model → channels
	byType  map[string][]*modelpkg.Config // type → channels
	all     []*modelpkg.Config            // 所有渠道
	builtAt time.Time
}

// ChannelCache 高性能渠道缓存层
// 内存查询比数据库查询快 1000 倍+
// 改造为 COW 模式：读操作零拷贝，写操作原子替换快照
type ChannelCache struct {
	store    Store
	snapshot atomic.Pointer[channelSnapshot] // COW 快照，读不加锁
	ttl      time.Duration
	// refreshMu 保护刷新过程，防止多个 goroutine 并发刷新
	refreshMu sync.Mutex

	// API Key 缓存（按需加载，非 COW——写频率更高）
	apiKeysMu          sync.RWMutex
	apiKeysByChannelID map[int64][]*modelpkg.APIKey

	// 冷却状态缓存（短 TTL，独立管理）
	cooldownMu    sync.RWMutex
	cooldownCache struct {
		channels           map[int64]time.Time
		keys               map[int64]map[int]time.Time
		channelsLastUpdate time.Time
		keysLastUpdate     time.Time
		ttl                time.Duration
	}
}

// NewChannelCache 创建渠道缓存实例
func NewChannelCache(store Store, ttl time.Duration) *ChannelCache {
	c := &ChannelCache{
		store:              store,
		ttl:                ttl,
		apiKeysByChannelID: make(map[int64][]*modelpkg.APIKey),
	}
	// 初始化冷却缓存
	c.cooldownCache.channels = make(map[int64]time.Time)
	c.cooldownCache.keys = make(map[int64]map[int]time.Time)
	c.cooldownCache.ttl = 30 * time.Second

	// 存一个空快照，避免 nil 判断
	c.snapshot.Store(&channelSnapshot{
		byModel: make(map[string][]*modelpkg.Config),
		byType:  make(map[string][]*modelpkg.Config),
		all:     make([]*modelpkg.Config, 0),
		builtAt: time.Time{}, // 零值，首次读必定触发刷新
	})
	return c
}

// getSnapshot 获取当前快照（零拷贝读）
func (c *ChannelCache) getSnapshot() *channelSnapshot {
	return c.snapshot.Load()
}

// GetEnabledChannelsByModel 缓存优先的模型查询
// COW 模式：直接返回快照引用，零拷贝
// 调用方不得修改返回的 Config 对象（只读约定）
func (c *ChannelCache) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		return c.store.GetEnabledChannelsByModel(ctx, model)
	}

	snap := c.getSnapshot()
	if model == "*" {
		return snap.all, nil
	}
	channels, exists := snap.byModel[model]
	if !exists {
		return []*modelpkg.Config{}, nil
	}
	return channels, nil
}

// GetEnabledChannelsByType 缓存优先的类型查询
// COW 模式：直接返回快照引用，零拷贝
func (c *ChannelCache) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)
	if err := c.refreshIfNeeded(ctx); err != nil {
		return c.store.GetEnabledChannelsByType(ctx, normalizedType)
	}

	snap := c.getSnapshot()
	channels, exists := snap.byType[normalizedType]
	if !exists {
		return []*modelpkg.Config{}, nil
	}
	return channels, nil
}

// GetConfig 获取指定ID的渠道配置
// 直接查询数据库，保证数据永远是最新的
func (c *ChannelCache) GetConfig(ctx context.Context, channelID int64) (*modelpkg.Config, error) {
	return c.store.GetConfig(ctx, channelID)
}

// refreshIfNeeded 检查快照是否过期，过期则刷新
func (c *ChannelCache) refreshIfNeeded(ctx context.Context) error {
	snap := c.getSnapshot()
	if time.Since(snap.builtAt) <= c.ttl {
		return nil
	}
	return c.refreshCache(ctx)
}

// refreshCache 刷新缓存：构建新快照并原子替换
func (c *ChannelCache) refreshCache(ctx context.Context) error {
	// 只允许一个 goroutine 刷新，其他的用旧快照
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// 双重检查
	if time.Since(c.getSnapshot().builtAt) <= c.ttl {
		return nil
	}

	start := time.Now()
	allChannels, err := c.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return err
	}

	// 预构建 modelIndex，这样读操作不会触发 buildIndexIfNeeded 的写锁
	for _, ch := range allChannels {
		ch.PreBuildIndex()
	}

	byModel := make(map[string][]*modelpkg.Config)
	byType := make(map[string][]*modelpkg.Config)
	for _, channel := range allChannels {
		channelType := channel.GetChannelType()
		byType[channelType] = append(byType[channelType], channel)
		for _, model := range channel.GetModels() {
			byModel[model] = append(byModel[model], channel)
		}
	}

	// 原子替换快照
	c.snapshot.Store(&channelSnapshot{
		byModel: byModel,
		byType:  byType,
		all:     allChannels,
		builtAt: time.Now(),
	})

	refreshDuration := time.Since(start)
	if refreshDuration > 5*time.Second {
		log.Printf("[WARN]  缓存刷新过慢: %v, 渠道数: %d, 模型数: %d, 类型数: %d",
			refreshDuration, len(allChannels), len(byModel), len(byType))
	}

	return nil
}

// InvalidateCache 手动失效缓存（把 builtAt 置零，下次读触发刷新）
func (c *ChannelCache) InvalidateCache() {
	c.snapshot.Store(&channelSnapshot{
		byModel: make(map[string][]*modelpkg.Config),
		byType:  make(map[string][]*modelpkg.Config),
		all:     make([]*modelpkg.Config, 0),
		builtAt: time.Time{},
	})
}

// ============================================================================
// API Key 缓存（不走 COW，写频率更高）
// ============================================================================

// GetAPIKeys 缓存优先的API Keys查询
func (c *ChannelCache) GetAPIKeys(ctx context.Context, channelID int64) ([]*modelpkg.APIKey, error) {
	c.apiKeysMu.RLock()
	if keys, exists := c.apiKeysByChannelID[channelID]; exists {
		c.apiKeysMu.RUnlock()
		// 深拷贝: 防止调用方修改污染缓存
		result := make([]*modelpkg.APIKey, len(keys))
		for i, key := range keys {
			keyCopy := *key
			result[i] = &keyCopy
		}
		return result, nil
	}
	c.apiKeysMu.RUnlock()

	// 缓存未命中，从数据库加载
	keys, err := c.store.GetAPIKeys(ctx, channelID)
	if err != nil {
		return nil, err
	}

	c.apiKeysMu.Lock()
	c.apiKeysByChannelID[channelID] = keys
	c.apiKeysMu.Unlock()

	// 返回拷贝
	result := make([]*modelpkg.APIKey, len(keys))
	for i, key := range keys {
		keyCopy := *key
		result[i] = &keyCopy
	}
	return result, nil
}

// ============================================================================
// 冷却缓存
// ============================================================================

// GetAllChannelCooldowns 缓存优先的渠道冷却查询
func (c *ChannelCache) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	c.cooldownMu.RLock()
	if time.Since(c.cooldownCache.channelsLastUpdate) <= c.cooldownCache.ttl {
		result := make(map[int64]time.Time, len(c.cooldownCache.channels))
		maps.Copy(result, c.cooldownCache.channels)
		c.cooldownMu.RUnlock()
		return result, nil
	}
	c.cooldownMu.RUnlock()

	cooldowns, err := c.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		return nil, err
	}

	c.cooldownMu.Lock()
	c.cooldownCache.channels = cooldowns
	c.cooldownCache.channelsLastUpdate = time.Now()
	c.cooldownMu.Unlock()

	result := make(map[int64]time.Time, len(cooldowns))
	maps.Copy(result, cooldowns)
	return result, nil
}

// GetAllKeyCooldowns 缓存优先的Key冷却查询
func (c *ChannelCache) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	c.cooldownMu.RLock()
	if time.Since(c.cooldownCache.keysLastUpdate) <= c.cooldownCache.ttl {
		result := make(map[int64]map[int]time.Time)
		for k, v := range c.cooldownCache.keys {
			keyMap := make(map[int]time.Time)
			maps.Copy(keyMap, v)
			result[k] = keyMap
		}
		c.cooldownMu.RUnlock()
		return result, nil
	}
	c.cooldownMu.RUnlock()

	cooldowns, err := c.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		return nil, err
	}

	c.cooldownMu.Lock()
	c.cooldownCache.keys = cooldowns
	c.cooldownCache.keysLastUpdate = time.Now()
	c.cooldownMu.Unlock()

	result := make(map[int64]map[int]time.Time, len(cooldowns))
	for k, v := range cooldowns {
		keyMap := make(map[int]time.Time, len(v))
		maps.Copy(keyMap, v)
		result[k] = keyMap
	}
	return result, nil
}

// InvalidateAPIKeysCache 手动失效API Keys缓存
func (c *ChannelCache) InvalidateAPIKeysCache(channelID int64) {
	c.apiKeysMu.Lock()
	defer c.apiKeysMu.Unlock()
	delete(c.apiKeysByChannelID, channelID)
}

// InvalidateAllAPIKeysCache 清空所有API Key缓存（批量操作后使用）
func (c *ChannelCache) InvalidateAllAPIKeysCache() {
	c.apiKeysMu.Lock()
	defer c.apiKeysMu.Unlock()
	c.apiKeysByChannelID = make(map[int64][]*modelpkg.APIKey)
}

// InvalidateCooldownCache 手动失效冷却缓存
func (c *ChannelCache) InvalidateCooldownCache() {
	c.cooldownMu.Lock()
	defer c.cooldownMu.Unlock()
	c.cooldownCache.channelsLastUpdate = time.Time{}
	c.cooldownCache.keysLastUpdate = time.Time{}
}
