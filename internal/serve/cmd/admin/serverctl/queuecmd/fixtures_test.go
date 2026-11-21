package queuecmd

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	cmdtest.Main(m)
}

func queueChild(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	child, _, err := root.Find([]string{name})
	require.NoError(t, err)
	require.Equal(t, name, child.Name())
	return child
}

// isolateConfigEnvForTests keeps config and provider caches off real paths.
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

func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

// waitForSQLiteHandleRelease retries removing the test data dir on Windows,
// where SQLite can leave a handle open briefly after shutdown.
func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()

	// 30s, not 10s: observed TestModelsBump_GLM52FullStepUp (which cycles
	// the DB open/close path multiple times, stepping a role through
	// several effort levels in one test) exceed a 10s budget under a cold
	// `go test` cache (every test binary recompiling at once — far heavier
	// transient CPU/IO load than a typical warm pre-push run).
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dataDir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		// Give up silently: t.TempDir()'s own cleanup will make one more
		// attempt (with its own 2s retry) and report the real error itself
		// if the handle is truly still held. We don't want to fail the
		// test here for a condition Go's own cleanup already surfaces
		// clearly.
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}
