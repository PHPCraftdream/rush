package jobbatch

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// waitRecordAfter waits until a record of the given type and node is
// published at or after cursor and returns the cursor to resume from.
// Unlike runtimeTestRecord it never re-reads earlier records, so repeated
// waits observe each cycle's own rejection instead of the first one.
func waitRecordAfter(t *testing.T, ctx context.Context, r *Runner, typ RecordType, node ID, cursor uint64) uint64 {
	t.Helper()
	for {
		p, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range p.Records {
			if rec.Type == typ && rec.Node == node {
				return rec.Cursor + 1
			}
		}
		if p.Final != 0 {
			t.Fatalf("feed closed before %s record for %s", typ, node)
		}
		cursor = p.Next
	}
}

// Catches the durable Release gaps: the speculative fence never transferred,
// published attempts/views/snapshots/feed entries of the released root
// survived, and an unrelated transition accepted behind the publication
// barrier could republish removed state. The overlap is deterministic: the
// transitioned seam fires only after B's engine apply and capture are
// queued, and the held-window feed length proves B was captured before the
// Release publication pruned.
func TestRuntimeDurableReleasePrunesWithoutRepublish(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	releaseAccepted := make(chan struct{}, 2)
	bCaptured := make(chan Transition, 4)
	lateMessages := make(chan runnerMessageKind, 4)
	var aToken atomic.Uint64
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventRelease {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{
		accepted: func(q *runnerRequest) {
			if q.event.Type == EventRelease && q.event.Root == "A" {
				releaseAccepted <- struct{}{}
			}
		},
		transitioned: func(tr Transition) {
			if tr.Event.Type == EventSubmit && tr.Event.Root == "B" {
				bCaptured <- tr
			}
		},
		afterMessage: func(m runnerMessage) {
			if m.token != 0 && m.token == aToken.Load() && (m.kind == runnerStopResult || m.kind == runnerStopReturned) {
				lateMessages <- m.kind
			}
		},
	})

	bA := runtimeTestSubmit(t, ctx, r, "A", OneItem(TaskSpec{Kind: "test"}))
	stopEntered := make(chan struct{})
	unblock := make(chan struct{})
	aA := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopEntered <- struct{}{}; <-unblock; return nil }})
	aToken.Store(aA.launch.Token)
	require.NoError(t, r.Stop(ctx, "A.1"))
	runtimeTestReceive(t, ctx, stopEntered)
	runtimeTestFinish(t, ctx, r, aA, StateCancelled)
	runtimeTestWait(t, ctx, bA)

	released := make(chan error, 1)
	go func() { released <- r.Release(ctx, "A") }()
	runtimeTestReceive(t, ctx, releaseAccepted)
	// Submit B behind the held Release commit. The Release request was
	// accepted first, so its publication will prune before B publishes.
	submitted := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "B", Spec: OneItem(TaskSpec{Kind: "test"})})
		submitted <- b
	}()
	runtimeTestReceive(t, ctx, bCaptured)
	// Quiescent window: the loop is parked sending the held publication to
	// the blocked committer, so loop-owned maps can be read. Four known ids
	// prove B's capture happened before the pruning.
	r.mu.Lock()
	knownHeld := len(r.feed.known)
	held := runnerCopySnapshot(r.snapshots["A"])
	r.mu.Unlock()
	require.Equal(t, 4, knownHeld, "B was not captured while the Release commit was held")
	if held.Node.State != StateCancelled {
		t.Fatalf("settled root snapshot missing behind the barrier: %+v", held)
	}
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, released))
	bB := runtimeTestReceive(t, ctx, submitted)

	// Unblock the stop that was in flight across the pruning: its result and
	// returned messages reach the Runner after A's attempt was pruned and
	// must be processed without effect.
	close(unblock)
	require.Equal(t, runnerStopResult, runtimeTestReceive(t, ctx, lateMessages))
	require.Equal(t, runnerStopReturned, runtimeTestReceive(t, ctx, lateMessages))

	aB := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, aB, StateCompleted)
	runtimeTestWait(t, ctx, bB)

	r.mu.Lock()
	knownAfter := len(r.feed.known)
	speculative := len(r.speculativeReleased)
	attemptRoots := map[ID]int{}
	for _, a := range r.attempts {
		attemptRoots[a.root]++
	}
	_, snapshotA := r.snapshots["A"]
	_, snapshotA1 := r.snapshots["A.1"]
	_, frozenA := r.frozenSnapshots["A"]
	_, frozenA1 := r.frozenSnapshots["A.1"]
	r.mu.Unlock()
	require.Equal(t, 2, knownAfter, "released root still enumerated by the durable feed")
	require.Zero(t, speculative, "published release fence not transferred")
	require.NotContains(t, attemptRoots, ID("A"), "durable Release left the released root's attempts")
	require.Equal(t, 1, attemptRoots[ID("B")], "unrelated root attempt disturbed")
	require.False(t, snapshotA || snapshotA1, "removed state republished after Release")
	require.False(t, frozenA || frozenA1, "removed frozen state republished after Release")

	_, err := r.View(ctx, "A")
	require.ErrorIs(t, err, ErrReleased)
	_, err = r.Snapshot(ctx, "A")
	require.ErrorIs(t, err, ErrReleased)
	require.ErrorIs(t, r.Stop(ctx, "A.1"), ErrReleased)
	require.ErrorIs(t, r.Release(ctx, "A"), ErrReleased)
	_ = runtimeTestView(t, ctx, r, "B.1")

	page := runtimeTestPage(t, ctx, r)
	releaseAt := -1
	for i, rec := range page.Records {
		if rec.Type == RecordRelease && rec.Node == "A" {
			releaseAt = i
		}
	}
	require.NotEqual(t, -1, releaseAt, "release record missing")
	for _, rec := range page.Records[releaseAt+1:] {
		require.NotEqual(t, ID("A"), rec.Root, "record after release references the released root: %v", rec)
		require.NotEqual(t, ID("A"), rec.Node, "record after release references the released root: %v", rec)
	}

	runtimeTestClose(t, ctx, r)
	r.mu.Lock()
	_, snapshotAFinal := r.snapshots["A"]
	r.mu.Unlock()
	require.False(t, snapshotAFinal, "Final publication resurrected the released root")
	_, err = r.View(ctx, "A.1")
	require.ErrorIs(t, err, ErrReleased)
}

