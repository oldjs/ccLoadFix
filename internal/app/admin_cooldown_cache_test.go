package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestHandleSetChannelCooldown_InvalidatesCooldownCache(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "channel-cooldown",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if _, err := server.getAllChannelCooldowns(ctx); err != nil {
		t.Fatalf("prime channel cooldown cache failed: %v", err)
	}

	reqBody := CooldownRequest{DurationMs: 2 * 60 * 1000}
	c1, w1 := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+strconv.FormatInt(created.ID, 10)+"/cooldown", reqBody))
	c1.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleSetChannelCooldown(c1)
	if w1.Code != http.StatusOK {
		t.Fatalf("HandleSetChannelCooldown failed: %d body=%s", w1.Code, w1.Body.String())
	}

	c2, w2 := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c2)
	if w2.Code != http.StatusOK {
		t.Fatalf("handleListChannels failed: %d", w2.Code)
	}

	resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w2.Body.Bytes())
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(resp.Data))
	}
	if resp.Data[0].CooldownUntil == nil || resp.Data[0].CooldownRemainingMS <= 0 {
		t.Fatalf("expected fresh channel cooldown, got until=%v remaining=%d", resp.Data[0].CooldownUntil, resp.Data[0].CooldownRemainingMS)
	}
}

func TestHandleSetKeyCooldown_InvalidatesRelatedCaches(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "key-cooldown",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-test-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	if _, err := server.getAPIKeys(ctx, created.ID); err != nil {
		t.Fatalf("prime api keys cache failed: %v", err)
	}
	if _, err := server.getAllKeyCooldowns(ctx); err != nil {
		t.Fatalf("prime key cooldown cache failed: %v", err)
	}

	reqBody := CooldownRequest{DurationMs: 2 * 60 * 1000}
	c1, w1 := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+strconv.FormatInt(created.ID, 10)+"/keys/0/cooldown", reqBody))
	c1.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}, {Key: "keyIndex", Value: "0"}}
	server.HandleSetKeyCooldown(c1)
	if w1.Code != http.StatusOK {
		t.Fatalf("HandleSetKeyCooldown failed: %d body=%s", w1.Code, w1.Body.String())
	}

	c2, w2 := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+strconv.FormatInt(created.ID, 10)+"/keys", nil))
	server.handleGetChannelKeys(c2, created.ID)
	if w2.Code != http.StatusOK {
		t.Fatalf("handleGetChannelKeys failed: %d", w2.Code)
	}
	var apiKeys []*model.APIKey
	mustUnmarshalAPIResponseData(t, w2.Body.Bytes(), &apiKeys)
	if len(apiKeys) != 1 || apiKeys[0].CooldownUntil == 0 {
		t.Fatalf("expected key list to show fresh cooldown, got %+v", apiKeys)
	}

	c3, w3 := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c3)
	if w3.Code != http.StatusOK {
		t.Fatalf("handleListChannels failed: %d", w3.Code)
	}
	resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w3.Body.Bytes())
	if len(resp.Data) != 1 || len(resp.Data[0].KeyCooldowns) != 1 {
		t.Fatalf("unexpected channel list payload: %+v", resp.Data)
	}
	if resp.Data[0].KeyCooldowns[0].CooldownUntil == nil || resp.Data[0].KeyCooldowns[0].CooldownRemainingMS <= 0 {
		t.Fatalf("expected fresh key cooldown in list, got %+v", resp.Data[0].KeyCooldowns[0])
	}
}

func TestHandleClearChannelAllCooldowns_ClearsByKeyIndex(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.cooldownManager = cooldown.NewManager(store, server)

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "clear-all-cooldowns",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    2,
		APIKey:      "sk-two",
		KeyStrategy: model.KeyStrategySequential,
	}, {
		ChannelID:   created.ID,
		KeyIndex:    7,
		APIKey:      "sk-seven",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	until := time.Now().Add(5 * time.Minute)
	if err := store.SetKeyCooldown(ctx, created.ID, 2, until); err != nil {
		t.Fatalf("SetKeyCooldown #2 failed: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, created.ID, 7, until); err != nil {
		t.Fatalf("SetKeyCooldown #7 failed: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/"+strconv.FormatInt(created.ID, 10)+"/cooldowns", nil))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleClearChannelAllCooldowns(c)
	if w.Code != http.StatusOK {
		t.Fatalf("HandleClearChannelAllCooldowns failed: %d body=%s", w.Code, w.Body.String())
	}

	keyCooldowns, err := store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns failed: %v", err)
	}
	if len(keyCooldowns[created.ID]) != 0 {
		t.Fatalf("expected sparse key cooldowns cleared, got %+v", keyCooldowns[created.ID])
	}
}
