package app

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== 冷却管理 ====================
// 从admin.go拆分冷却管理,遵循SRP原则

// HandleSetChannelCooldown 设置渠道级别冷却
func (s *Server) HandleSetChannelCooldown(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetChannelCooldown(c.Request.Context(), id, until)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 精确计数(手动设置渠道冷却

	RespondJSON(c, http.StatusOK, gin.H{"message": fmt.Sprintf("渠道已冷却 %d 毫秒", req.DurationMs)})
}

// HandleClearChannelAllCooldowns 一键清除渠道所有冷却（渠道级+Key级+URL级）
func (s *Server) HandleClearChannelAllCooldowns(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}

	ctx := c.Request.Context()

	// 清渠道级冷却
	_ = s.cooldownManager.ClearChannelCooldown(ctx, id)

	// 清所有Key冷却
	apiKeys, _ := s.store.GetAPIKeys(ctx, id)
	for i := range apiKeys {
		_ = s.cooldownManager.ClearKeyCooldown(ctx, id, i)
	}

	// 清所有URL冷却 + 重置失败计数
	urlCleared := 0
	if s.urlSelector != nil {
		urlCleared = s.urlSelector.ClearChannelCooldowns(id)
	}

	s.invalidateChannelRelatedCache(id)

	RespondJSON(c, http.StatusOK, gin.H{
		"message":     fmt.Sprintf("已清除渠道所有冷却（URL: %d 条）", urlCleared),
		"url_cleared": urlCleared,
	})
}

// HandleSetKeyCooldown 设置Key级别冷却
func (s *Server) HandleSetKeyCooldown(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}

	keyIndexStr := c.Param("keyIndex")
	keyIndex, err := strconv.Atoi(keyIndexStr)
	if err != nil || keyIndex < 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid key index")
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetKeyCooldown(c.Request.Context(), id, keyIndex, until)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// [INFO] 修复：使API Keys缓存失效，确保前端能立即看到冷却状态
	s.InvalidateAPIKeysCache(id)

	RespondJSON(c, http.StatusOK, gin.H{"message": fmt.Sprintf("Key #%d 已冷却 %d 毫秒", keyIndex+1, req.DurationMs)})
}
