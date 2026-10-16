// D6 (docs/plans/2026-10-15-role-models-hot-reload.md): the OAuth disk-sync
// poller and its journal workers run under the App-owned lifecycle — started
// by New right after InitCoderAgent, stopped and joined synchronously by
// releaseResources before the parallel cleanupFuncs batch.

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestAppNewStartsAndShutdownStopsOAuthDiskSync(t *testing.T) {
	// Isolate BOTH global config paths (CLAUDE.md: RUSH_GLOBAL_DATA and
	// RUSH_GLOBAL_CONFIG are separate resolutions) before app.New reads
	// the host config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("copilot", config.ProviderConfig{
		ID:      "copilot",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		OAuthToken: &oauth.Token{
			AccessToken:  "fixture-access",
			RefreshToken: "fixture-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		},
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Runs after the Shutdown cleanup below (LIFO).
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	// Failure-path cleanup; the explicit Shutdown below is idempotent with it.
	t.Cleanup(application.Shutdown)
	require.NotNil(t, application.AgentCoordinator)
	require.True(t, store.OAuthDiskSyncRunning(), "App.New must start the OAuth disk-sync poller after InitCoderAgent")

	result := application.ShutdownWithResult()
	require.False(t, result.Forced)
	require.Empty(t, result.CleanupErrors)
	require.False(t, store.OAuthDiskSyncRunning(), "App shutdown must stop the OAuth disk-sync poller")
}