// Guards the pre-existing observable: a settled-but-unreleased root's
// published snapshot keeps advancing BatchView.Sequence with unrelated
// transitions. Fails on any design that freezes settled snapshots.
func TestRuntimeDurableSettledSnapshotSequenceAdvances(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	bA := runtimeTestSubmit(t, ctx, r, "a", OneItem(TaskSpec{Kind: "test"}))
	aA := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, aA, StateCompleted)
	runtimeTestWait(t, ctx, bA)
	r.mu.Lock()
	frozen := runnerCopySnapshot(r.snapshots["a"])
	r.mu.Unlock()
	if frozen.Sequence == 0 {
		t.Fatalf("no published snapshot for a: %+v", frozen)
	}
	bB := runtimeTestSubmit(t, ctx, r, "b", OneItem(TaskSpec{Kind: "test"}))
	aB := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	started := runtimeTestRecord(t, ctx, r, RecordStarted, "b.1")
	r.mu.Lock()
	advanced := runnerCopySnapshot(r.snapshots["a"])
	r.mu.Unlock()
	if advanced.Sequence != started.Sequence {
		t.Fatalf("settled snapshot sequence %d, want the later transition %d", advanced.Sequence, started.Sequence)
	}
	if advanced.Counts != frozen.Counts || len(advanced.Descendants) != len(frozen.Descendants) {
		t.Fatalf("settled snapshot content changed: %+v vs %+v", advanced, frozen)
	}
	runtimeTestFinish(t, ctx, r, aB, StateCompleted)
	runtimeTestWait(t, ctx, bB)
	runtimeTestClose(t, ctx, r)
}

