package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// TestCacheIsolation_SnapshotRefresh 验证 COW 快照刷新后返回新数据
// COW 设计：同一快照内返回共享引用（零拷贝），刷新后原子替换为新快照
func TestCacheIsolation_SnapshotRefresh(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 创建测试渠道
	cfg := &model.Config{
		Name:           "test-channel",
		URL:            "https://test.example.com",
		Priority:       10,
		DailyCostLimit: 2.0,
		ModelEntries: []model.ModelEntry{
			{Model: "model-1", RedirectModel: ""},
			{Model: "model-2", RedirectModel: ""},
			{Model: "alias-1", RedirectModel: "model-1"},
		},
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 用极短 TTL 确保测试中能触发刷新
	cache := storage.NewChannelCache(store, 10*time.Millisecond)

	// 首次查询
	channels1, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel 失败: %v", err)
	}
	if len(channels1) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels1))
	}

	// 验证字段完整
	ch1 := channels1[0]
	if ch1.Name != "test-channel" {
		t.Errorf("Name 不对: 期望 'test-channel', 实际 %q", ch1.Name)
	}
	if ch1.Priority != 10 {
		t.Errorf("Priority 不对: 期望 10, 实际 %d", ch1.Priority)
	}
	if ch1.DailyCostLimit != 2.0 {
		t.Errorf("DailyCostLimit 不对: 期望 2.0, 实际 %v", ch1.DailyCostLimit)
	}
	if len(ch1.ModelEntries) != 3 {
		t.Errorf("ModelEntries 长度不对: 期望 3, 实际 %d", len(ch1.ModelEntries))
	}

	// 验证 modelIndex 预构建可用（COW 快照在刷新时预构建索引）
	redirect, hasRedirect := ch1.GetRedirectModel("alias-1")
	if !hasRedirect || redirect != "model-1" {
		t.Errorf("GetRedirectModel 不对: redirect=%q, hasRedirect=%v", redirect, hasRedirect)
	}

	// 修改数据库（不能直接 *created 因为 Config 含 sync.RWMutex）
	updated := &model.Config{
		ID:             created.ID,
		Name:           "updated-channel",
		ChannelType:    created.ChannelType,
		URL:            created.URL,
		Priority:       99,
		Enabled:        created.Enabled,
		DailyCostLimit: created.DailyCostLimit,
		ModelEntries:   created.ModelEntries,
	}
	if _, err := store.UpdateConfig(ctx, created.ID, updated); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}

	// 等 TTL 过期触发刷新
	time.Sleep(20 * time.Millisecond)

	// 再次查询，应该拿到新数据
	channels2, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
	if err != nil {
		t.Fatalf("第二次 GetEnabledChannelsByModel 失败: %v", err)
	}
	if len(channels2) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels2))
	}
	ch2 := channels2[0]
	if ch2.Name != "updated-channel" {
		t.Errorf("刷新后 Name 不对: 期望 'updated-channel', 实际 %q", ch2.Name)
	}
	if ch2.Priority != 99 {
		t.Errorf("刷新后 Priority 不对: 期望 99, 实际 %d", ch2.Priority)
	}

	t.Logf("COW 快照刷新测试通过")
}

// TestCacheIsolation_GetEnabledChannelsByType 验证类型查询
func TestCacheIsolation_GetEnabledChannelsByType(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation_type.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	cfg := &model.Config{
		Name:           "test-anthropic",
		ChannelType:    "anthropic",
		URL:            "https://test.example.com",
		Priority:       10,
		DailyCostLimit: 2.0,
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-sonnet", RedirectModel: ""},
			{Model: "claude", RedirectModel: "claude-3-sonnet"},
		},
		Enabled: true,
	}
	_, err = store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	channels, err := cache.GetEnabledChannelsByType(ctx, "anthropic")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByType 失败: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels))
	}
	ch := channels[0]
	if len(ch.ModelEntries) != 2 {
		t.Errorf("ModelEntries 长度不对: 期望 2, 实际 %d", len(ch.ModelEntries))
	}
	if ch.DailyCostLimit != 2.0 {
		t.Errorf("DailyCostLimit 不对: 期望 2.0, 实际 %v", ch.DailyCostLimit)
	}

	t.Logf("GetEnabledChannelsByType 测试通过")
}

// TestCacheIsolation_WildcardQuery 验证通配符查询
func TestCacheIsolation_WildcardQuery(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation_wildcard.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     "wildcard-test-" + string(rune('A'+i-1)),
			URL:      "https://test.example.com",
			Priority: i * 10,
			ModelEntries: []model.ModelEntry{
				{Model: "model-common", RedirectModel: ""},
			},
			Enabled: true,
		}
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建渠道 %d 失败: %v", i, err)
		}
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	channels, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("通配符查询失败: %v", err)
	}
	if len(channels) != 3 {
		t.Fatalf("期望3个渠道，实际 %d 个", len(channels))
	}

	for _, ch := range channels {
		if len(ch.ModelEntries) != 1 || ch.ModelEntries[0].Model != "model-common" {
			t.Errorf("渠道 %s ModelEntries 异常: %v", ch.Name, ch.ModelEntries)
		}
	}

	t.Logf("通配符查询测试通过")
}
