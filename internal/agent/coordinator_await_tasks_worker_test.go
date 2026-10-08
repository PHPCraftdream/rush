// Worker (delegated-child) semantics for the await_tasks tool (#1271): the
// coordinator path must BLOCK inside the tool call instead of ending the
// turn, and the top-level path must stay byte-identical.
package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newAwaitWorkerEnv builds a coordinator whose sessions/asyncJobs come from
// testEnv, plus a live async-job store against the same DB.
func newAwaitWorkerEnv(t *testing.T) (fakeEnv, *coordinator, *session.AsyncJobStore) {
	t.Helper()
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-worker-test")
	c := &coordinator{
		sessions:  env.sessions,
		arb:       arbiter{bySession: map[string]*arbiterState{}},
		asyncJobs: &workLedger{store: store},
	}
	return env, c, store
}

func shrinkWorkerPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := awaitTasksWorkerPollInterval
	awaitTasksWorkerPollInterval = d
	t.Cleanup(func() { awaitTasksWorkerPollInterval = old })
}

func childSession(t *testing.T, env fakeEnv, toolCallID, parentID string) string {
	t.Helper()
	ctx := context.Background()
	_, err := env.sessions.CreateTaskSession(ctx, toolCallID, parentID, "worker child")
	require.NoError(t, err)
	return toolCallID
}

// TestAwaitTasksWorkerBlocksUntilOwnJobFinishes pins the worker branch in
// coordinator.AwaitTasks: a delegated child must block, not return at once.
func TestAwaitTasksWorkerBlocksUntilOwnJobFinishes(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-1", "parent-1")
	ctx := context.Background()
	_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)

	type result struct {
		res tools.AwaitTasksResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := c.AwaitTasks(ctx, child, "any", 0)
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("AwaitTasks returned before any job finished: %+v err=%v", r.res, r.err)
	case <-time.After(50 * time.Millisecond):
	}
	_, err = store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	r := <-done
	require.NoError(t, r.err)
	// Revert-check: comment out the worker branch in AwaitTasks and this
	// fails — the top-level path returns immediately without blocking.
	require.True(t, r.res.Blocked)
	require.False(t, r.res.TimedOut)
	require.Empty(t, r.res.WaitingFor)
}

// TestAwaitTasksWorkerUntilAllWaitsForBothJobs pins the mode=="all" wake
// condition in awaitTasksForWorker.
func TestAwaitTasksWorkerUntilAllWaitsForBothJobs(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-2", "parent-1")
	ctx := context.Background()
	for _, id := range []string{"call-1", "call-2"} {
		_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: id, Kind: session.JobKindCommand, Input: id, ToolName: "bash"})
		require.NoError(t, err)
	}
	done := make(chan tools.AwaitTasksResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := c.AwaitTasks(ctx, child, "all", 0)
		done <- res
		errCh <- err
	}()
	// Let the goroutine's initial live-work read happen first.
	time.Sleep(3 * awaitTasksWorkerPollInterval)
	_, err := store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	select {
	case r := <-done:
		t.Fatalf("await all returned after only the first job finished: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	_, err = store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-2", State: "completed", Wake: true})
	require.NoError(t, err)
	res := <-done
	require.NoError(t, <-errCh)
	require.True(t, res.Blocked)
	require.Empty(t, res.WaitingFor)
	require.Equal(t, 2, res.FinishedCount)
}

// TestAwaitTasksWorkerRefusesWhenNothingRunning pins the live-work refusal
// for the worker path.
func TestAwaitTasksWorkerRefusesWhenNothingRunning(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env, c, _ := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-3", "parent-1")
	_, err := c.AwaitTasks(context.Background(), child, "any", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing is running")
}

// TestAwaitTasksWorkerCancelUnblocks pins the ctx.Done() select arm in
// awaitTasksForWorker.
func TestAwaitTasksWorkerCancelUnblocks(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-4", "parent-1")
	ctx, cancel := context.WithCancel(context.Background())
	_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := c.AwaitTasks(ctx, child, "all", 0)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitTasks did not unblock on cancellation")
	}
}

// TestAwaitTasksWorkerDeadlineReturnsRemaining pins the deadline timer arm;
// it calls awaitTasksForWorker directly to bypass the top-level 60s bound.
func TestAwaitTasksWorkerDeadlineReturnsRemaining(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-5", "parent-1")
	ctx := context.Background()
	_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	start := time.Now()
	res, err := c.awaitTasksForWorker(ctx, child, "any", 1)
	require.NoError(t, err)
	require.True(t, res.Blocked)
	require.True(t, res.TimedOut)
	require.Len(t, res.WaitingFor, 1)
	require.Less(t, time.Since(start), 5*time.Second)
}

// TestAwaitTasksTopLevelPathUnchanged pins the child-detection branch: a
// top-level session keeps the immediate, non-blocking StopTurn-shaped path.
func TestAwaitTasksTopLevelPathUnchanged(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-top-test")
	c := &coordinator{
		sessions:  env.sessions,
		arb:       arbiter{bySession: map[string]*arbiterState{}},
		asyncJobs: &workLedger{store: store},
	}
	ctx := context.Background()
	top := "top-level-await-" + time.Now().Format("150405.000000000")
	_, err := env.sessions.CreateWithID(ctx, top, "top")
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: top, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	res, err := c.AwaitTasks(ctx, top, "any", 0)
	require.NoError(t, err)
	require.False(t, res.Blocked)
	require.Len(t, res.WaitingFor, 1)
	// mode "all" arms the arbiter sleep exactly as before.
	_, err = c.AwaitTasks(ctx, top, "all", 0)
	require.NoError(t, err)
	require.True(t, c.arb.snapshot(top).SleepAll)
}

// TestAwaitTasksWorkerCapAppliesWithoutMaxWait pins the unconditional wait
// cap: without max_wait the worker must still wake before the tool watchdog.
func TestAwaitTasksWorkerCapAppliesWithoutMaxWait(t *testing.T) {
	shrinkWorkerPoll(t, 10*time.Millisecond)
	old := workerAwaitBlockCap
	workerAwaitBlockCap = 100 * time.Millisecond
	t.Cleanup(func() { workerAwaitBlockCap = old })
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-6", "parent-1")
	ctx := context.Background()
	_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	done := make(chan tools.AwaitTasksResult, 1)
	go func() {
		res, _ := c.AwaitTasks(ctx, child, "all", 0)
		done <- res
	}()
	select {
	case res := <-done:
		require.True(t, res.TimedOut)
		require.Len(t, res.WaitingFor, 1)
	case <-time.After(5 * time.Second):
		t.Fatal("worker await without max_wait ignored the block cap")
	}
}

// TestAwaitTasksNeverStallDetached pins await_tasks in stallDetachExcludedTools:
// detaching the worker's deliberate block would end its wait early.
func TestAwaitTasksNeverStallDetached(t *testing.T) {
	require.True(t, stallDetachExcludedTools[tools.AwaitTasksToolName])
}
