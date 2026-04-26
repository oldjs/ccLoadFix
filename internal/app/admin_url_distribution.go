package app

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
)

// URLDistributionEntry 单条 URL 的分发统计快照（admin 面板用）
// 合并 URLSelector.GetURLStats（延迟/成功率/冷却）和 URLSmoothWeightedRR.SnapshotChannel（被选次数/权重/最后使用）
type URLDistributionEntry struct {
	ChannelID          int64   `json:"channel_id"`
	ChannelName        string  `json:"channel_name"`
	URL                string  `json:"url"`
	Selections         int64   `json:"selections"`           // SmoothWRR 累计命中数
	Requests           int64   `json:"requests"`             // 实际成功请求数
	Failures           int64   `json:"failures"`             // 失败次数
	SuccessRate        float64 `json:"success_rate"`         // 0..1，无样本时为 1
	TTFBLatencyMs      float64 `json:"ttfb_latency_ms"`      // 真实 TTFB EWMA，-1 表示无数据
	EffectiveLatencyMs float64 `json:"effective_latency_ms"` // 含 slow penalty 后的延迟（用于权重计算）
	CurrentWeight      int64   `json:"current_weight"`       // SmoothWRR 当前权重值（可正可负）
	LastSelectedAtMs   int64   `json:"last_selected_at_ms"`  // Unix 毫秒，0 表示从未被选
	IdleMs             int64   `json:"idle_ms"`              // 距最后被选过去多久，-1 表示从未被选
	CooledDown         bool    `json:"cooled_down"`
	SlowIsolated       bool    `json:"slow_isolated"`
}

// URLDistributionResponse URL 分发面板的总响应
type URLDistributionResponse struct {
	Entries          []URLDistributionEntry `json:"entries"`
	TotalSelections  int64                  `json:"total_selections"`
	TotalURLs        int                    `json:"total_urls"`
	ActiveURLs       int                    `json:"active_urls"`        // 最近 IdleThresholdMs 内被选过的
	IdleURLs         int                    `json:"idle_urls"`          // 超过 IdleThresholdMs 没被选过 / 从未被选
	IdleThresholdMs  int64                  `json:"idle_threshold_ms"`  // 闲置阈值（毫秒），默认 5 分钟
}

// HandleURLDistribution 返回所有渠道的 URL 分发统计（管理员面板用）
// GET /admin/url-distribution[?idle_threshold_ms=N]
//
// 数据完全从内存聚合：URLSelector 状态 + URLSmoothWeightedRR 状态，无 DB 查询
func (s *Server) HandleURLDistribution(c *gin.Context) {
	if s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, URLDistributionResponse{
			Entries: []URLDistributionEntry{},
		})
		return
	}

	// 闲置阈值默认 5 分钟，允许通过 query 自定义
	idleThresholdMs := int64(5 * 60 * 1000)
	if v := c.Query("idle_threshold_ms"); v != "" {
		if parsed, err := parsePositiveInt64(v); err == nil && parsed > 0 {
			idleThresholdMs = parsed
		}
	}

	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 渠道 ID → 名称 map，方便前端展示
	nameMap := make(map[int64]string, len(cfgs))
	channelURLs := make(map[int64][]string, len(cfgs))
	for _, cfg := range cfgs {
		if cfg == nil || !cfg.Enabled {
			continue
		}
		nameMap[cfg.ID] = cfg.Name
		channelURLs[cfg.ID] = cfg.GetURLs()
	}

	entries := make([]URLDistributionEntry, 0, 64)
	for cid, urls := range channelURLs {
		if len(urls) == 0 {
			continue
		}
		// 合并：URLStat（延迟/成功率/冷却） + Snapshot（轮询命中/权重/最后使用）
		statsByURL := make(map[string]URLStat, len(urls))
		for _, st := range s.urlSelector.GetURLStats(cid, urls) {
			statsByURL[st.URL] = st
		}
		var snap URLChannelDistribution
		if s.urlSelector.urlBalancer != nil {
			snap = s.urlSelector.urlBalancer.SnapshotChannel(cid)
		}
		selByURL := make(map[string]URLSelectionStat, len(snap.URLs))
		for _, s := range snap.URLs {
			selByURL[s.URL] = s
		}

		for _, u := range urls {
			st := statsByURL[u]
			sel := selByURL[u]
			// 计算成功率
			rate := 1.0
			if total := st.Requests + st.Failures; total > 0 {
				rate = float64(st.Requests) / float64(total)
			}
			entries = append(entries, URLDistributionEntry{
				ChannelID:          cid,
				ChannelName:        nameMap[cid],
				URL:                u,
				Selections:         sel.Selections,
				Requests:           st.Requests,
				Failures:           st.Failures,
				SuccessRate:        rate,
				TTFBLatencyMs:      st.TTFBLatencyMs,
				EffectiveLatencyMs: st.EffectiveLatencyMs,
				CurrentWeight:      sel.CurrentWeight,
				LastSelectedAtMs:   sel.LastSelectedAtMs,
				IdleMs:             sel.IdleMs,
				CooledDown:         st.CooledDown,
				SlowIsolated:       st.SlowIsolated,
			})
		}
	}

	// 默认按 selections 倒序排，扎堆 URL 一眼可见
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Selections != entries[j].Selections {
			return entries[i].Selections > entries[j].Selections
		}
		// selections 相同时按渠道+URL 字典序，保证稳定
		if entries[i].ChannelID != entries[j].ChannelID {
			return entries[i].ChannelID < entries[j].ChannelID
		}
		return entries[i].URL < entries[j].URL
	})

	// 汇总数据
	var totalSelections int64
	activeURLs, idleURLs := 0, 0
	for _, e := range entries {
		totalSelections += e.Selections
		switch {
		case e.IdleMs < 0:
			// 从未被选
			idleURLs++
		case e.IdleMs <= idleThresholdMs:
			activeURLs++
		default:
			idleURLs++
		}
	}

	RespondJSON(c, http.StatusOK, URLDistributionResponse{
		Entries:         entries,
		TotalSelections: totalSelections,
		TotalURLs:       len(entries),
		ActiveURLs:      activeURLs,
		IdleURLs:        idleURLs,
		IdleThresholdMs: idleThresholdMs,
	})
}

// parsePositiveInt64 把字符串 query 参数解成 int64（手动循环，避免 strconv 处理负数/前缀的歧义）
func parsePositiveInt64(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, errInvalidInt
		}
		n = n*10 + int64(ch-'0')
		// 防溢出：单次循环不会立刻溢出，但极端情况下保护
		if n < 0 {
			return 0, errInvalidInt
		}
	}
	return n, nil
}

type errStr string

func (e errStr) Error() string { return string(e) }

const errInvalidInt = errStr("invalid int")
