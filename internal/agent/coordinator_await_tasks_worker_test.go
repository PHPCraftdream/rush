// Worker (delegated-child) semantics for the await_tasks tool (#1271): the
// coordinator path must BLOCK inside the tool call instead of ending the
// turn, and the top-level path must stay byte-identical.
package agent

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newAwaitWorkerEnv builds a coordinator whose sessions come from testEnv
// (batch world: there is no in-process job store; AwaitTasks answers through
// the bound live reader, see awtBindControls).
func newAwaitWorkerEnv(t *testing.T) (fakeEnv, *coordinator) {
	t.Helper()
	env := testEnv(t)
	c := newBatchRuntimeCoordinator()
	c.sessions = env.sessions
	return env, c
}

// awtShrinkPoll shrinks the worker await's poll interval
// (batchAwaitPollInterval — awaitTasksForBatchWorker passes no PollInterval,
// so the default var is live).
func awtShrinkPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := batchAwaitPollInterval
	batchAwaitPollInterval = d
	t.Cleanup(func() { batchAwaitPollInterval = old })
}

// awtShrinkBlockCap shrinks the unconditional worker block cap
// (batchAwaitWorkerCap — awaitTasksForBatchWorker passes no WaitCap).
func awtShrinkBlockCap(t *testing.T, d time.Duration) {
	t.Helper()
	old := batchAwaitWorkerCap
	batchAwaitWorkerCap = d
	t.Cleanup(func() { batchAwaitWorkerCap = old })
}

// awtShrinkMinWait shrinks the max_wait floor (batchAwaitMinWaitSeconds —
// BatchAwaitWorker validates against it even on the direct worker path).
func awtShrinkMinWait(t *testing.T, seconds int) {
	t.Helper()
	old := batchAwaitMinWaitSeconds
	batchAwaitMinWaitSeconds = seconds
	t.Cleanup(func() { batchAwaitMinWaitSeconds = old })
}

// awtBindControls gives AwaitTasks its flag-ON live binding (bind.live): the
// worker and top-level paths both error "batch live reader unavailable"
// without it (coordinator_await_tasks.go:93-96, :142-145). Flag OFF binds
// nothing: AwaitTasks then reads asyncJobs.store.LiveWorkForRoots. The nil
// middle argument of BindCoordinatorBatchControls is the BatchModelControl
// (batch_install_ingress.go:155-158) — AwaitTasks never touches controls,
// only bind.live, so the fixture's nil is the documented AwaitTasks-only
// shape. The ingress itself must be non-nil (BindCoordinatorBatchRuntimeBindings
// fails closed on nil ingress), so a minimal inert *batchWorkIngress over a
// zero batchRuntime is built here; AwaitTasks never invokes through it.
func awtBindControls(t *testing.T, env fakeEnv, c *coordinator) {
	t.Helper()
	ingress, err := NewBatchWorkIngressWithDiskBindings(&batchRuntime{}, NewBatchLeafRegistryStore(), NewDiskProviderRegistry())
	require.NoError(t, err)
	bindBatchCoordinatorControls(t, c, env.conn, env.dbDir, ingress)
}

// awtSeedLiveWork makes `owner` own ONE live task named callID: it reserves
// + announces a real running batch delegation node pair (seedBatchDelegation,
// batch fixture). Returns the batch root/child node ids.
func awtSeedLiveWork(t *testing.T, conn *sql.DB, owner, callID string) (string, string) {
	t.Helper()
	return seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
}

// awtFinishWork settles the work awtSeedLiveWork created: it projects root
// AND child to completed (sequence 2, same host/runner identity the seed
// reserved with).
func awtFinishWork(t *testing.T, conn *sql.DB, owner, root, child string) {
	t.Helper()
	require.NotEmpty(t, root, "the batch seed must return the root node id")
	require.NoError(t, session.NewBatchStore(conn).Project(context.Background(), session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{
			{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "awt released", ControlState: "none"},
			{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: "awt released", ControlState: "none"},
		}}},
	}))
}

