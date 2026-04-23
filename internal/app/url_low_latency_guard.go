package app

import "time"

// 低延迟守卫默认值（与 migrate.go 中的 system_settings 默认值保持一致）
const (
	defaultLowLatencyAffinityMinMs       = 900
	defaultLowLatencyCooldownMs          = 300
	defaultLowLatencyCooldownDurationSec = 300
	defaultLowLatencyGuardEnabled        = true
)

// lowLatencyGuardThresholds 返回两个阈值：
//   - suspectCooldown: TTFB低于此值触发URL冷却（不写EWMA、不写亲和）
//   - affinityMinLatency: TTFB低于此值不写URL亲和（但仍写EWMA）
//
// 仅当守卫启用且请求是流式时生效，否则返回 (0, 0) 让调用方走旧行为
func (s *Server) lowLatencyGuardThresholds(isStreaming bool) (time.Duration, time.Duration) {
	if !isStreaming {
		return 0, 0
	}
	if s.configService == nil {
		return time.Duration(defaultLowLatencyCooldownMs) * time.Millisecond,
			time.Duration(defaultLowLatencyAffinityMinMs) * time.Millisecond
	}
	if !s.configService.GetBool("low_latency_guard_enabled", defaultLowLatencyGuardEnabled) {
		return 0, 0
	}
	cdMs := s.configService.GetInt("low_latency_cooldown_ms", defaultLowLatencyCooldownMs)
	affMs := s.configService.GetInt("low_latency_affinity_min_ms", defaultLowLatencyAffinityMinMs)
	// 冷却阈值必须严格小于亲和阈值，否则语义不自洽：用兜底默认值
	if cdMs < 0 || affMs < 0 || cdMs >= affMs {
		cdMs = defaultLowLatencyCooldownMs
		affMs = defaultLowLatencyAffinityMinMs
	}
	return time.Duration(cdMs) * time.Millisecond, time.Duration(affMs) * time.Millisecond
}

// lowLatencyCooldownDuration 返回可疑低延迟URL的固定冷却时长
func (s *Server) lowLatencyCooldownDuration() time.Duration {
	sec := defaultLowLatencyCooldownDurationSec
	if s.configService != nil {
		sec = s.configService.GetInt("low_latency_cooldown_duration_seconds", defaultLowLatencyCooldownDurationSec)
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}
