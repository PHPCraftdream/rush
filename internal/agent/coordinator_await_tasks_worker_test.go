// Worker (delegated-child) semantics for the await_tasks tool (#1271): the
// coordinator path must BLOCK inside the tool call instead of ending the
// turn, and the top-level path must stay byte-identical.
package agent

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newAwaitWorkerEnv builds a coordinator whose sessions/asyncJobs come from
// testEnv, plus a live async-job store against the same DB.
func newAwaitWorkerEnv(t *testing.T) (fakeEnv, *coordinator, *session.AsyncJobStore) {
	t.Helper()
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-worker-test")
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	c := &coordinator{
		sessions:  env.sessions,
		arb:       arbiter{bySession: map[string]*arbiterState{}},
		asyncJobs: &workLedger{store: store},
	}
	return env, c, store
}

// awtShrinkPoll shrinks whichever poll-interval var the selected runtime's
// worker await actually honors: OFF reads awaitTasksWorkerPollInterval
// (coordinator_await_tasks.go:212); ON routes through BatchAwaitWorker's
// batchAwaitPollInterval (batch_await.go:130 — awaitTasksForBatchWorker
// passes no PollInterval, so the default var is live). The legacy var is
// shrunk unconditionally: it is dead under ON, so shrinking it is inert.
func awtShrinkPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := awaitTasksWorkerPollInterval
	awaitTasksWorkerPollInterval = d
	t.Cleanup(func() { awaitTasksWorkerPollInterval = old })
	if batchFlagEnabled() {
		oldBatch := batchAwaitPollInterval
		batchAwaitPollInterval = d
		t.Cleanup(func() { batchAwaitPollInterval = oldBatch })
	}
}

// awtShrinkBlockCap shrinks the unconditional worker block cap for both
// authorities: workerAwaitBlockCap (legacy, coordinator_await_tasks.go:264)
// and batchAwaitWorkerCap (batch, batch_await.go:134 — again the default var
// is live because awaitTasksForBatchWorker passes no WaitCap).
func awtShrinkBlockCap(t *testing.T, d time.Duration) {
	t.Helper()
	old := workerAwaitBlockCap
	workerAwaitBlockCap = d
	t.Cleanup(func() { workerAwaitBlockCap = old })
	if batchFlagEnabled() {
		oldBatch := batchAwaitWorkerCap
		batchAwaitWorkerCap = d
		t.Cleanup(func() { batchAwaitWorkerCap = oldBatch })
	}
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
	if !batchFlagEnabled() {
		return
	}
	ingress, err := NewBatchWorkIngressWithDiskBindings(&batchRuntime{}, NewBatchLeafRegistryStore(), NewDiskProviderRegistry())
	require.NoError(t, err)
	bindBatchCoordinatorControls(t, c, env.conn, env.dbDir, ingress)
}

// awtSeedLiveWork makes `owner` own ONE live task named callID in the
// selected authority: ON reserves + announces a real running batch
// delegation node pair (seedBatchDelegation, batch fixture); OFF claims a
// legacy running async_jobs row. Returns the batch root/child node ids
// (both empty OFF).
func awtSeedLiveWork(t *testing.T, conn *sql.DB, owner, callID string) (string, string) {
	t.Helper()
	if batchFlagEnabled() {
		return seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
	}
	store := session.NewAsyncJobStore(conn, os.TempDir(), os.Getpid(), "await-worker-test")
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	_, err := store.Claim(context.Background(), session.ClaimParams{Owner: owner, ToolCallID: callID, Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	return "", ""
}

// awtFinishWork settles the work awtSeedLiveWork created, in the same
// authority: ON projects root AND child to completed (sequence 2, same
// host/runner identity the seed reserved with); OFF does nothing beyond
// checking the seed shape — the legacy tests drive store.Transition through
// their own store handle.
func awtFinishWork(t *testing.T, conn *sql.DB, owner, root, child string) {
	t.Helper()
	if batchFlagEnabled() {
		require.NotEmpty(t, root, "the batch seed must return the root node id")
		require.NoError(t, session.NewBatchStore(conn).Project(context.Background(), session.BatchProjection{
			Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
			Roots: []session.BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "awt released", ControlState: "none"},
				{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: "awt released", ControlState: "none"},
			}}},
		}))
		return
	}
	require.Empty(t, root, "the legacy seed returns no batch node ids")
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

