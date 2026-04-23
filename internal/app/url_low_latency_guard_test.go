package app

import (
	"testing"
	"time"

	"ccLoad/internal/model"
)

// setGuardSetting 直接往 ConfigService cache 塞配置，绕过存储层
func setGuardSetting(cs *ConfigService, key, value string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.cache[key] = &model.SystemSetting{Key: key, Value: value}
}

func TestServer_LowLatencyGuardThresholds_NonStreamingReturnsZero(t *testing.T) {
	s := &Server{}
	cd, aff := s.lowLatencyGuardThresholds(false)
	if cd != 0 || aff != 0 {
		t.Fatalf("non-streaming should disable guard, got cd=%v aff=%v", cd, aff)
	}
}

func TestServer_LowLatencyGuardThresholds_NilConfigServiceUsesDefaults(t *testing.T) {
	s := &Server{}
	cd, aff := s.lowLatencyGuardThresholds(true)
	wantCD := time.Duration(defaultLowLatencyCooldownMs) * time.Millisecond
	wantAff := time.Duration(defaultLowLatencyAffinityMinMs) * time.Millisecond
	if cd != wantCD || aff != wantAff {
		t.Fatalf("nil config should fall back to defaults, got cd=%v aff=%v want cd=%v aff=%v", cd, aff, wantCD, wantAff)
	}
}

func TestServer_LowLatencyGuardThresholds_DisabledReturnsZero(t *testing.T) {
	cs := &ConfigService{cache: make(map[string]*model.SystemSetting), loaded: true}
	setGuardSetting(cs, "low_latency_guard_enabled", "false")
	s := &Server{configService: cs}
	cd, aff := s.lowLatencyGuardThresholds(true)
	if cd != 0 || aff != 0 {
		t.Fatalf("disabled guard should return zeros, got cd=%v aff=%v", cd, aff)
	}
}

func TestServer_LowLatencyGuardThresholds_CustomValues(t *testing.T) {
	cs := &ConfigService{cache: make(map[string]*model.SystemSetting), loaded: true}
	setGuardSetting(cs, "low_latency_guard_enabled", "true")
	setGuardSetting(cs, "low_latency_cooldown_ms", "150")
	setGuardSetting(cs, "low_latency_affinity_min_ms", "1200")
	s := &Server{configService: cs}

	cd, aff := s.lowLatencyGuardThresholds(true)
	if cd != 150*time.Millisecond {
		t.Fatalf("cd want 150ms, got %v", cd)
	}
	if aff != 1200*time.Millisecond {
		t.Fatalf("aff want 1200ms, got %v", aff)
	}
}

func TestServer_LowLatencyGuardThresholds_InvalidConfigFallsBack(t *testing.T) {
	// 冷却阈值 >= 亲和阈值，语义不自洽，应回退默认
	cs := &ConfigService{cache: make(map[string]*model.SystemSetting), loaded: true}
	setGuardSetting(cs, "low_latency_guard_enabled", "true")
	setGuardSetting(cs, "low_latency_cooldown_ms", "1000")
	setGuardSetting(cs, "low_latency_affinity_min_ms", "500")
	s := &Server{configService: cs}

	cd, aff := s.lowLatencyGuardThresholds(true)
	if cd != time.Duration(defaultLowLatencyCooldownMs)*time.Millisecond ||
		aff != time.Duration(defaultLowLatencyAffinityMinMs)*time.Millisecond {
		t.Fatalf("invalid config should fall back to defaults, got cd=%v aff=%v", cd, aff)
	}
}

func TestServer_LowLatencyCooldownDuration_DefaultsAndOverrides(t *testing.T) {
	// nil config → default
	s := &Server{}
	if got := s.lowLatencyCooldownDuration(); got != time.Duration(defaultLowLatencyCooldownDurationSec)*time.Second {
		t.Fatalf("nil config expected default %ds, got %v", defaultLowLatencyCooldownDurationSec, got)
	}

	// 自定义值
	cs := &ConfigService{cache: make(map[string]*model.SystemSetting), loaded: true}
	setGuardSetting(cs, "low_latency_cooldown_duration_seconds", "60")
	s = &Server{configService: cs}
	if got := s.lowLatencyCooldownDuration(); got != 60*time.Second {
		t.Fatalf("custom 60s expected, got %v", got)
	}

	// 非正值 → 返回 0
	setGuardSetting(cs, "low_latency_cooldown_duration_seconds", "0")
	if got := s.lowLatencyCooldownDuration(); got != 0 {
		t.Fatalf("zero duration should return 0, got %v", got)
	}
}
