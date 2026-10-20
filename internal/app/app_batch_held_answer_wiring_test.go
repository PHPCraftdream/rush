package app

// Held-question inject bridge wiring oracles: the sink exists exactly when
// a flag-ON install produced a live Runner, and the shutdown release gate
// closes and clears it. Replaces the coordinator-level tests' coverage of
// App-side install (they build their own sink by design). Uses the existing
// f6BatchOn() dual-mode helper (batch_fixture_port_f6_test.go) for the
// mode assertion; the flag itself is selected per test with t.Setenv, like
// every other flagged app test. No t.Parallel in this file (t.Setenv).
import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBatchHeldAnswerWiring_FlagOnInstallsSinkAndShutdownClears: flag ON +
// installBatchRuntime with a live Runner -> non-nil sink; ShutdownWithResult
// (which runs releaseBatchRuntimeGate) -> sink closed and cleared.
// Mutation caught: deleting the App-side sink install leaves the field nil
// and the first assertion fails; deleting the release-gate clear leaves it
// non-nil after shutdown and the last assertion fails.
func TestBatchHeldAnswerWiring_FlagOnInstallsSinkAndShutdownClears(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, f6BatchOn(), "the dual-mode helper reads the same flag")
	a, _, _ := newBatchMaintenanceApp(t)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	require.NotNil(t, a.batchRuntime, "the runtime installed")
	require.NotNil(t, a.batchHeldAnswerSink,
		"flag ON with a live Runner installs the inject bridge sink")
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	result := a.ShutdownWithResult()
	require.False(t, result.Forced)
	require.Nil(t, a.batchHeldAnswerSink, "the release gate cleared the sink")
}

// TestBatchHeldAnswerWiring_FlagOffInstallsNothing: flag OFF -> no runtime
// and no sink (the inject_agent legacy path stays byte-identical).
func TestBatchHeldAnswerWiring_FlagOffInstallsNothing(t *testing.T) {
	unsetBatchRuntimeEnv(t)
	require.False(t, f6BatchOn(), "the dual-mode helper reads the same flag")
	a := newFullNewApp(t)
	require.Nil(t, a.batchRuntime)
	require.Nil(t, a.batchHeldAnswerSink, "flag OFF installs no sink")
	require.False(t, a.ShutdownWithResult().Forced)
}
