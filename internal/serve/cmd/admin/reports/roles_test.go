package reports

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/stretchr/testify/require"
)

// isolateLockEnv mirrors the package cmd test helper of the same name:
// every env var the config path reads is pointed at throwaway directories,
// a synthetic zai provider is seeded, and the provider cache is reset. The
// once-only global logger is pre-initialised first (rushlog.Setup("", false)
// plus a discard slog default) so config.Init's setup path never opens a
// lumberjack handle inside the temp dirs, which would lock the files and
// break t.TempDir cleanup.
func isolateLockEnv(t *testing.T) (string, string, string) {
	t.Helper()
	global := t.TempDir()
	globalConfig := t.TempDir()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	// The package used to gate ktav auto-migration off via a root TestMain;
	// after the move it's per-test, so gate it here (restored via cleanup) to
	// keep fixtures as plain rush.json. Clear RUSH_GLOBAL_DIR too so a host
	// override can't divert the global root away from `global`.
	t.Setenv("RUSH_GLOBAL_DIR", "")
	t.Cleanup(config.SetKtavAutoMigrateForTest(false))
	t.Setenv("RUSH_GLOBAL_DATA", global)
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	require.NoError(t, os.WriteFile(filepath.Join(global, "rush.json"), []byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o600))
	config.ResetProviderCacheForTests()
	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	return global, workspace, dataDir
}

func runRolesTestCommand(t *testing.T, globalPath string, args ...string) (string, error) {
	t.Helper()
	command := newRolesCmd()
	command.PersistentFlags().String("data-dir", filepath.Dir(globalPath), "")
	command.SetContext(t.Context())
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestRolesCLISetShowUpdateAndRemove(t *testing.T) {
	globalDir, _, _ := isolateLockEnv(t)
	globalPath := filepath.Join(globalDir, "rush.json")
	output, err := runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3", "--tools", "[]", "--json")
	require.NoError(t, err, output)
	var view customRoleView
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Equal(t, "reader", view.Name)
	require.Equal(t, "global", view.Source)
	require.NotNil(t, view.Role.Tools)
	require.Empty(t, *view.Role.Tools)

	output, err = runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3@high", "--json")
	require.NoError(t, err, output)
	view = customRoleView{}
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Equal(t, "high", view.Role.ReasoningEffort)
	require.NotNil(t, view.Role.Tools, "changing only model/effort must not silently remove the cap")
	require.Empty(t, *view.Role.Tools)

	output, err = runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3", "--inherit-tools", "--json")
	require.NoError(t, err, output)
	view = customRoleView{}
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Nil(t, view.Role.Tools)
	output, err = runRolesTestCommand(t, globalPath, "show", "reader", "--scope", "global", "--json")
	require.NoError(t, err, output)
	var shown customRoleView
	require.NoError(t, json.Unmarshal([]byte(output), &shown))
	require.Equal(t, view, shown)

	output, err = runRolesTestCommand(t, globalPath, "remove", "reader", "--scope", "global", "--json")
	require.NoError(t, err, output)
	require.Contains(t, output, `"lower_scope_may_apply":true`)
	output, err = runRolesTestCommand(t, globalPath, "list", "--scope", "global", "--json")
	require.NoError(t, err, output)
	require.JSONEq(t, `[]`, output)
}

func TestRolesCLIInvalidInputDoesNotWrite(t *testing.T) {
	globalDir, _, _ := isolateLockEnv(t)
	globalPath := filepath.Join(globalDir, "rush.json")
	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	for _, args := range [][]string{
		{"set", "reader", "zai/glm-5.3", "--tools", "null"},
		{"set", "reader", "zai/glm-5.3", "--tools", `{"view":true}`},
		{"set", "reader", "zai/glm-5.3", "--tools", "[]", "--inherit-tools"},
		{"set", "smart", "zai/glm-5.3"},
		{"set", "reader", "zai/glm-5.3", "--scope", "bad"},
		{"set", "reader", "zai/glm-5.3", "--scope", ""},
		{"set", "reader", "zai/glm-5.3", "--scope", "global", "--local"},
	} {
		output, err := runRolesTestCommand(t, globalPath, args...)
		require.Error(t, err, output)
		after, err := os.ReadFile(globalPath)
		require.NoError(t, err)
		require.Equal(t, before, after, "invalid command must not write: %v", args)
	}
}
