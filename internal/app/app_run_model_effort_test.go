// An explicit per-call --effort must travel on the model override of the slot
// it targets: the session row is written before the model slot is persisted,
// so coordinator.RunWithOverrides cannot rely on the row to carry it (#1230).

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// newModelEffortApp builds a real App over a throwaway data dir with one
// provider offering a default and an alternative model per slot.
func newModelEffortApp(t *testing.T) *App {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000,"default_reasoning_effort":"medium"},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":"smart-b","context_window":200000,"default_max_tokens":1000,"default_reasoning_effort":"low"},
        {"id":"fast-b","context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"}
  }
}`, "http://127.0.0.1:1")
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	store.SetupAgents()

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	application, err := New(t.Context(), conn, store)
	if err != nil {
		db.ReleaseConn(conn)
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}

// preparedOverrides runs the request-to-config setup of ExecuteRun and
// returns the model overrides the turn will be built from.
func preparedOverrides(t *testing.T, app *App, ov RunOverrides) (smart, fast *agent.ModelOverride) {
	t.Helper()
	_, cancel, setup, err := app.prepareExecuteRun(t.Context(), RunRequest{Prompt: "p", Overrides: ov, HideSpinner: true})
	require.NoError(t, err)
	t.Cleanup(cancel)
	return setup.smartOverride, setup.fastOverride
}

// REVERT CHECK: dropping the effort assignment in prepareExecuteRun leaves the
// override's effort empty -- the turn then runs without the requested effort.
func TestPrepareExecuteRun_ExplicitEffortRidesTheSmartOverride(t *testing.T) {
	app := newModelEffortApp(t)

	smart, fast := preparedOverrides(t, app, RunOverrides{SmartModel: "smart-b", ReasoningEffort: "high", RoleSmart: true})

	require.NotNil(t, smart)
	require.Equal(t, "smart-b", smart.Model)
	require.Equal(t, "high", smart.ReasoningEffort)
	require.Nil(t, fast)
}

func TestPrepareExecuteRun_ExplicitEffortRidesTheFastOverride(t *testing.T) {
	app := newModelEffortApp(t)

	smart, fast := preparedOverrides(t, app, RunOverrides{FastModel: "fast-b", ReasoningEffort: "low", RoleSmart: false})

	require.NotNil(t, fast)
	require.Equal(t, "fast-b", fast.Model)
	require.Equal(t, "low", fast.ReasoningEffort)
	require.Nil(t, smart)
}

// The effort belongs to the targeted slot only, and without --effort nothing is
// invented: #1221's rule (no effort across a model switch unless explicit).
func TestPrepareExecuteRun_EffortIsExplicitAndSlotScoped(t *testing.T) {
	app := newModelEffortApp(t)

	smart, _ := preparedOverrides(t, app, RunOverrides{SmartModel: "smart-b", RoleSmart: true})
	require.NotNil(t, smart)
	require.Empty(t, smart.ReasoningEffort, "no --effort: the override carries none")

	smart, fast := preparedOverrides(t, app, RunOverrides{SmartModel: "smart-b", FastModel: "fast-b", ReasoningEffort: "high", RoleSmart: true})
	require.Equal(t, "high", smart.ReasoningEffort)
	require.Empty(t, fast.ReasoningEffort, "the effort targets the smart slot, not the fast one")
}