// Catches an append-only handle ledger: sequential successful submit, settle
// and release cycles on one open durable runner must leave the ledger
// bounded. Fails when handles are removed only by Close or fault draining.
func TestRuntimeDurableSuccessfulDisposeCompactsHandles(t *testing.T) {
	t.Parallel()
	const n = 16
	returned := make(chan struct{}, 8*n)
	hooks := &runnerRequestHooks{controlReturned: func() { returned <- struct{}{} }}
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, hooks)
	for i := 0; i < n; i++ {
		root := ID(fmt.Sprintf("r%d", i))
		b := runtimeTestSubmit(t, ctx, r, root, OneItem(TaskSpec{Kind: "test"}))
		disposed := make(chan struct{}, 1)
		a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }})
		runtimeTestFinish(t, ctx, r, a, StateCompleted)
		runtimeTestWait(t, ctx, b)
		released := make(chan error, 1)
		go func() { released <- r.Release(ctx, root) }()
		require.NoError(t, runtimeTestReceive(t, ctx, released))
		runtimeTestReceive(t, ctx, disposed)
		runtimeTestReceive(t, ctx, returned)
	}
	r.mu.Lock()
	handles := len(r.handles)
	r.mu.Unlock()
	if handles > 2 {
		t.Fatalf("handle ledger grew to %d over %d successful cycles", handles, n)
	}
	runtimeTestClose(t, ctx, r)
}

// Catches durable capacity-rejected attempts accumulating on one root: each
// rejection retires the attempt, and before the supersede prune nothing
// deleted it until Release, so N reject/retry cycles on a long-lived root
// retained N Launch payloads. Fails without the supersede prune in
// indexAttempt. Also proves a late completion for the superseded first
// token still reaches the Runner and is ignored without effect.
func TestRuntimeDurableSupersededAttemptsBoundedAndLateReportSafe(t *testing.T) {
	t.Parallel()
	const n = 8
	lateCompletions := make(chan struct{}, n)
	var firstToken atomic.Uint64
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, &runnerRequestHooks{
		afterMessage: func(m runnerMessage) {
			if m.kind == runnerCompletion && firstToken.Load() != 0 && m.token == firstToken.Load() {
				lateCompletions <- struct{}{}
			}
		},
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Group: "g"}))
	first := runtimeTestReceive(t, ctx, e.starts)
	firstToken.Store(first.launch.Token)
	first.decision <- runtimeTestDecision{err: ErrCapacity}
	// Wait for this cycle's own rejection record: it exists only after the
	// rejection was applied and published, so the next capacity signal cannot
	// overtake it.
	cursor := waitRecordAfter(t, ctx, r, RecordStartRejected, "r.1", 1)
	for i := 1; i < n; i++ {
		require.NoError(t, r.CapacityAvailable(ctx, "g"))
		retry := runtimeTestReceive(t, ctx, e.starts)
		retry.decision <- runtimeTestDecision{err: ErrCapacity}
		cursor = waitRecordAfter(t, ctx, r, RecordStartRejected, "r.1", cursor)
	}
	require.NoError(t, r.CapacityAvailable(ctx, "g"))
	last := runtimeTestReceive(t, ctx, e.starts)
	last.decision <- runtimeTestDecision{execution: &runtimeTestExecution{}}
	runtimeTestFinish(t, ctx, r, last, StateCompleted)
	runtimeTestWait(t, ctx, b)
	r.mu.Lock()
	attempts := len(r.attempts)
	r.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("%d attempts retained after %d superseded rejections on one root", attempts, n)
	}
	// The superseded first token was pruned; its late completion reaches the
	// Runner, finds no attempt, and is ignored.
	first.report(Result{State: StateCompleted})
	runtimeTestReceive(t, ctx, lateCompletions)
	r.mu.Lock()
	attemptsAfter := len(r.attempts)
	snapshotR := r.snapshots["r"]
	r.mu.Unlock()
	if attemptsAfter != 1 {
		t.Fatalf("late completion changed the attempt count to %d", attemptsAfter)
	}
	if snapshotR.Counts.Completed != 1 || snapshotR.Counts.Failed != 0 {
		t.Fatalf("late completion altered the settled snapshot: %+v", snapshotR)
	}
	require.NoError(t, r.Release(ctx, "r"))
	r.mu.Lock()
	afterRelease := len(r.attempts)
	r.mu.Unlock()
	if afterRelease != 0 {
		t.Fatalf("%d attempts survived the release", afterRelease)
	}
	runtimeTestClose(t, ctx, r)
}

