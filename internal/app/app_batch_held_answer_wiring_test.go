package app

// Held-question inject bridge wiring oracles: the sink exists exactly when
// the batch install produced a live Runner, and the shutdown release gate
// closes and clears it. Replaces the coordinator-level tests' coverage of
// App-side install (they build their own sink by design). Batch-only (H4
// atomic activation): the install is unconditional for every persisted,
// agent-setup App, and library-mode Apps (SkipAgentSetup / no data dir)
// install nothing. No t.Parallel in this file (shared package state).
import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBatchHeldAnswerWiring_InstallInstallsSinkAndShutdownClears: the batch
// install with a live Runner -> non-nil sink; ShutdownWithResult (which runs
// releaseBatchRuntimeGate) -> sink closed and cleared.
// Mutation caught: deleting the App-side sink install leaves the field nil
// and the first assertion fails; deleting the release-gate clear leaves it
// non-nil after shutdown and the last assertion fails.
func TestBatchHeldAnswerWiring_InstallInstallsSinkAndShutdownClears(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	require.NotNil(t, a.batchRuntime, "the runtime installed")
	require.NotNil(t, a.batchHeldAnswerSink,
		"a batch install with a live Runner installs the inject bridge sink")
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	result := a.ShutdownWithResult()
	require.False(t, result.Forced)
	require.Nil(t, a.batchHeldAnswerSink, "the release gate cleared the sink")
}

// TestBatchHeldAnswerWiring_LibraryModeInstallsNothing: library-mode New
// (SkipAgentSetup) -> no runtime and no sink (the legacy
// inject_agent path was removed with the old runtime; library-mode Apps
// carry no batch infrastructure at all). Obsolete flag-OFF contract
// translated to the library-mode shape.
func TestBatchHeldAnswerWiring_LibraryModeInstallsNothing(t *testing.T) {
	a := newFullNewAppSkipAgentSetup(t)
	require.Nil(t, a.batchRuntime)
	require.Nil(t, a.batchHeldAnswerSink, "library-mode New installs no sink")
	require.False(t, a.ShutdownWithResult().Forced)
}
