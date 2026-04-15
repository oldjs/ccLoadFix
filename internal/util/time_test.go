package util

import (
	"testing"
	"time"
)

func TestCalculateBackoffDuration_504Error(t *testing.T) {
	now := time.Now()
	statusCode504 := 504

	tests := []struct {
		name        string
		prevMs      int64
		until       time.Time
		statusCode  *int
		expectedMin time.Duration
		expectedMax time.Duration
		description string
	}{
		{
			name:        "首次504错误应冷却1分钟",
			prevMs:      0,
			until:       time.Time{},
			statusCode:  &statusCode504,
			expectedMin: time.Minute,
			expectedMax: time.Minute,
			description: "504 Gateway Timeout should trigger 1-minute cooldown on first occurrence",
		},
		{
			name:        "连续504错误应指数退避",
			prevMs:      int64(time.Minute / time.Millisecond),
			until:       now.Add(time.Minute),
			statusCode:  &statusCode504,
			expectedMin: 2 * time.Minute,
			expectedMax: 2 * time.Minute,
			description: "Subsequent 504 errors should double the cooldown (1min -> 2min)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration := CalculateBackoffDuration(tt.prevMs, tt.until, now, tt.statusCode)

			if duration < tt.expectedMin || duration > tt.expectedMax {
				t.Errorf("%s\n期望冷却时间: %v-%v\n实际冷却时间: %v",
					tt.description, tt.expectedMin, tt.expectedMax, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_ChannelErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{500, time.Minute}, // Internal Server Error: 1min -> 2min -> 4min ...
		{502, time.Minute}, // Bad Gateway: 1min -> 2min -> 4min ...
		{503, time.Minute}, // Service Unavailable: 1min -> 2min -> 4min ...
		{504, time.Minute}, // Gateway Timeout: 1min -> 2min -> 4min ...
		{520, time.Minute}, // Web Server Returned an Unknown Error: 1min -> 2min -> 4min ...
		{521, time.Minute}, // Web Server Is Down: 1min -> 2min -> 4min ...
		{524, time.Minute}, // A Timeout Occurred: 1min -> 2min -> 4min ...
		{599, time.Minute}, // Stream Incomplete (内部状态码): 1min -> 2min -> 4min ...
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_AuthErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{401, 5 * time.Minute}, // Unauthorized
		{402, 5 * time.Minute}, // Payment Required
		{403, 5 * time.Minute}, // Forbidden
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("认证错误%d首次应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_OtherErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{429, time.Minute}, // Too Many Requests - 1分钟冷却
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_TimeoutError(t *testing.T) {
	now := time.Now()
	statusCode598 := 598

	duration := CalculateBackoffDuration(0, time.Time{}, now, &statusCode598)

	if duration != TimeoutErrorCooldown {
		t.Errorf("超时错误(598)应固定冷却%v，实际%v",
			TimeoutErrorCooldown, duration)
	}
}

func TestCalculateBackoffDuration_ExponentialBackoff(t *testing.T) {
	now := time.Now()
	statusCode := 500 // 使用服务器错误测试指数退避（1分钟起始）

	// 测试指数退避序列：1min -> 2min -> 4min -> 8min -> 15min(上限)
	expectedSequence := []time.Duration{
		time.Minute,      // 初始
		2 * time.Minute,  // 2x
		4 * time.Minute,  // 4x
		8 * time.Minute,  // 8x
		15 * time.Minute, // 达到上限
		15 * time.Minute, // 保持上限
	}

	prevMs := int64(0)
	for i, expected := range expectedSequence {
		duration := CalculateBackoffDuration(prevMs, time.Time{}, now, &statusCode)

		if duration != expected {
			t.Errorf("第%d次退避应为%v，实际%v", i+1, expected, duration)
		}

		prevMs = int64(duration / time.Millisecond)
	}
}
