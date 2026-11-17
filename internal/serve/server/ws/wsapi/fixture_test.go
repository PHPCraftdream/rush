package wsapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/store/db"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/stretchr/testify/require"
)

// isolateAllGlobalConfigPaths redirects every environment variable that
// config.GlobalConfig()/config.GlobalConfigData() (and the XDG fallbacks
// they defer to) can resolve through, so config.Init below can never read
// or merge the operator's real ~/.config/rush/rush.json (live API keys,
// MCP server definitions) nor the real global data-dir rush.json into the
// App under test — see internal/cmd/providers_test.go's
// runProvidersCmdInIsolatedApp for the concrete failure mode this guards
// against (a leaked real MCP config made app.New's mcp.Initialize hang on a
// real network connection for 9+ minutes inside a test).
//
// This is a package-local twin of internal/config's and internal/agent's
// identically-named helpers: the logic must stay identical, but it can't be
// shared as a single file because these are three separate Go packages and
// all three copies are test-only (no non-test importer to hang a shared
// helper off of).
func isolateAllGlobalConfigPaths(t *testing.T) {
	t.Helper()

	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "global-config")
	dataDir := filepath.Join(tmp, "global-data")
	skillsDir := filepath.Join(tmp, "global-skills")

	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	// The process-wide heartbeat writer must not inherit a real output dir.
	t.Setenv("RUSH_HEARTBEAT_DIR", "")

	// Without this, provider discovery makes a real network call to Catwalk
	// (and Hyper) the first time it runs against a fresh, cache-empty
	// isolated data dir — see internal/cmd/providers_test.go's identical
	// use of this env var for the same reason.
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// If a local CLI (claude/gemini/...) happens to be on this machine's
	// PATH, config.Load auto-synthesizes a "local-cli" provider, which
	// makes cfg.IsConfigured() true and drives app.New into
	// InitCoderAgent -> NewCoordinator -> discoverSkills. Without this
	// override that reads the OPERATOR's real
	// ~/.config/rush/skills / ~/.claude/skills, making the test
	// non-deterministic across machines and leaking real filesystem state
	// into a test that has nothing to do with skills.
	t.Setenv("RUSH_SKILLS_DIR", skillsDir)
}

// forceLocalCLIProviderForTest deterministically satisfies the
// IsConfigured() precondition appPkg.New needs to build a real
// AgentCoordinator, by stubbing cliprovider's PATH probe to always report
// one spec. isolateAllGlobalConfigPaths deliberately does NOT isolate PATH,
// so whether appPkg.New constructs a coordinator at all currently depends on
// whether the machine running the test happens to have claude/gemini/codex/
// qwen on its ambient PATH (config.Load auto-synthesizes the local-cli
// provider from that probe, which is the only provider that can exist under
// full global-config isolation). That made the test's precondition vary
// across CI runner images — observed as a macos-latest flake where appPkg.New
// returned no error but a nil coordinator. The stub returns All[0], a real
// spec, so the provider client built from cliprovider.All can resolve the
// auto-selected model; the CLI binary itself is never executed by tests
// using this helper. Must be called before config.Init runs (i.e. before
// newAttachmentsTestApp) and only from sequential tests — which every
// caller is, since newAttachmentsTestApp calls t.Setenv.
func forceLocalCLIProviderForTest(t *testing.T) {
	t.Helper()
	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec {
		return []cliprovider.CLISpec{cliprovider.All[0]}
	}
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })
}

// newAttachmentsTestApp builds a real *appPkg.App over a config.Init'd temp
// workingDir/dataDir pair, isolated from the host's real global config.
// dataDir may be "" to exercise config's own default-resolution path (see
// setDefaults); the resolved directory (store.Config().Options.DataDirectory)
// is what actually gets used for the DB connection, not the raw parameter.
func newAttachmentsTestApp(t *testing.T, workingDir, dataDir string) *appPkg.App {
	t.Helper()
	isolateAllGlobalConfigPaths(t)

	store, err := config.Init(workingDir, dataDir, false, config.WithCLIModels(cliprovider.AvailableCLIModels))
	require.NoError(t, err)
	resolvedDataDir := store.Config().Options.DataDirectory
	require.NotEmpty(t, resolvedDataDir)
	// Mirrors setupApp's createDotRushDir (internal/cmd/root.go): db.Connect
	// expects the data directory to already exist.
	require.NoError(t, os.MkdirAll(resolvedDataDir, 0o700))

	conn, err := db.Connect(t.Context(), resolvedDataDir)
	require.NoError(t, err)
	// ReleaseAll, not Release: App.Shutdown's forced-shutdown path (taken
	// whenever an agent doesn't finish within its grace period — several
	// tests using this helper deliberately install a coordinator that never
	// stops being busy, to test fails-closed behavior) intentionally skips
	// calling Release at all, so a single paired Release call here can leave
	// the pool entry's refCount above zero forever once app.New's own
	// internal Connect/ConnectRead calls are counted — silently leaking the
	// DB file handle for the rest of this test binary's life and breaking
	// t.TempDir()'s own cleanup on Windows (found investigating a real,
	// consistently-reproducing pre-push CI failure). ReleaseAll guarantees
	// this dataDir's connection is fully closed regardless of how Shutdown
	// behaved — see its own doc for why this is safe under t.Parallel().
	t.Cleanup(func() { _ = db.ReleaseAll(resolvedDataDir) })
	// Forced shutdown pins host handles; LIFO must release them after Shutdown
	// and before DB/TempDir cleanup.
	t.Cleanup(func() { sessionhosts.ReleaseRetainedHostLocksForTest() })

	a, err := appPkg.New(t.Context(), conn, store)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)
	original := a.AgentCoordinator
	// Retain startup ownership even when a test replaces the coordinator.
	t.Cleanup(func() {
		if original != nil {
			original.CancelAll()
			if ctrl, ok := original.(agent.WakeScheduleController); ok {
				ctrl.StopWakeScheduler()
			}
		}
	})

	return a
}

// drainSessionsListReplies collects every sessions_list reply currently
// buffered on the client's send channel. handleListSessions may also emit
// agent_busy / summarize_queued corrections when a coordinator is
// configured, so the replies are filtered by type.
func drainSessionsListReplies(t *testing.T, client *Client) [][]core.Session {
	t.Helper()
	var out [][]core.Session
	for {
		select {
		case raw := <-client.SendChan():
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionsList {
				continue
			}
			var sessions []core.Session
			require.NoError(t, json.Unmarshal(env.Payload, &sessions))
			out = append(out, sessions)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

// findSessionRow returns the row for sessionID from a sessions_list reply.
func findSessionRow(t *testing.T, sessions []core.Session, sessionID string) core.Session {
	t.Helper()
	for _, s := range sessions {
		if s.ID == sessionID {
			return s
		}
	}
	t.Fatalf("session %s missing from the sessions_list reply", sessionID)
	return core.Session{}
}

// blockingCoordinator is referenced by the package-local coordinator mocks;
// its behavioral implementation remains with the read-pump tests in server.
type blockingCoordinator struct{}