// awtArmCaptureHook registers the test-only first-capture hook BEFORE the
// awaited goroutine is launched (the batch worker's FIRST capture happens at
// BatchAwaitWorker entry, coordinator_await_tasks.go:164-169 — a hook stored
// after that fire is never called). Returns the channel the hook closes.
func awtArmCaptureHook(t *testing.T) <-chan struct{} {
	t.Helper()
	arrived := make(chan struct{})
	hook := func(string, string) { close(arrived) }
	awaitTasksWorkerSnapshotHook.Store(&hook)
	t.Cleanup(func() { awaitTasksWorkerSnapshotHook.Store(nil) })
	return arrived
}

// awtAwaitCaptureArrived waits for the armed hook, so a settle write can
// never race ahead of the awaited goroutine's FIRST live-work snapshot.
func awtAwaitCaptureArrived(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the batch worker await never completed its initial capture")
	}
}

func childSession(t *testing.T, env fakeEnv, toolCallID, parentID string) string {
	t.Helper()
	ctx := context.Background()
	_, err := env.sessions.CreateWithID(ctx, parentID, "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, toolCallID, parentID, "worker child")
	require.NoError(t, err)
	// The durable delegation child-link: the live batch readers answer from
	// batch nodes only.
	seedBatchDelegation(t, env.conn, parentID, toolCallID, child.ID, "agent", "running", false)
	return child.ID
}

// TestAwaitTasksWorkerBlocksUntilOwnJobFinishes pins the worker branch in
// coordinator.AwaitTasks: a delegated child must block, not return at once.
func TestAwaitTasksWorkerBlocksUntilOwnJobFinishes(t *testing.T) {
	awtShrinkPoll(t, 10*time.Millisecond)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-1", "parent-1")
	ctx := context.Background()
	// The worker polls bind.live over batch task nodes, so the awaited work
	// must exist as a live batch node pair owned by the child. Seed ONCE at
	// setup and keep BOTH node ids: the finish Project (batch_project)
	// requires every Updates entry to match a real row, so an empty child id
	// would fail batchRequireRow. Arm the capture hook HERE, before the
	// goroutine below is launched.
	awtBindControls(t, env, c)
	root, seedChild := awtSeedLiveWork(t, env.conn, child, "call-1")
	captureArrived := awtArmCaptureHook(t)

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
	// The 50ms guard proves the await is blocked, not that its FIRST
	// live-work read already landed; sync on it before settling, or a
	// settle that wins the race reads as "nothing is running".
	awtAwaitCaptureArrived(t, captureArrived)
	awtFinishWork(t, env.conn, child, root, seedChild)
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
	awtShrinkPoll(t, 10*time.Millisecond)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-2", "parent-1")
	ctx := context.Background()
	// per-call batch node ids: the finish Project needs BOTH the root and
	// the child id (every Updates entry must match a real row). Arm the
	// capture hook before the goroutine below is launched.
	seeds := map[string][2]string{}
	awtBindControls(t, env, c)
	captureArrived := awtArmCaptureHook(t)
	for _, id := range []string{"call-1", "call-2"} {
		root, seedChild := awtSeedLiveWork(t, env.conn, child, id)
		seeds[id] = [2]string{root, seedChild}
	}
	done := make(chan tools.AwaitTasksResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := c.AwaitTasks(ctx, child, "all", 0)
		done <- res
		errCh <- err
	}()
	// Let the goroutine's initial live-work read happen first: the snapshot
	// hook fires after the first BatchAwaitWorker capture.
	awtAwaitCaptureArrived(t, captureArrived)
	awtFinishWork(t, env.conn, child, seeds["call-1"][0], seeds["call-1"][1])
	select {
	case r := <-done:
		t.Fatalf("await all returned after only the first job finished: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	awtFinishWork(t, env.conn, child, seeds["call-2"][0], seeds["call-2"][1])
	res := <-done
	require.NoError(t, <-errCh)
	require.True(t, res.Blocked)
	require.Empty(t, res.WaitingFor)
	require.Equal(t, 2, res.FinishedCount)
}

// TestAwaitTasksWorkerRefusesWhenNothingRunning pins the live-work refusal
// for the worker path.
func TestAwaitTasksWorkerRefusesWhenNothingRunning(t *testing.T) {
	awtShrinkPoll(t, 10*time.Millisecond)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-3", "parent-1")
	// ON must still hold the live reader: with the binding in place the
	// complete-but-empty capture refuses exactly like the legacy read
	// (batch_await.go:118-120, batchAwaitRefusal).
	awtBindControls(t, env, c)
	_, err := c.AwaitTasks(context.Background(), child, "any", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing is running")
}

// TestAwaitTasksWorkerCancelUnblocks pins the ctx.Done() select arm in
// awaitTasksForWorker.
func TestAwaitTasksWorkerCancelUnblocks(t *testing.T) {
	awtShrinkPoll(t, 10*time.Millisecond)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-4", "parent-1")
	ctx, cancel := context.WithCancel(context.Background())
	awtBindControls(t, env, c)
	awtSeedLiveWork(t, env.conn, child, "call-1")
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
	awtShrinkPoll(t, 10*time.Millisecond)
	// BatchAwaitWorker validates max_wait against the shared floor; shrink
	// it so max_wait=1 still reaches the deadline arm.
	awtShrinkMinWait(t, 1)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-5", "parent-1")
	ctx := context.Background()
	awtBindControls(t, env, c)
	awtSeedLiveWork(t, env.conn, child, "call-1")
	start := time.Now()
	res, err := c.awaitTasksForBatchWorker(ctx, child, "any", 1)
	require.NoError(t, err)
	require.True(t, res.Blocked)
	require.True(t, res.TimedOut)
	require.Len(t, res.WaitingFor, 1)
	require.Less(t, time.Since(start), 5*time.Second)
}

// TestAwaitTasksTopLevelPathUnchanged pins the child-detection branch: a
// top-level session keeps the immediate, non-blocking StopTurn-shaped path.
func TestAwaitTasksTopLevelPathUnchanged(t *testing.T) {
	awtShrinkPoll(t, 10*time.Millisecond)
	env := testEnv(t)
	c := newBatchRuntimeCoordinator()
	c.sessions = env.sessions
	ctx := context.Background()
	top := "top-level-await-" + time.Now().Format("150405.000000000")
	_, err := env.sessions.CreateWithID(ctx, top, "top")
	require.NoError(t, err)
	// The top-level path reads bind.live (coordinator_await_tasks.go); the
	// live row must be a batch node pair owned by the top-level session.
	awtBindControls(t, env, c)
	_, _ = awtSeedLiveWork(t, env.conn, top, "call-1")
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
	awtShrinkPoll(t, 10*time.Millisecond)
	awtShrinkBlockCap(t, 100*time.Millisecond)
	env, c := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-6", "parent-1")
	ctx := context.Background()
	awtBindControls(t, env, c)
	awtSeedLiveWork(t, env.conn, child, "call-1")
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

// TestAwaitTasksNeverStallDetached was DELETEd as an obsolete contract: the
// stall-detach authority (and its exclusion set) is gone with the work
// ledger; there is no detacher left to exclude await_tasks from.

// runAwaitSequence, awaitWork and the TestAwaitTasksWorkerIdentity* tests
// were DELETEd as obsolete contracts: they pinned the DELETED legacy
// awaitTasksWorker's identity/InMinimal semantics over session.RootLiveWork
// (LiveJob/RootLiveWork are removed session symbols). The batch await's
// identity contract is pinned over BatchCapturedRoots by
// coordinator_await_tasks_identity_test.go and the BatchAwaitWorker tests.