// awtAwaitCaptureArrived waits for the armed hook: OFF keeps the historical
// sleep of three poll intervals (the byte-identical legacy arrangement); ON
// waits for the armed channel, so a settle write can never race ahead of the
// awaited goroutine's FIRST live-work snapshot.
func awtAwaitCaptureArrived(t *testing.T, arrived <-chan struct{}, legacyWait func()) {
	t.Helper()
	if !batchFlagEnabled() {
		legacyWait()
		return
	}
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
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-worker-test")
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	_, err = store.Claim(ctx, session.ClaimParams{Owner: parentID, ToolCallID: toolCallID, Kind: session.JobKindAgent, Input: "x", ChildSessionID: child.ID, ToolName: "agent"})
	require.NoError(t, err)
	return child.ID
}

// TestAwaitTasksWorkerBlocksUntilOwnJobFinishes pins the worker branch in
// coordinator.AwaitTasks: a delegated child must block, not return at once.
func TestAwaitTasksWorkerBlocksUntilOwnJobFinishes(t *testing.T) {
	awtShrinkPoll(t, 10*time.Millisecond)
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-1", "parent-1")
	ctx := context.Background()
	root, seedChild := "", ""
	captureArrived := (<-chan struct{})(nil)
	if batchFlagEnabled() {
		// ON: the worker polls bind.live over batch task nodes, so the
		// awaited work must exist as a live batch node pair owned by the
		// child (legacy async_jobs rows are invisible to BatchCaptured).
		// Seed ONCE at setup and keep BOTH node ids: the finish Project
		// (batch_project) requires every Updates entry to match a real row,
		// so an empty child id would fail batchRequireRow. Arm the capture
		// hook HERE, before the goroutine below is launched.
		awtBindControls(t, env, c)
		root, seedChild = awtSeedLiveWork(t, env.conn, child, "call-1")
		captureArrived = awtArmCaptureHook(t)
	} else {
		_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
	}

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
	awtAwaitCaptureArrived(t, captureArrived, func() { time.Sleep(3 * awaitTasksWorkerPollInterval) })
	if batchFlagEnabled() {
		awtFinishWork(t, env.conn, child, root, seedChild)
	} else {
		_, err := store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-1", State: "completed", Wake: true})
		require.NoError(t, err)
	}
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
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-2", "parent-1")
	ctx := context.Background()
	// per-call batch node ids: the finish Project needs BOTH the root and
	// the child id (every Updates entry must match a real row). Arm the
	// capture hook before the goroutine below is launched.
	seeds := map[string][2]string{}
	var captureArrived <-chan struct{}
	if batchFlagEnabled() {
		captureArrived = awtArmCaptureHook(t)
	}
	for _, id := range []string{"call-1", "call-2"} {
		if batchFlagEnabled() {
			awtBindControls(t, env, c)
			root, seedChild := awtSeedLiveWork(t, env.conn, child, id)
			seeds[id] = [2]string{root, seedChild}
			continue
		}
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
	// Let the goroutine's initial live-work read happen first (ON: the
	// snapshot hook fires after the first BatchAwaitWorker capture; OFF: the
	// legacy three-poll sleep).
	awtAwaitCaptureArrived(t, captureArrived, func() { time.Sleep(3 * awaitTasksWorkerPollInterval) })
	if batchFlagEnabled() {
		awtFinishWork(t, env.conn, child, seeds["call-1"][0], seeds["call-1"][1])
	} else {
		_, err := store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-1", State: "completed", Wake: true})
		require.NoError(t, err)
	}
	select {
	case r := <-done:
		t.Fatalf("await all returned after only the first job finished: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if batchFlagEnabled() {
		awtFinishWork(t, env.conn, child, seeds["call-2"][0], seeds["call-2"][1])
	} else {
		_, err := store.Transition(ctx, session.TransitionParams{Owner: child, ToolCallID: "call-2", State: "completed", Wake: true})
		require.NoError(t, err)
	}
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
	env, c, _ := newAwaitWorkerEnv(t)
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
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-4", "parent-1")
	ctx, cancel := context.WithCancel(context.Background())
	if batchFlagEnabled() {
		awtBindControls(t, env, c)
		awtSeedLiveWork(t, env.conn, child, "call-1")
	} else {
		_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
	}
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
	awtShrinkPoll(t, 10*time.Millisecond)
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
	if batchFlagEnabled() {
		// ON: the top-level path reads bind.live (coordinator_await_tasks.go
		// :84-100); without the binding it errors "batch live reader
		// unavailable", and the live row must be a batch node pair owned by
		// the top-level session.
		awtBindControls(t, env, c)
		_, _ = awtSeedLiveWork(t, env.conn, top, "call-1")
	} else {
		_, err = store.Claim(ctx, session.ClaimParams{Owner: top, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
	}
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
	env, c, store := newAwaitWorkerEnv(t)
	child := childSession(t, env, "call-child-6", "parent-1")
	ctx := context.Background()
	if batchFlagEnabled() {
		awtBindControls(t, env, c)
		awtSeedLiveWork(t, env.conn, child, "call-1")
	} else {
		_, err := store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
	}
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

// runAwaitSequence advances one poll per read, then expires the deadline.
// No wall-clock sleeps or shared hooks are needed.
func runAwaitSequence(t *testing.T, mode string, maxWait int, sequence ...session.RootLiveWork) (tools.AwaitTasksResult, int, error) {
	t.Helper()
	poll := make(chan time.Time, 1)
	deadline := make(chan time.Time, 1)
	reads := 0
	res, err := awaitTasksWorker(context.Background(), mode, maxWait, func() session.RootLiveWork {
		require.Less(t, reads, len(sequence), "unexpected extra read")
		work := sequence[reads]
		reads++
		if reads < len(sequence)-1 {
			poll <- time.Time{}
		} else if reads == len(sequence)-1 {
			deadline <- time.Time{}
		}
		return work
	}, func(cap time.Duration) (<-chan time.Time, <-chan time.Time, func()) {
		want := workerAwaitBlockCap
		if maxWait > 0 && time.Duration(maxWait)*time.Second < want {
			want = time.Duration(maxWait) * time.Second
		}
		require.Equal(t, want, cap)
		return poll, deadline, func() {}
	})
	return res, reads, err
}

func awaitWork(ids ...string) session.RootLiveWork {
	work := session.RootLiveWork{}
	for _, id := range ids {
		work.Own = append(work.Own, session.LiveJob{SessionID: "worker", ToolCallID: id, ToolName: "bash"})
	}
	return work
}

func TestAwaitTasksWorkerIdentityAnyReplacement(t *testing.T) {
	t.Parallel()
	res, reads, err := runAwaitSequence(t, "any", 0, awaitWork("A", "B"), awaitWork("B", "C"), awaitWork("B", "C"))
	require.NoError(t, err)
	require.Equal(t, 2, reads, "A disappeared even though C replaced its count")
	require.True(t, res.Blocked)
	require.False(t, res.TimedOut)
	require.Equal(t, 1, res.FinishedCount)
	require.Len(t, res.WaitingFor, 1)
}

func TestAwaitTasksWorkerIdentityAllIgnoresLateLaunches(t *testing.T) {
	t.Parallel()
	res, reads, err := runAwaitSequence(t, "all", 0,
		awaitWork("A", "B"), awaitWork("B", "C"), awaitWork("C", "D"), awaitWork("C", "D"))
	require.NoError(t, err)
	require.Equal(t, 3, reads, "wait only for A and B, not late C and D")
	require.False(t, res.TimedOut)
	require.Equal(t, 2, res.FinishedCount)
	require.Empty(t, res.WaitingFor)
}

func TestAwaitTasksWorkerIdentityAllOriginalsGone(t *testing.T) {
	t.Parallel()
	res, reads, err := runAwaitSequence(t, "all", 0, awaitWork("A", "B"), awaitWork(), awaitWork())
	require.NoError(t, err)
	require.Equal(t, 2, reads)
	require.True(t, res.Blocked)
	require.False(t, res.TimedOut)
	require.Equal(t, 2, res.FinishedCount)
	require.Empty(t, res.WaitingFor)
}

func TestAwaitTasksWorkerIdentityEmptyEntry(t *testing.T) {
	t.Parallel()
	_, reads, err := runAwaitSequence(t, "all", 0, awaitWork())
	require.EqualError(t, err, "nothing is running; continue or finish")
	require.Equal(t, 1, reads)
}

func TestAwaitTasksWorkerIdentityReappearanceStaysFinished(t *testing.T) {
	t.Parallel()
	res, reads, err := runAwaitSequence(t, "all", 0,
		awaitWork("A", "B"), awaitWork("B"), awaitWork("A"), awaitWork("A"))
	require.NoError(t, err)
	require.Equal(t, 3, reads, "A's observed disappearance is latched, not undone")
	require.False(t, res.TimedOut)
	require.Equal(t, 2, res.FinishedCount)
	require.Empty(t, res.WaitingFor)
}

func TestAwaitTasksWorkerIdentityOwnerAndDuplicates(t *testing.T) {
	t.Parallel()
	entry := awaitWork("same")
	entry.Descendants = []session.LiveJob{{SessionID: "child", ToolCallID: "same", ToolName: "agent", ChildSessionID: "grandchild"}, entry.Own[0]}
	next := session.RootLiveWork{Descendants: entry.Descendants[:1]}
	res, reads, err := runAwaitSequence(t, "any", 0, entry, next, next)
	require.NoError(t, err)
	require.Equal(t, 2, reads)
	require.Equal(t, 1, res.FinishedCount)
	require.Len(t, res.WaitingFor, 1)
	require.Equal(t, "grandchild", res.WaitingFor[0].ChildSessionID)
}

func TestAwaitTasksWorkerIdentityIncompleteRead(t *testing.T) {
	t.Parallel()
	for _, incomplete := range []session.RootLiveWork{{OwnIncomplete: true}, {DescendantsIncomplete: true}} {
		res, reads, err := runAwaitSequence(t, "all", 0,
			awaitWork("A"), incomplete, awaitWork("A"), awaitWork())
		require.NoError(t, err)
		require.Equal(t, 4, reads, "incomplete read must not finish A")
		require.True(t, res.TimedOut)
		require.Equal(t, 1, res.FinishedCount)
	}
}

func TestAwaitTasksWorkerIdentityCapAndMaxWait(t *testing.T) {
	t.Parallel()
	for _, maxWait := range []int{0, 60, 21600} {
		res, reads, err := runAwaitSequence(t, "all", maxWait,
			awaitWork("A", "B"), awaitWork("B", "C"), awaitWork("B", "C", "D"))
		require.NoError(t, err)
		require.Equal(t, 3, reads)
		require.True(t, res.Blocked)
		require.True(t, res.TimedOut)
		require.Equal(t, 1, res.FinishedCount)
		require.Len(t, res.WaitingFor, 1)
		require.Equal(t, "B", res.WaitingFor[0].ToolCallID)
	}
}

func TestAwaitTasksWorkerIdentityCancellation(t *testing.T) {
	t.Parallel()
	for _, readyPoll := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		poll := make(chan time.Time, 1)
		reads := 0
		_, err := awaitTasksWorker(ctx, "all", 0, func() session.RootLiveWork {
			reads++
			return awaitWork("A")
		}, func(time.Duration) (<-chan time.Time, <-chan time.Time, func()) {
			cancel()
			if readyPoll {
				poll <- time.Time{}
			}
			return poll, nil, func() {}
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, reads, "cancellation wins over a same-tick poll")
	}
}
