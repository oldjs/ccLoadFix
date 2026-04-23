package app

import "time"

// URLAffinityStatus URL 级亲和状态快照（admin 展示用）
// 一条记录 = (渠道, 模型) 当前粘的上游 URL
type URLAffinityStatus struct {
	ChannelID int64  `json:"channel_id"`
	Model     string `json:"model"`
	URL       string `json:"url"`
	AgeMs     int64  `json:"age_ms"` // 距离上次使用多久（毫秒）
}

// WarmSlotStatus warm 列表中单个 URL 槽位的状态
type WarmSlotStatus struct {
	URL         string `json:"url"`
	AgeMs       int64  `json:"age_ms"`        // 距离上次成功多久
	TTLRemainMs int64  `json:"ttl_remain_ms"` // 距离 warm TTL 过期还剩多久
}

// URLWarmStatus 单个 (渠道, 模型) 的 warm 备选列表
type URLWarmStatus struct {
	ChannelID int64            `json:"channel_id"`
	Model     string           `json:"model"`
	Slots     []WarmSlotStatus `json:"slots"` // 按新→旧排列
}

// ListAllAffinities 扫所有分片，导出当前所有 URL 亲和条目（admin 展示用）
// 锁粒度：每片一次 RLock，互不阻塞热路径
func (s *URLSelector) ListAllAffinities() []URLAffinityStatus {
	now := time.Now()
	var out []URLAffinityStatus
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k, e := range sh.affinities {
			if e == nil {
				continue
			}
			out = append(out, URLAffinityStatus{
				ChannelID: k.channelID,
				Model:     k.model,
				URL:       e.url,
				AgeMs:     now.Sub(e.lastUsed).Milliseconds(),
			})
		}
		sh.mu.RUnlock()
	}
	return out
}

// ListAllWarms 扫所有分片，导出全部 warm 列表（admin 展示用）
// 每个 entry 内的 slot 已按 lastSuccess 倒序存放，[0] 最新
func (s *URLSelector) ListAllWarms() []URLWarmStatus {
	now := time.Now()
	var out []URLWarmStatus
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k, entry := range sh.warms {
			if entry == nil || entry.count == 0 {
				continue
			}
			slots := make([]WarmSlotStatus, 0, entry.count)
			for j := uint8(0); j < entry.count; j++ {
				slot := entry.slots[j]
				if slot.url == "" {
					continue
				}
				age := now.Sub(slot.lastSuccess)
				ttlRemain := max(warmEntryTTL-age, 0)
				slots = append(slots, WarmSlotStatus{
					URL:         slot.url,
					AgeMs:       age.Milliseconds(),
					TTLRemainMs: ttlRemain.Milliseconds(),
				})
			}
			if len(slots) == 0 {
				continue
			}
			out = append(out, URLWarmStatus{
				ChannelID: k.channelID,
				Model:     k.model,
				Slots:     slots,
			})
		}
		sh.mu.RUnlock()
	}
	return out
}
