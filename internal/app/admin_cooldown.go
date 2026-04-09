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

	// 手动改完冷却就把缓存打掉，列表页才能马上看到新状态。
	s.invalidateCooldownCache()

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
	for _, apiKey := range apiKeys {
		_ = s.cooldownManager.ClearKeyCooldown(ctx, id, apiKey.KeyIndex)
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
	// Key 冷却也会出现在渠道列表里，这里一起把冷却缓存打掉。
	s.invalidateCooldownCache()

	RespondJSON(c, http.StatusOK, gin.H{"message": fmt.Sprintf("Key #%d 已冷却 %d 毫秒", keyIndex+1, req.DurationMs)})
}

// HandleGetNoThinkingList 获取渠道的 thinking 黑名单
// GET /admin/channels/:id/no-thinking
func (s *Server) HandleGetNoThinkingList(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}
	if s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, []any{})
		return
	}
	list := s.urlSelector.GetNoThinkingList(id)
	if list == nil {
		list = []NoThinkingEntry{}
	}
	RespondJSON(c, http.StatusOK, list)
}

// HandleClearNoThinking 清除渠道的 thinking 黑名单
// DELETE /admin/channels/:id/no-thinking
func (s *Server) HandleClearNoThinking(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}
	if s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, gin.H{"cleared": 0})
		return
	}

	// 支持精确清除：?url=xxx&model=yyy，不传就全清
	rawURL := c.Query("url")
	model := c.Query("model")
	cleared := s.urlSelector.ClearNoThinking(id, rawURL, model)
	RespondJSON(c, http.StatusOK, gin.H{
		"cleared": cleared,
		"message": fmt.Sprintf("已清除 %d 条黑名单", cleared),
	})
}
