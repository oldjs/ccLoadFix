package app

import "ccLoad/internal/model"

// cloneConfigForAdminResponse 给管理端响应单独拷一份配置，别把运行时对象改脏了。
func cloneConfigForAdminResponse(src *model.Config) *model.Config {
	if src == nil {
		return nil
	}

	dst := &model.Config{
		ID:                 src.ID,
		Name:               src.Name,
		ChannelType:        src.ChannelType,
		URL:                src.URL,
		Priority:           src.Priority,
		Enabled:            src.Enabled,
		CooldownUntil:      src.CooldownUntil,
		CooldownDurationMs: src.CooldownDurationMs,
		DailyCostLimit:     src.DailyCostLimit,
		CreatedAt:          src.CreatedAt,
		UpdatedAt:          src.UpdatedAt,
		KeyCount:           src.KeyCount,
	}

	if src.ModelEntries != nil {
		dst.ModelEntries = make([]model.ModelEntry, len(src.ModelEntries))
		copy(dst.ModelEntries, src.ModelEntries)
		for i := range dst.ModelEntries {
			if dst.ModelEntries[i].RedirectModel == "" {
				dst.ModelEntries[i].RedirectModel = dst.ModelEntries[i].Model
			}
		}
	}

	return dst
}
