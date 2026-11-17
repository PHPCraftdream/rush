package sessaudit

// Test fixtures byte-copied from the root cmd package (rule 18): helpers the
// moved audit tests need on BOTH sides — the staying root tests keep their
// own untouched copies.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/stretchr/testify/require"
)

// isolateConfigEnvForTests sets XDG_DATA_HOME/RUSH_GLOBAL_DATA,
// XDG_CONFIG_HOME/RUSH_GLOBAL_CONFIG, and RUSH_PROVIDER_CACHE_ONLY to
// throwaway directories under a fresh t.TempDir(), and points the default
// slog logger at io.Discard — the isolation this repo's CLAUDE.md documents
// as mandatory (not optional) for any test that reaches config.Load /
// config.Init / config.ResolveDataDirectory, since those read real config
// paths from the environment (GlobalConfig / GlobalConfigData) unless
// explicitly isolated. Without this, a test can silently read from or write
// to the operator's real global config/data — confirmed to have happened
// before.
//
// Returns the base tmp dir so callers can derive their own subdirectories
// (e.g. a configured --data-dir that lives outside it, or a workDir to
// os.Chdir into) without colliding with the env dirs this helper already
// created.
func isolateConfigEnvForTests(t *testing.T) (tmp string) {
	t.Helper()
	config.ResetProviderCacheForTests()

	tmp = t.TempDir()

	dataDir := filepath.Join(tmp, "global-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)

	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	return tmp
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// short mirrors the root cmd package's short (cmd_bridge.go): the moved
// audit tests reference it, and the audit report prints truncated session
// ids the same way.
func short(hash string) string { return cliout.Short(hash) }
