// D6 (docs/plans/2026-10-15-role-models-hot-reload.md): SDK application mode
// runs the OAuth disk-sync poller through the same App-owned lifecycle as
// rush (web) and rush run — openApplication's app.New starts it after
// InitCoderAgent, and Close's shutdown stops and joins it. Internal test
// file (package sdk) because the poller is observed through the unexported
// Client.app field.

package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenApplicationModeStartsAndCloseStopsOAuthDiskSync(t *testing.T) {
	// Isolate BOTH global config paths before Open reads the host config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	workDir := t.TempDir()
	content := fmt.Sprintf(`{"disable_default_providers": true, "providers": {"copilot": {"id": "copilot", "name": "copilot", "type": "openai-compat", "base_url": %q, "api_key": "probe", "discover_models": false, "oauth": {"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_at": %d}, "models": [{"id": "probe", "name": "probe", "context_window": 200000, "default_max_tokens": 1000}]}}, "models": {"smart": {"provider": "copilot", "model": "probe"}, "fast": {"provider": "copilot", "model": "probe"}}}`, srv.URL, time.Now().Add(time.Hour).Unix())
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(content), 0o644))

	client, err := Open(context.Background(), Options{WorkingDir: workDir})
	require.NoError(t, err)
	// Failure-path cleanup; the explicit Close below is idempotent with it.
	t.Cleanup(func() { client.Close() })
	require.NotNil(t, client.app)
	require.True(t, client.app.Store().OAuthDiskSyncRunning(), "SDK application mode must start the OAuth disk-sync poller")

	res := client.Close()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)
	require.False(t, client.app.Store().OAuthDiskSyncRunning(), "SDK Close must stop the OAuth disk-sync poller")
}