// Catches the inline re-authorisation ordering: the durable apply creates the
// re-admitted attempt before it retires the rejected one, so indexAttempt
// cannot see the retirement; without dropSuperseded the rejected attempt
// survives alongside its successor. Deterministic: the capacity signal lands
// while the first attempt is still active, so the StartRejected transition
// re-admits r.1 in the same transition.
func TestRuntimeDurableInlineReauthPrunesSuperseded(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Group: "g"}))
	first := runtimeTestReceive(t, ctx, e.starts)
	// Signal capacity while the first attempt is active: the generation bump
	// is recorded, and the rejection below re-admits r.1 inline.
	require.NoError(t, r.CapacityAvailable(ctx, "g"))
	first.decision <- runtimeTestDecision{err: ErrCapacity}
	waitRecordAfter(t, ctx, r, RecordStartRejected, "r.1", 1)
	second := runtimeTestReceive(t, ctx, e.starts)
	second.decision <- runtimeTestDecision{execution: &runtimeTestExecution{}}
	runtimeTestFinish(t, ctx, r, second, StateCompleted)
	runtimeTestWait(t, ctx, b)
	r.mu.Lock()
	attempts := len(r.attempts)
	r.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("inline re-authorisation left %d attempts (want 1: the superseded first attempt must be dropped)", attempts)
	}
	require.NoError(t, r.Release(ctx, "r"))
	r.mu.Lock()
	afterRelease := len(r.attempts)
	r.mu.Unlock()
	if afterRelease != 0 {
		t.Fatalf("%d attempts survived the release", afterRelease)
	}
	runtimeTestClose(t, ctx, r)
}

// Catches durable Release leaving a root's retained accepted attempt behind:
// after the supersede prune only the accepted, settled attempt is still
// retained, and the root-keyed prune must remove it at Release.
func TestRuntimeDurableReleasePrunesRetainedAttempts(t *testing.T) {
	t.Parallel()
	const n = 8
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	for i := 0; i < n; i++ {
		root := ID(fmt.Sprintf("r%d", i))
		b := runtimeTestSubmit(t, ctx, r, root, OneItem(TaskSpec{Kind: "test"}))
		a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
		runtimeTestFinish(t, ctx, r, a, StateCompleted)
		runtimeTestWait(t, ctx, b)
		require.NoError(t, r.Release(ctx, root))
	}
	r.mu.Lock()
	afterRelease := len(r.attempts)
	r.mu.Unlock()
	if afterRelease != 0 {
		t.Fatalf("%d attempts survived their releases", afterRelease)
	}
	runtimeTestClose(t, ctx, r)
}

// Catches a frozen cache that re-derives terminal task ids on every
// transition: Engine.Snapshot returns false for tasks, so without caching
// the miss the capture loop repeats the failed Snapshot call for every
// retained leaf on every transition. The cache entry is the observable
// proxy; the repeated call itself is internal.
func TestRuntimeDurableFrozenCacheCoversTaskIds(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	bA := runtimeTestSubmit(t, ctx, r, "a", OneItem(TaskSpec{Kind: "test"}))
	aA := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, aA, StateCompleted)
	runtimeTestWait(t, ctx, bA)
	bB := runtimeTestSubmit(t, ctx, r, "b", OneItem(TaskSpec{Kind: "test"}))
	aB := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, aB, StateCompleted)
	runtimeTestWait(t, ctx, bB)
	r.mu.Lock()
	_, batchFrozen := r.frozenSnapshots["a"]
	taskFrozen, taskCached := r.frozenSnapshots["a.1"]
	_, taskPublished := r.snapshots["a.1"]
	r.mu.Unlock()
	require.True(t, batchFrozen, "settled batch not frozen")
	require.True(t, taskCached, "terminal task id not cached after the first transition")
	require.Equal(t, BatchView{}, taskFrozen, "task cache entry must stay contentless")
	require.False(t, taskPublished, "task ids must not gain published snapshots")
	_, err := r.Snapshot(ctx, "a.1")
	require.ErrorIs(t, err, ErrUnsupported)
	runtimeTestClose(t, ctx, r)
}
