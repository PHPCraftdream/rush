package sdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestClientCloseIsIdempotent and TestClientCloseNilReceiverAndNilApp
// live in close_internal_test.go (package sdk): they construct a Client
// around a stub Coordinator through the unexported app field, and
// sdk.Wrap — the old external shortcut for that — was removed from the
// public API (review R1-7).

// TestOpenAfterCloseOnNewWorkingDir pins that a second sdk.Open in the
// same process, after a graceful Close, works — no leftover process-wide
// blocking; the documented v1 MCP sync.Once limitation is accepted.
func TestOpenAfterCloseOnNewWorkingDir(t *testing.T) {
	// Isolate global config/data resolution so config.Init inside
	// sdk.Open reads only the rush.json written below into the temp
	// working directory — never the operator's real global config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// Open must not dial the provider (discovery is off in the config
	// below); the server exists only so base_url is a well-formed URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	writeRushJSON := func(dir string) {
		content := fmt.Sprintf(`{"disable_default_providers": true, "providers": {"probe": {"id": "probe", "name": "probe", "type": "openai-compat", "base_url": %q, "api_key": "probe", "discover_models": false, "models": [{"id": "probe", "name": "probe", "context_window": 200000, "default_max_tokens": 1000}]}}, "models": {"smart": {"provider": "probe", "model": "probe"}, "fast": {"provider": "probe", "model": "probe"}}}`, srv.URL)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.json"), []byte(content), 0o644))
	}

	workDir1 := t.TempDir()
	writeRushJSON(workDir1)
	client1, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir1})
	require.NoError(t, err)

	res1 := client1.Close()
	require.False(t, res1.Forced)
	require.Empty(t, res1.CleanupErrors)

	// A second Open after the graceful Close must not be blocked by
	// leftover state from client1.
	workDir2 := t.TempDir()
	writeRushJSON(workDir2)
	client2, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir2})
	require.NoError(t, err)

	res2 := client2.Close()
	require.False(t, res2.Forced)
	require.Empty(t, res2.CleanupErrors)
}
