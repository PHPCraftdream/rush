package agent

// Unit tests for task #1053: job_kill/job_output only ever learn a job's
// SHELL id from the ledger, set by asyncTool.awaitShell once the inner bash
// tool reports it (see work_ledger.go's setShellID/ResolveJobShellID).

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// TestWorkLedger_ResolveJobShellID_OwnedJobResolves pins the success path:
// before a shell id is recorded the job is "still starting"; once
// setShellID runs (as awaitShell does live), ResolveJobShellID returns it
// for the owning session.
func TestWorkLedger_ResolveJobShellID_OwnedJobResolves(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "", tools.BashToolName, "", true, false, nil, nil)
	require.NoError(t, err)

	_, err = l.ResolveJobShellID("session-a", "call-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "still starting")

	l.setShellID(jobOf(l, "session-a", "call-1"), "003")
	got, err := l.ResolveJobShellID("session-a", "call-1")
	require.NoError(t, err)
	require.Equal(t, "003", got)
}

// TestWorkLedger_ResolveJobShellID_ForeignSessionNotFound pins ownership: a
// job id from another session must never resolve, matching
// BackgroundShellManager.GetOwned/KillOwned's own per-session boundary.
func TestWorkLedger_ResolveJobShellID_ForeignSessionNotFound(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "", tools.BashToolName, "", true, false, nil, nil)
	require.NoError(t, err)
	l.setShellID(jobOf(l, "session-a", "call-1"), "003")

	_, err = l.ResolveJobShellID("session-b", "call-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestWorkLedger_ResolveJobShellID_UnknownJobNotFound covers the plain
// not-found case: no such job id was ever started for this session.
func TestWorkLedger_ResolveJobShellID_UnknownJobNotFound(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, err := l.ResolveJobShellID("session-a", "does-not-exist")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestWorkLedger_ResolveJobShellID_NotACommandJob pins the agent/
// agentic_fetch boundary: a delegation job id must not resolve to a shell --
// job_kill/job_output refuse it with a distinct message instead of silently
// reporting "still starting" forever (a delegation never gets a shellID).
func TestWorkLedger_ResolveJobShellID_NotACommandJob(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "", AgentToolName, "child-session", false, false, nil, nil)
	require.NoError(t, err)

	_, err = l.ResolveJobShellID("session-a", "call-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a command")
}

// TestWorkLedger_ResolveJobShellID_RunCommandRefusedClearly: run_command
// never gets a shellID (no BackgroundShellManager), so it must be refused
// with a distinct message, not "still starting" forever.
func TestWorkLedger_ResolveJobShellID_RunCommandRefusedClearly(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "", tools.RunCommandToolName, "", true, false, nil, nil)
	require.NoError(t, err)

	_, err = l.ResolveJobShellID("session-a", "call-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "run_command job")
	require.NotContains(t, err.Error(), "still starting")
}

// TestWorkLedger_ResolveJobShellID_NilLedgerIsSafe: a nil *workLedger (a
// coordinator built without NewCoordinator, or asyncJobs unset) must not
// panic -- it degrades to "not found", same as no ledger at all.
func TestWorkLedger_ResolveJobShellID_NilLedgerIsSafe(t *testing.T) {
	t.Parallel()
	var l *workLedger
	_, err := l.ResolveJobShellID("session-a", "call-1")
	require.Error(t, err)
	l.setShellID(jobOf(l, "session-a", "call-1"), "003") // must not panic
}

// TestWorkLedger_SetShellID_UnknownJobIsNoOp: recording a shell id for a job
// that finished/was never started must not panic or create a phantom entry.
func TestWorkLedger_SetShellID_UnknownJobIsNoOp(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	l.setShellID(jobOf(l, "session-a", "does-not-exist"), "003")
	_, err := l.ResolveJobShellID("session-a", "does-not-exist")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}
