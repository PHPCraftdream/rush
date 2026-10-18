package jobbatch

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

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
	runtimeTestFinish(t, ctx, r, aA, StateCompleted)
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
	// Stop latched the cause, so the settled root reads cancelled; the point
	// here is only that its terminal snapshot is still retained.
	if !held.Node.State.Terminal() {
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

// Catches durable capacity-rejected attempts surviving Release: the rejected
// token is retired but never deleted, so every reject-then-succeed cycle
// leaked one attempt with its Launch payload. Fails without the root-keyed
// attempt index.
func TestRuntimeDurableReleasePrunesRejectedAttempts(t *testing.T) {
	t.Parallel()
	const n = 8
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	for i := 0; i < n; i++ {
		root := ID(fmt.Sprintf("r%d", i))
		b := runtimeTestSubmit(t, ctx, r, root, OneItem(TaskSpec{Kind: "test", Group: "g"}))
		rejected := runtimeTestReceive(t, ctx, e.starts)
		rejected.decision <- runtimeTestDecision{err: ErrCapacity}
		runtimeTestRecord(t, ctx, r, RecordStartRejected, root+".1")
		require.NoError(t, r.CapacityAvailable(ctx, "g"))
		a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
		if a.launch.ID != root+".1" {
			t.Fatalf("retry launch %q", a.launch.ID)
		}
		runtimeTestFinish(t, ctx, r, a, StateCompleted)
		runtimeTestWait(t, ctx, b)
		require.NoError(t, r.Release(ctx, root))
	}
	r.mu.Lock()
	attempts := len(r.attempts)
	r.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("%d attempts survived their releases", attempts)
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
