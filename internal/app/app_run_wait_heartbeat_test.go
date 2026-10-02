// The `rush run` wait heartbeat: it names the job and host it waits on
// (ASYNC-10) and its cadence is per run, not per nextStep call.
package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func heartbeatLines(out string) int {
	return strings.Count(out, "still has open work")
}

// Revert-check: the notice state local to nextStep (the old shape) prints one
// line per call -- the first assertion sees 3 lines instead of 1.
func TestNextStep_HeartbeatCadenceSpansCalls(t *testing.T) {
	orig := cliOpenScopeWaitNoticeInterval
	cliOpenScopeWaitNoticeInterval = time.Hour
	t.Cleanup(func() { cliOpenScopeWaitNoticeInterval = orig })

	working := scopeAnswer{state: agent.CLIScopeState{WorkOpen: true}}
	closed := scopeAnswer{state: agent.CLIScopeState{}}
	var out syncBuffer
	l := nextStepLoop(&scriptedScopeSource{script: []scopeAnswer{working, working, closed}})
	l.stderr = &out
	for range 3 {
		l.source = &scriptedScopeSource{script: []scopeAnswer{working, closed}}
		step, _, err := l.nextStep()
		require.NoError(t, err)
		require.Equal(t, stepExit, step)
	}
	require.Equal(t, 1, heartbeatLines(out.String()), "one heartbeat per interval over the whole run, not per nextStep call")

	// Once the interval has passed, the next wait prints again.
	cliOpenScopeWaitNoticeInterval = 0
	l.source = &scriptedScopeSource{script: []scopeAnswer{working, closed}}
	_, _, err := l.nextStep()
	require.NoError(t, err)
	require.Equal(t, 2, heartbeatLines(out.String()))
}

// The heartbeat names what it waits on: the own-process job and a job on
// another live host (with its PID), as `sessions jobs` shows them.
//
// Revert-check: the generic "a running job/delegation, or an unreachable
// host" wording (the old line) contains neither name.
func TestRunNonInteractive_WaitHeartbeatNamesJobAndHost(t *testing.T) {
	orig := cliOpenScopeWaitNoticeInterval
	cliOpenScopeWaitNoticeInterval = time.Hour
	origErr := cliLoopStderr
	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() { cliOpenScopeWaitNoticeInterval, cliLoopStderr = orig, origErr })

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 11, 3)
	})
	ctx := context.Background()
	// A live foreign host: a lock this test holds, plus its display row.
	foreign, err := filelock.TryAcquireFileLock(session.HostLockPath(h.dataDir, "peer-host"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreign.Release() })
	q := db.New(h.app.DB())
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "peer-host", Pid: 4242, Label: "peer", StartedAt: 1700000000})
	require.NoError(t, err)
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: h.sessionID, ToolCallID: "peer-job-1", Kind: "command", ToolName: "run_command",
		InputHash: "h", HostID: "peer-host", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: h.sessionID, ToolCallID: "own-job-1", Kind: session.JobKindCommand, Input: "sleep", ToolName: "bash",
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	var out syncBuffer
	_, _ = h.app.RunNonInteractiveWithResult(runCtx, &out, "do it", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, h.sessionID, false)

	got := stderr.String()
	require.Equal(t, 1, heartbeatLines(got), "one heartbeat line in this short wait: %s", got)
	require.Contains(t, got, `bash "own-job-1" on this process`)
	require.Contains(t, got, `run_command "peer-job-1" on host peer-host (PID 4242, alive)`)
}

// The stderr line for debt no automatic turn is allowed for names the REAL
// reason the policy gave (a released delegation child here), not a fixed
// "background-shell completion" wording.
//
// Revert-check: printing a fixed reason (the old text) instead of
// state.Reason turns the first assertion red.
func TestNextStep_DeferredLineNamesTheRealReason(t *testing.T) {
	var out syncBuffer
	l := nextStepLoop(&scriptedScopeSource{script: []scopeAnswer{
		{state: agent.CLIScopeState{Drain: agent.DrainDeferred, Reason: "released delegation child"}},
	}})
	l.stderr = &out
	step, _, err := l.nextStep()
	require.NoError(t, err)
	require.Equal(t, stepExit, step)
	require.Contains(t, out.String(), "released delegation child")
	require.NotContains(t, out.String(), "background-shell")
}
