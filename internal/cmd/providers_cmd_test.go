// providers show/list command tests running the real subcommand RunE
// through runProvidersCmdInIsolatedApp (isolated config fixture):
// peak-hours rendering in show, its omission without peak hours, and
// the list PEAK column.
package cmd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	crushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runProvidersCmdInIsolatedApp executes a real providers subcommand's RunE
// against an isolated config fixture, capturing real stdout. It stands up a
// full app via setupApp (the same path the CLI uses) in a temp data dir with
// network/provider-discovery disabled, so the output is produced by the real
// rendering code in providers.go — not a reimplementation.
//
// cmd is the real providersShowCmd/providersListCmd. providerJSON is the raw
// JSON for the "providers" object written into the isolated global crush.json
// before the command runs. args is the positional/flag payload parsed onto cmd
// (e.g. "with-peak" for show, "--json" for list).
func runProvidersCmdInIsolatedApp(t *testing.T, cmd *cobra.Command, providerJSON, args string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", tmp)
	// GlobalConfig() (CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (CRUSH_GLOBAL_DATA) above — see
	// CLAUDE.md's "two real config paths" caveat and the longer explanation
	// in isolatedModelsEnv (models_use_test.go). Without this, setupApp's
	// app.New() -> mcp.Initialize reads the real host
	// ~/.config/crush/crush.json and, if it configures MCP servers, tries to
	// open real network connections to them from inside the test — this is
	// the exact path that previously hung a stress run for 9+ minutes.
	//
	// Use a SEPARATE subdirectory from CRUSH_GLOBAL_DATA (not the same tmp)
	// so lookupConfigs (internal/config/load.go), which loads and merges
	// both GlobalConfig() and GlobalConfigData(), doesn't load the same file
	// path twice under two different env vars — see the "Low, latent"
	// duplicate-merge caveat fixed in isolatedModelsEnv.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	// Cache-only so provider discovery makes no network calls.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	// Pre-initialise the once-only global logger so setupApp's log.Setup
	// call is a no-op and does not open a lumberjack handle inside the
	// temp dir (which would lock the file and break t.TempDir cleanup).
	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalDataPath := filepath.Join(tmp, "crush.json")
	require.NoError(t, os.WriteFile(globalDataPath, []byte(providerJSON), 0o644))

	// setupApp reads debug/data-dir/cwd off the command it receives. Those
	// are normally rootCmd persistent flags; build a carrier command that
	// carries them so we can invoke the real subcommand's RunE directly.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		// setupApp opens a pooled SQLite connection under tmp; cancel ctx
		// and release THIS test's own connection so t.TempDir cleanup
		// doesn't hit a locked crush.db / crush.log on Windows.
		//
		// db.Release(tmp), not db.ResetPool(): this file alone has ~19
		// t.Parallel() tests sharing this helper. ResetPool() used to nuke
		// the ENTIRE process-wide connection pool, including any other
		// still-running parallel test's live connection to a different
		// data dir — real cross-test interference, not just OS-level
		// handle-release lag, and a genuine contributor to this package's
		// Windows-only "process cannot access the file" flakiness.
		cancel()
		_ = db.Release(tmp)
	})
	carrier := &cobra.Command{Use: "crush"}
	carrier.Flags().Bool("debug", false, "")
	carrier.Flags().String("data-dir", tmp, "")
	carrier.Flags().String("cwd", workDir, "")
	carrier.SetContext(ctx)

	// Reset the subcommand's own flags so state from a prior invocation in
	// the same process (e.g. a leftover --json) doesn't leak in.
	for _, fl := range []string{"json", "grep"} {
		if f := cmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	cmd.SetArgs(nil)

	var runArgs []string
	if args != "" {
		runArgs = strings.Fields(args)
	}
	require.NoError(t, cmd.ParseFlags(runArgs))

	// Capture os.Stdout — providers list/show write there directly.
	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	runErr := cmd.RunE(carrier, runArgs)

	_ = w.Close()
	os.Stdout = oldOut
	<-done

	require.NoError(t, runErr, "command RunE failed; stdout was:\n%s", buf.String())
	return buf.String()
}

const peakFixtureJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00"}
    },
    "no-peak": {
      "name": "No Peak",
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "models": [{"id": "claude-sonnet-4"}]
    }
  }
}`

func TestProvidersShow_PeakHoursRendering(t *testing.T) {
	// Regression: this previously reimplemented the peak-hours line inline
	// and asserted the duplicate against itself. It now runs the real
	// providersShowCmd.RunE and asserts on the actual emitted stdout.
	out := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak")

	assert.Contains(t, out, "id:          with-peak")
	assert.Contains(t, out, "peak hours:  09:00-18:00 (currently:")
	// The state must be one of the two real branches the command emits.
	assert.True(t, strings.Contains(out, "(currently: in peak)") || strings.Contains(out, "(currently: not in peak)"),
		"expected a real 'currently:' state in output:\n%s", out)
}

func TestProvidersShow_NoPeakHoursOmitsLine(t *testing.T) {
	// Regression: this previously only asserted p.PeakHours == nil without
	// running the command. It now runs the real providersShowCmd.RunE on a
	// provider without peak hours and asserts the line is absent.
	out := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "no-peak")

	assert.Contains(t, out, "id:          no-peak")
	assert.NotContains(t, out, "peak hours", "show must omit the peak-hours line when PeakHours is nil")
}

func TestProvidersList_PeakColumn(t *testing.T) {
	// Regression: this previously reimplemented the PEAK column rendering
	// inline. It now runs the real providersListCmd.RunE and asserts on the
	// actual table output.
	out := runProvidersCmdInIsolatedApp(t, providersListCmd, peakFixtureJSON, "")

	assert.Contains(t, out, "PEAK", "list header must include the PEAK column")
	assert.Contains(t, out, "with-peak", "with-peak row must be present")
	assert.Contains(t, out, "no-peak", "no-peak row must be present")
	// The with-peak row must show the window; the no-peak row must show the
	// em-dash placeholder used by the list command's real rendering.
	assert.Contains(t, out, "09:00-18:00", "with-peak PEAK cell must show the window")
	assert.Contains(t, out, "—", "no-peak PEAK cell must show the placeholder")
}
