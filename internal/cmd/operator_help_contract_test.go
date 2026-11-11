package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchHelp_DescribesInputAndControlBoundaries(t *testing.T) {
	run := oneLine(batchRunCmd.Long)
	require.Contains(t, run, "JSON or Ktav")
	require.Contains(t, run, "in this process")
	require.Contains(t, run, "no answering surface")
	require.Contains(t, run, "served intake")
	require.Contains(t, run, "There is no detached execution")
	require.Contains(t, oneLine(batchRunCmd.Flags().Lookup("spec").Usage), "JSON or Ktav")
	require.Contains(t, oneLine(batchAddCmd.Flags().Lookup("spec").Usage), "JSON or Ktav")
	require.Contains(t, oneLine(batchRunCmd.Flags().Lookup("detach").Usage), "Unsupported")

	add := oneLine(batchAddCmd.Long)
	require.Contains(t, add, "owner-process-only")
	require.Contains(t, add, "separate rush CLI process cannot access the owner's runtime")

	stop := oneLine(batchStopCmd.Long)
	require.Contains(t, stop, "By default the result is queued")
	require.Contains(t, stop, "--wait (up to 30s)")
	require.Contains(t, stop, "confirms the request was accepted, not that the node or process has terminated")
	require.Contains(t, stop, "rush batch show")
}

func TestSessionsWhyHelp_UsesCurrentActivityAndBatchEvidence(t *testing.T) {
	long := oneLine(sessionsWhyCmd.Long)
	require.Contains(t, long, "session lock")
	require.Contains(t, long, "host lock")
	require.Contains(t, long, ".rush/locks")
	require.Contains(t, long, ".rush/hosts")
	require.Contains(t, long, "shared session-activity classifier")
	require.Contains(t, long, "does not use the legacy async-job ledger")
	require.Contains(t, long, "batch unknown")
	require.Contains(t, long, "incomplete batch read")
	require.Contains(t, long, "end_turn")
	require.Contains(t, long, "not by itself a session-end or task-completion signal")
	require.Contains(t, long, "only ended_reason records that the run ended")
	require.Contains(t, long, "stale/dead recorded PID is only an annotation")
	require.NotContains(t, sessionsWhyCmd.Long, `\n`)
	require.NotContains(t, long, "last assistant message finished with end_turn")
}

func TestSessionsLocksHelp_DistinguishesTurnLocksFromRunAndHostActivity(t *testing.T) {
	long := oneLine(sessionsLocksCmd.Long)
	require.Contains(t, long, ".rush/locks")
	require.Contains(t, long, "initial pulse is based on lock-file mtime")
	require.Contains(t, long, "alive through 10s, ping through 15s, stopping through 20s, then offline")
	require.Contains(t, long, "Mtime age is not run duration")
	require.Contains(t, long, "A session lock is held only during a turn")
	require.Contains(t, long, "waiting between turns")
	require.Contains(t, long, "worker's per-process host lock is distinct from the session lock")
	require.Contains(t, long, "--prune")
	require.Contains(t, long, "older than 60s")
	require.Contains(t, long, "real OS-level lock probe")
	require.NotContains(t, long, "older than 10 minutes")
	require.NotContains(t, long, "run completes")
}

func TestSessionsWatchHelp_SeparatesSessionMonitorFromTaskTermination(t *testing.T) {
	long := oneLine(sessionsWatchCmd.Long)
	require.Contains(t, long, "shared session-activity view")
	require.Contains(t, long, "work between turns")
	require.Contains(t, long, "incomplete batch read is treated as unknown/live evidence")
	require.Contains(t, long, "end_turn finishes a turn, not necessarily the session or a batch task")
	require.Contains(t, long, "does not independently confirm termination of every related process or task")
	require.Contains(t, long, "does not terminate the session being observed")
}

func TestReadmeRunSecurity_DescribesRestrictedModeAndBoundaries(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	readme := string(data)
	start := strings.Index(readme, "## Security — `rush run` permissions")
	require.GreaterOrEqual(t, start, 0, "no rush run security section")
	section := readme[start:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	require.Contains(t, section, "no interactive approval prompt")
	require.Contains(t, section, "auto-approve permission requests")
	require.Contains(t, section, "--restrict-run")
	require.Contains(t, section, "deny-by-default")
	require.Contains(t, section, "permissions.run.restrict")
	require.Contains(t, section, "permissions.allowed_tools")
	require.Contains(t, section, "not an OS sandbox")
	require.Contains(t, section, "worktree")
	require.Contains(t, section, "arbitrary filesystem")
	require.NotContains(t, section, "no permission gating at all")
	require.NotContains(t, section, "It is unconditional")
	require.NotContains(t, section, "no toggle to turn this off")
}
