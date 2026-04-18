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
	ttlSec := 600
	if s.configService != nil {
		ttlSec = s.configService.GetInt("channel_affinity_ttl_seconds", 600)
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
