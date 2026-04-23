package app

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HandleChannelAffinity 返回当前所有渠道亲和状态
// GET /admin/channel-affinity
func (s *Server) HandleChannelAffinity(c *gin.Context) {
	if s.channelAffinity == nil {
		RespondJSON(c, http.StatusOK, []ChannelAffinityStatus{})
		return
	}

	// 读取 TTL 配置
	ttlSec := 1800
	if s.configService != nil {
		ttlSec = s.configService.GetInt("channel_affinity_ttl_seconds", 1800)
	}
	ttl := time.Duration(ttlSec) * time.Second

	result := s.channelAffinity.ListAll(ttl)
	if result == nil {
		result = []ChannelAffinityStatus{}
	}

	RespondJSON(c, http.StatusOK, result)
}

// HandleClearChannelAffinity 清除所有渠道亲和
// DELETE /admin/channel-affinity
func (s *Server) HandleClearChannelAffinity(c *gin.Context) {
	if s.channelAffinity == nil {
		RespondJSON(c, http.StatusOK, gin.H{"message": "channel affinity not initialized"})
		return
	}

	// 用 0 TTL 清掉所有条目
	s.channelAffinity.Cleanup(0)

	RespondJSON(c, http.StatusOK, gin.H{"message": "all channel affinities cleared"})
}

// HandleURLAffinity 返回所有 URL 级亲和条目（跨渠道扫所有分片）
// GET /admin/url-affinity
func (s *Server) HandleURLAffinity(c *gin.Context) {
	if s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, []URLAffinityStatus{})
		return
	}
	list := s.urlSelector.ListAllAffinities()
	if list == nil {
		list = []URLAffinityStatus{}
	}
	RespondJSON(c, http.StatusOK, list)
}

// HandleURLWarm 返回所有 (渠道, 模型) 的 warm 备选列表
// GET /admin/url-warm
func (s *Server) HandleURLWarm(c *gin.Context) {
	if s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, []URLWarmStatus{})
		return
	}
	list := s.urlSelector.ListAllWarms()
	if list == nil {
		list = []URLWarmStatus{}
	}
	RespondJSON(c, http.StatusOK, list)
}

// HandleWarmBoostCandidates 返回当前所有具备 warm boost 资格的 (channel, model) 条目
// GET /admin/warm-boost-candidates
// 用途：管理员在 affinity 页上直观看到——当 channel affinity 失效时，哪些 channel 会被这条
// 跨渠道 warm 软兜底加权、依据是什么（age / tier / boost_prob / 当前是否活跃）
func (s *Server) HandleWarmBoostCandidates(c *gin.Context) {
	list := s.ListWarmBoostCandidates(time.Now())
	if list == nil {
		list = []WarmBoostCandidateStatus{}
	}
	RespondJSON(c, http.StatusOK, list)
}
