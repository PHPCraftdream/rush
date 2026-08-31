package sdk_test

// Regression tests for Open's WorkingDir handling: it stays a required
// parameter (no default/fallback to process cwd or a temp dir), but a
// directory that does not exist yet is created rather than rejected —
// the common case of a host provisioning a fresh per-tenant workspace
// before ever calling Open on it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

func isolateGlobalConfigForWorkdirTest(t *testing.T) {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
}

func TestOpen_CreatesWorkingDirWhenMissing(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	parent := t.TempDir()
	workDir := filepath.Join(parent, "fresh-tenant-workspace")
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %q must not exist yet", workDir)
	}

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	info, statErr := os.Stat(workDir)
	require.NoError(t, statErr, "Open must create the missing working directory")
	require.True(t, info.IsDir())
}

func TestOpen_RejectsWorkingDirThatIsAFile(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	parent := t.TempDir()
	filePath := filepath.Join(parent, "not-a-directory")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o644))

	_, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: filePath})
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a directory")
}

func TestOpen_RequiresWorkingDir(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	_, err := sdk.Open(context.Background(), sdk.Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Options.WorkingDir is required")
}
