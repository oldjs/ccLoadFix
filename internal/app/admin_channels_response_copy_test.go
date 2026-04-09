package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

type sharedConfigStore struct {
	storage.Store
	listConfig *model.Config
	getConfig  *model.Config
}

func (s *sharedConfigStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	if s.listConfig != nil {
		return []*model.Config{s.listConfig}, nil
	}
	return s.Store.ListConfigs(ctx)
}

func (s *sharedConfigStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	if s.getConfig != nil && s.getConfig.ID == id {
		return s.getConfig, nil
	}
	return s.Store.GetConfig(ctx, id)
}

func TestHandleListChannels_DoesNotMutateSharedConfig(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "shared-list",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	sharedCfg, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}

	server.store = &sharedConfigStore{Store: store, listConfig: sharedCfg}
	server.channelCache = storage.NewChannelCache(server.store, time.Minute)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("handleListChannels failed: %d", w.Code)
	}

	if sharedCfg.ModelEntries[0].RedirectModel != "" {
		t.Fatalf("expected shared config untouched, got redirect_model=%q", sharedCfg.ModelEntries[0].RedirectModel)
	}
}

func TestHandleGetChannel_DoesNotMutateSharedConfig(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "shared-get",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	sharedCfg, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}

	server.store = &sharedConfigStore{Store: store, getConfig: sharedCfg}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+strconv.FormatInt(created.ID, 10), nil))
	server.handleGetChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("handleGetChannel failed: %d", w.Code)
	}

	if sharedCfg.ModelEntries[0].RedirectModel != "" {
		t.Fatalf("expected shared config untouched, got redirect_model=%q", sharedCfg.ModelEntries[0].RedirectModel)
	}
}
