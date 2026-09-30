// Executor identity (A11's in-process half, R2A-3/R2B-6) and gone-delegation
// retirement (R2B-12): an executor reports its result for the *asyncJob it
// was started for, never for whatever job its key names by then.
package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAsyncTool_StaleExecutorAfterAckAbortCannotCommitOntoReclaimedKey drives
// the exact sequence through the REAL ledger and asyncTool: Start (E1) ->
// ack write fails, abort deletes the row/job -> the provider repeats the same
// tool_call_id -> Start (E2, fresh claim) -> E1's late result. E1's result
// must not be committed onto E2's row.
//
// Revert-check: with finish/armDelegation resolving the job by key again
// (job = s.jobs[toolCallID]), E1's late result committed "output of executor
// 1" onto the second claim's row (state completed, E2's job marked
// executorReturned), and the Never assertion failed.
func TestAsyncTool_StaleExecutorAfterAckAbortCannotCommitOntoReclaimedKey(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	var started, release [2]chan struct{}
	for i := range started {
		started[i], release[i] = make(chan struct{}), make(chan struct{})
	}
	t.Cleanup(func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})
	var calls atomic.Int32
	// The inner tool ignores its ctx: a late return is exactly the case
	// abort's executor cancel cannot prevent.
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		n := int(calls.Add(1)) - 1
		close(started[n])
		<-release[n]
		return fantasy.NewTextResponse(fmt.Sprintf("output of executor %d", n+1)), nil
	})
	completed := make(chan AsyncCompletion, 4)
	l := newWorkLedger(func(c AsyncCompletion) { completed <- c })
	l.store = store
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: l}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginWeb)
	call := fantasy.ToolCall{ID: "call_0", Name: "run_command", Input: `{}`}

	_, err := wrapped.Run(ctx, call)
	require.NoError(t, err)
	<-started[0]
	job1 := jobOf(l, "session", "call_0")
	require.NotNil(t, job1)

	l.abort(job1) // the started result's write failed

	_, err = wrapped.Run(ctx, call)
	require.NoError(t, err)
	<-started[1]
	job2 := jobOf(l, "session", "call_0")
	require.NotNil(t, job2)
	require.NotSame(t, job1, job2)
	require.NotEqual(t, job1.claimID, job2.claimID)

	close(release[0]) // E1 returns late
	require.Never(t, func() bool {
		row, err := store.Get(context.Background(), "session", "call_0")
		l.mu.Lock()
		defer l.mu.Unlock()
		return err != nil || row.State != "running" || job2.state != phaseRunning || job2.executorReturned
	}, 300*time.Millisecond, 10*time.Millisecond, "a superseded executor's result must never reach the reclaimed key's job or row")

	l.acknowledged(jobOf(l, "session", "call_0"))
	close(release[1])
	select {
	case c := <-completed:
		require.Equal(t, "output of executor 2", c.Content, "the job's own executor decides its outcome")
	case <-time.After(5 * time.Second):
		t.Fatal("the reclaimed job's own completion was never delivered")
	}
	require.Empty(t, drainCompletions(completed), "exactly one completion")
}

// TestWorkLedger_SupersededExecutorWritesNothingByKey pins the remaining
// executor-side by-key writers: a superseded job's shell id and output
// buffer must not land on the job that replaced it.
func TestWorkLedger_SupersededExecutorWritesNothingByKey(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)

	job1, _, err := l.Start("owner", "call_0", "x", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.abort(job1)
	job2, _, err := l.Start("owner", "call_0", "x", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.NotSame(t, job1, job2)

	l.setShellID(job1, "stale-shell")
	l.setRunCommandBuffer(job1, nil)
	l.finish(job1, jobResult{content: "stale"})

	l.mu.Lock()
	defer l.mu.Unlock()
	require.Empty(t, job2.shellID, "a superseded executor must not name its shell on the new job (job_kill would kill the wrong shell)")
	require.False(t, job2.executorReturned)
	require.Equal(t, phaseRunning, job2.state)
}

// TestWorkLedger_GoneDelegationLeavesNoParkedEntry pins R2B-12: a delegation
// whose row is gone (parent session deleted) is retired, not left phaseRunning
// in byChild where every 60s pass would re-check it and the deleted parent
// would stay reported as parked.
//
// Revert-check: with the Gone branch again only deleting from the owner's map
// (no retire), hasParked stayed true.
func TestWorkLedger_GoneDelegationLeavesNoParkedEntry(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store

	job, _, err := l.Start("parent", "deleg", "x", AgentToolName, "child-1", false, false, nil, func() {})
	require.NoError(t, err)
	require.NoError(t, store.DeleteUnannounced(context.Background(), "parent", "deleg")) // the cascade removed the row

	l.armDelegation(job, jobResult{content: "child yielded"})

	require.False(t, l.hasParked(), "a gone delegation must leave byChild")
	require.Empty(t, l.parkedParentSessions())
	require.Empty(t, l.parkedChildSessions())
	require.False(t, l.running("parent"))
}
