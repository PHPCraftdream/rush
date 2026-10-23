package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchAwaitSource is the agent-test fake of session.BatchLiveSource: one
// in-memory projection whose reads the test mutates between hints.
type batchAwaitSource struct {
	mu         sync.Mutex
	roots      []db.BatchNode
	levels     map[string][]db.BatchNode
	failOwners bool
	queried    []string
}

func (s *batchAwaitSource) BatchLiveNodesForOwners(ctx context.Context, owners []string) ([]db.BatchNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOwners {
		return nil, errors.New("injected owners failure")
	}
	return append([]db.BatchNode(nil), s.roots...), nil
}

func (s *batchAwaitSource) BatchLevelNodes(ctx context.Context, p db.BatchLevelNodesParams) ([]db.BatchNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queried = append(s.queried, p.RootID)
	return append([]db.BatchNode(nil), s.levels[p.RootID]...), nil
}

func batchAwaitNull(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func batchAwaitNode(owner, root, node, parent, kind, state string) db.BatchNode {
	n := db.BatchNode{
		NodeID: node, RootID: root, OwnerSessionID: owner, Kind: kind, State: state,
		HostID: "host", ToolCallID: batchAwaitNull("call-" + root),
	}
	if node == root {
		n.Notify = batchAwaitNull(batchNotifyAll)
	}
	if parent != "" {
		n.ParentID = batchAwaitNull(parent)
		n.Ordinal = 1
	}
	return n
}

// batchAwaitHarness wires the fake source into the R10a reader plus the
// await params with a test-controlled hint channel.
type batchAwaitHarness struct {
	t        *testing.T
	src      *batchAwaitSource
	rd       *session.BatchLiveReader
	hint     chan struct{}
	captured session.BatchCapturedRoots
	notify   map[string]string
	mu       sync.Mutex
	calls    int
	session  string
}

func newBatchAwaitHarness(t *testing.T, roots ...db.BatchNode) *batchAwaitHarness {
	h := &batchAwaitHarness{
		t:       t,
		src:     &batchAwaitSource{roots: roots, levels: map[string][]db.BatchNode{}},
		hint:    make(chan struct{}, 64),
		notify:  map[string]string{},
		session: "s1",
	}
	h.rd = session.NewBatchLiveReader(h.src, t.TempDir())
	h.rd.IsOwnHost = func(string) bool { return false }
	h.rd.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	return h
}

func (h *batchAwaitHarness) setLevel(root string, rows ...db.BatchNode) {
	h.src.mu.Lock()
	defer h.src.mu.Unlock()
	h.src.levels[root] = rows
}

func (h *batchAwaitHarness) evalCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *batchAwaitHarness) params(mode string, maxWait int) BatchAwaitParams {
	return BatchAwaitParams{
		Mode: mode, MaxWaitSeconds: maxWait,
		Capture: func(ctx context.Context) session.BatchCapturedRoots {
			h.captured = h.rd.BatchCaptured(ctx, []string{h.session})
			return h.captured
		},
		Levels: func(ctx context.Context) session.BatchCapturedLevels {
			h.mu.Lock()
			h.calls++
			h.mu.Unlock()
			return h.rd.BatchCapturedLevel(ctx, h.captured)
		},
		Hints: func() (<-chan struct{}, func()) { return h.hint, func() {} },
		NotifyModeOf: func(ref session.BatchLiveRef) string {
			if h.notify[ref.RootID] == "each" {
				return "each"
			}
			return "all"
		},
		PollInterval: time.Hour, // hints only, unless a test overrides it
		WaitCap:      2 * time.Second,
	}
}

func (h *batchAwaitHarness) fireHint() {
	select {
	case h.hint <- struct{}{}:
	default:
	}
}

const batchAwaitOwner = "s1"

func batchAwaitRootRow(root, state string) db.BatchNode {
	return batchAwaitNode(batchAwaitOwner, root, root, "", "batch", state)
}

func batchAwaitChildRow(root, node, state string) db.BatchNode {
	return batchAwaitNode(batchAwaitOwner, root, node, root, "task", state)
}

func TestBatchAwaitNotifyEachStoredRootMode(t *testing.T) {
	root := batchAwaitRootRow("each-root", "running")
	root.Notify = batchAwaitNull(batchNotifyEach)
	h := newBatchAwaitHarness(t, root)
	h.setLevel("each-root", root, batchAwaitChildRow("each-root", "each-root.1", "running"))
	levels := h.rd.BatchCapturedLevel(context.Background(), h.rd.BatchCaptured(context.Background(), []string{h.session}))
	modes := batchAwaitNotifyModes(levels)
	require.Equal(t, batchNotifyEach, modes[session.BatchLiveRef{OwnerSessionID: h.session, RootID: "each-root"}])
	finished, remaining := batchAwaitEvaluateFor("any", []batchAwaitTarget{{ref: session.BatchLiveRef{OwnerSessionID: h.session, RootID: "each-root"}, notifyAll: modes[session.BatchLiveRef{OwnerSessionID: h.session, RootID: "each-root"}] != batchNotifyEach}}, levels)
	require.Empty(t, finished)
	require.Len(t, remaining, 1)
	h.setLevel("each-root", root, batchAwaitChildRow("each-root", "each-root.1", "completed"))
	levels = h.rd.BatchCapturedLevel(context.Background(), h.rd.BatchCaptured(context.Background(), []string{h.session}))
	finished, remaining = batchAwaitEvaluateFor("any", []batchAwaitTarget{{ref: session.BatchLiveRef{OwnerSessionID: h.session, RootID: "each-root"}, notifyAll: false}}, levels)
	require.Len(t, finished, 1)
	require.Empty(t, remaining)
}

func TestBatchAwaitAnyFirstUnit(t *testing.T) {
	t.Parallel()
	a, b := "r1", "r2"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"), batchAwaitRootRow(b, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"), batchAwaitChildRow(a, a+".1", "running"))
	h.setLevel(b, batchAwaitRootRow(b, "running"))
	done := make(chan BatchAwaitResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
		done <- res
		errCh <- err
	}()
	h.setLevel(a, batchAwaitRootRow(a, "completed"))
	h.fireHint()
	res := <-done
	require.NoError(t, <-errCh)
	require.False(t, res.TimedOut)
	require.Equal(t, "any", res.Mode)
	require.Len(t, res.Finished, 1)
	require.Equal(t, a, res.Finished[0].Ref.RootID)
	require.Len(t, res.Remaining, 1)
	require.GreaterOrEqual(t, h.evalCalls(), 1, "evaluated at entry")
}

func TestBatchAwaitAllWaitsForBothRoots(t *testing.T) {
	t.Parallel()
	a, b := "r1", "r2"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"), batchAwaitRootRow(b, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	h.setLevel(b, batchAwaitRootRow(b, "running"))
	type outcome struct {
		res BatchAwaitResult
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := BatchAwaitWorker(context.Background(), h.params("all", 0))
		out <- outcome{res, err}
	}()
	h.setLevel(a, batchAwaitRootRow(a, "completed"))
	h.fireHint()
	select {
	case o := <-out:
		t.Fatalf("all returned after one root: %+v %v", o.res, o.err)
	default:
	}
	h.setLevel(b, batchAwaitRootRow(b, "completed"))
	h.fireHint()
	o := <-out
	require.NoError(t, o.err)
	require.False(t, o.res.TimedOut)
	require.Empty(t, o.res.Remaining)
	require.Len(t, o.res.Finished, 2)
}

func TestBatchAwaitRefusalWhenNothingCaptured(t *testing.T) {
	t.Parallel()
	h := newBatchAwaitHarness(t)
	_, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
	require.ErrorContains(t, err, "nothing is running; continue or finish")
}

func TestBatchAwaitIncompleteZeroCaptureWaits(t *testing.T) {
	t.Parallel()
	h := newBatchAwaitHarness(t)
	h.src.mu.Lock()
	h.src.failOwners = true
	h.src.mu.Unlock()
	p := h.params("any", 0)
	p.WaitCap = 20 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err, "incomplete capture with zero refs must not refuse")
	require.True(t, res.TimedOut)
	require.Empty(t, res.Finished)
	require.Empty(t, res.Remaining)
}

func TestBatchAwaitIncompleteReadRemovesNothing(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	// Owner-mismatched rows make the level read incomplete without a
	// disappearance, so the root must NOT settle.
	h.setLevel(a, batchAwaitNode("other", a, a, "", "batch", "running"))
	p := h.params("any", 0)
	p.WaitCap = 20 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err)
	require.True(t, res.TimedOut)
	require.Empty(t, res.Finished, "incomplete read removes nothing")
	require.Len(t, res.Remaining, 1)
}

func TestBatchAwaitTimeoutReportsRemaining(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	p := h.params("any", 0)
	p.WaitCap = 20 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err, "timeout reports remaining without an error")
	require.True(t, res.TimedOut)
	require.Len(t, res.Remaining, 1)
}

func TestBatchAwaitMaxWaitBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		maxWait int
		wantErr string
	}{
		{10, "max_wait_seconds must be between"},
		{30000, "max_wait_seconds must be between"},
		{60, "nothing is running"},
		{21600, "nothing is running"},
		{0, "nothing is running"},
	} {
		h := newBatchAwaitHarness(t)
		_, err := BatchAwaitWorker(context.Background(), h.params("any", tc.maxWait))
		require.ErrorContains(t, err, tc.wantErr, "max_wait=%d", tc.maxWait)
	}
	// Injected bounds are validated like the defaults.
	h := newBatchAwaitHarness(t)
	p := h.params("any", 5)
	p.MinWaitSeconds, p.MaxWaitBounds = 10, 20
	_, err := BatchAwaitWorker(context.Background(), p)
	require.ErrorContains(t, err, "max_wait_seconds must be between")
}

func TestBatchAwaitContextCancel(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := BatchAwaitWorker(ctx, h.params("any", 0))
		errCh <- err
	}()
	cancel()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the wait")
	}
}

func TestBatchAwaitNewRootAfterCaptureExcluded(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	go func() {
		h.fireHint()
	}()
	res, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
	require.NoError(t, err)
	// A second root became visible in the owners read only after capture.
	h.src.mu.Lock()
	defer h.src.mu.Unlock()
	require.NotContains(t, h.src.queried, "r2", "levels never reread uncaptured refs")
	for _, unit := range append(res.Finished, res.Remaining...) {
		require.Equal(t, a, unit.Ref.RootID, "result units are captured roots only")
	}
}

func TestBatchAwaitAllRequiresRootsNotChildren(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"), batchAwaitChildRow(a, a+".1", "completed"))
	p := h.params("all", 0)
	p.WaitCap = 20 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err)
	require.True(t, res.TimedOut, "child settlement must not finish ALL")
	require.Empty(t, res.Finished)
	require.Len(t, res.Remaining, 1)
}

// ALL under NotifyEach: a terminal direct child is the root's first await
// unit for ANY, but it must not finish ALL while the root itself is live.
func TestBatchAwaitAllNotifyEachChildDoesNotFinish(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"), batchAwaitChildRow(a, a+".1", "completed"))
	h.notify[a] = "each"
	p := h.params("all", 0)
	p.WaitCap = 20 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err)
	require.True(t, res.TimedOut, "a settled child alone must not finish ALL under NotifyEach")
	require.Empty(t, res.Finished)
	require.Len(t, res.Remaining, 1)
	require.True(t, res.Remaining[0].ChildFinished, "the child settlement is still reported")

	h.setLevel(a, batchAwaitRootRow(a, "completed"), batchAwaitChildRow(a, a+".1", "completed"))
	p = h.params("all", 0)
	res, err = BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err)
	require.False(t, res.TimedOut)
	require.Len(t, res.Finished, 1)
	require.False(t, res.Finished[0].ChildFinished)
}

func TestBatchAwaitNotifyEachLaterChildVisible(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	h.notify[a] = "each"
	type outcome struct {
		res BatchAwaitResult
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
		out <- outcome{res, err}
	}()
	// A later Add appends a direct child that is already terminal.
	h.setLevel(a, batchAwaitRootRow(a, "running"), batchAwaitChildRow(a, a+".1", "completed"))
	h.fireHint()
	o := <-out
	require.NoError(t, o.err)
	require.False(t, o.res.TimedOut)
	require.Len(t, o.res.Finished, 1)
	require.True(t, o.res.Finished[0].ChildFinished)
	require.Equal(t, "call-"+a, o.res.Finished[0].ToolCallID)
}

func TestBatchAwaitDisappearedNeverFinished(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"any", "all"} {
		a := "r1"
		h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
		// A complete read whose root row is gone means disappearance.
		h.setLevel(a, batchAwaitChildRow(a, a+".1", "completed"))
		res, err := BatchAwaitWorker(context.Background(), h.params(mode, 0))
		require.NoError(t, err)
		require.False(t, res.TimedOut, "disappearance breaks the wait immediately")
		require.Empty(t, res.Finished, "a disappeared root is never finished")
		require.Len(t, res.Remaining, 1)
		require.True(t, res.Remaining[0].Disappeared)
	}
}

func TestBatchAwaitNoLostWakeBetweenCaptureAndWait(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "completed"))
	// No hint is ever sent; the initial evaluation must catch it.
	res, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
	require.NoError(t, err)
	require.False(t, res.TimedOut)
	require.Len(t, res.Finished, 1)
	require.Equal(t, 1, h.evalCalls(), "exactly the initial evaluation")
}

func TestBatchAwaitPollFallbackTick(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "completed"))
	p := h.params("any", 0)
	p.PollInterval = 5 * time.Millisecond
	res, err := BatchAwaitWorker(context.Background(), p)
	require.NoError(t, err)
	require.False(t, res.TimedOut)
	require.Len(t, res.Finished, 1)
}

func TestBatchAwaitHintStormOneReturn(t *testing.T) {
	t.Parallel()
	a := "r1"
	h := newBatchAwaitHarness(t, batchAwaitRootRow(a, "running"))
	h.setLevel(a, batchAwaitRootRow(a, "running"))
	go func() {
		for i := 0; i < 64; i++ {
			h.fireHint()
		}
	}()
	h.setLevel(a, batchAwaitRootRow(a, "completed"))
	res, err := BatchAwaitWorker(context.Background(), h.params("any", 0))
	require.NoError(t, err)
	require.Len(t, res.Finished, 1)
	require.Len(t, res.Remaining, 0)
	require.Less(t, h.evalCalls(), 70, "one return per call despite many hints")
}

func TestBatchAwaitModeValidation(t *testing.T) {
	t.Parallel()
	h := newBatchAwaitHarness(t, batchAwaitRootRow("r1", "running"))
	_, err := BatchAwaitWorker(context.Background(), h.params("sometimes", 0))
	require.ErrorContains(t, err, "mode must be")
}

// batchAwaitFakes are the injected fact reads for the arbiter/scope tests.
func batchArbDeps(live session.BatchLiveResult, debt session.BatchPendingDebt, debtErr error) BatchArbiterDeps {
	return BatchArbiterDeps{
		Live: func(context.Context) session.BatchLiveResult { return live },
		Debt: func(context.Context) (session.BatchPendingDebt, error) { return debt, debtErr },
	}
}

func TestBatchArbiterFactsRuleOneDebt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, err := ReadBatchArbiterFacts(ctx, batchArbDeps(session.BatchLiveResult{Complete: true}, session.BatchPendingDebt{}, nil))
	require.NoError(t, err)
	require.True(t, f.Complete)
	require.False(t, f.DebtPendingIncl, "empty complete read is no debt")
	f, err = ReadBatchArbiterFacts(ctx, batchArbDeps(session.BatchLiveResult{Complete: true},
		session.BatchPendingDebt{Nodes: []session.BatchDebtNodeRef{{RootID: "r", NodeID: "n"}}}, nil))
	require.NoError(t, err)
	require.True(t, f.DebtPendingIncl)
	f, err = ReadBatchArbiterFacts(ctx, batchArbDeps(session.BatchLiveResult{Complete: true},
		session.BatchPendingDebt{}, errors.New("debt read failed")))
	require.NoError(t, err)
	require.False(t, f.Complete)
	require.True(t, f.DebtPendingIncl, "unknown debt stays pending")
}

func TestBatchArbiterFactsLiveRoots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Pending, running (held report) and Cancelling roots all stay live.
	for _, state := range []string{"pending", "running", "cancelling"} {
		live := session.BatchLiveResult{Complete: true, Tree: []session.BatchLiveNode{{
			Ref: session.BatchLiveRef{OwnerSessionID: "s1", RootID: "r1"}, State: state,
		}}}
		f, err := ReadBatchArbiterFacts(ctx, batchArbDeps(live, session.BatchPendingDebt{}, nil))
		require.NoError(t, err, state)
		require.True(t, f.LiveRoots, state)
	}
	f, err := ReadBatchArbiterFacts(ctx, batchArbDeps(
		session.BatchLiveResult{Complete: false}, session.BatchPendingDebt{}, nil))
	require.NoError(t, err)
	require.True(t, f.LiveRoots, "incomplete live read is conservatively live")
	f, err = ReadBatchArbiterFacts(ctx, batchArbDeps(session.BatchLiveResult{Complete: true},
		session.BatchPendingDebt{}, nil))
	require.NoError(t, err)
	require.False(t, f.LiveRoots, "complete empty read is not live")
}

func TestBatchArbiterFactsStopIntentNotCompletion(t *testing.T) {
	t.Parallel()
	// Stop intent creates no debt row: complete reads show no debt, so rule
	// 1 gives VNone and the sleep predicate can never hold on stop alone.
	f, err := ReadBatchArbiterFacts(context.Background(), batchArbDeps(
		session.BatchLiveResult{Complete: true}, session.BatchPendingDebt{}, nil))
	require.NoError(t, err)
	require.False(t, f.DebtPendingIncl)
	require.False(t, f.StopIntentIsNotCompletion(), "stop intent is not a completion")
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{}))
}

func TestBatchArbiterFactsCompletionOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	liveRoots := session.BatchLiveResult{Complete: true, Tree: []session.BatchLiveNode{{
		Ref: session.BatchLiveRef{OwnerSessionID: "s1", RootID: "r1"}, State: "running",
	}}}
	completion := session.BatchPendingDebt{Notices: []session.PendingNoticeDebt{{ID: 1, Kind: "task"}}}
	f, err := ReadBatchArbiterFacts(ctx, batchArbDeps(liveRoots, completion, nil))
	require.NoError(t, err)
	require.True(t, f.CompletionOnly, "selected completion notices are completion-class")
	require.True(t, f.SleepHolds(true, BatchSleepBreakers{}), "completion-only debt with live roots defers")
	f, err = ReadBatchArbiterFacts(ctx, batchArbDeps(session.BatchLiveResult{Complete: true}, completion, nil))
	require.NoError(t, err)
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{}), "no live roots, no deferral")
	question := session.BatchPendingDebt{Notices: []session.PendingNoticeDebt{{ID: 2, Kind: "child_question"}}}
	f, err = ReadBatchArbiterFacts(ctx, batchArbDeps(liveRoots, question, nil))
	require.NoError(t, err)
	require.False(t, f.CompletionOnly, "child_question is not completion-class")
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{}))
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{QuestionDebt: true}), "question breaks sleep")
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{WakeFired: true}))
	require.False(t, f.SleepHolds(true, BatchSleepBreakers{MaxWaitExpired: true}))
	require.False(t, f.SleepHolds(false, BatchSleepBreakers{}), "no sleep armed, no deferral")
}

func TestBatchScopeWorkOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := func(live session.BatchLiveResult, debt session.BatchPendingDebt, debtErr error) BatchScopeDeps {
		return BatchScopeDeps{
			Live: func(context.Context) session.BatchLiveResult { return live },
			Debt: func(context.Context) (session.BatchPendingDebt, error) { return debt, debtErr },
		}
	}
	f := ReadBatchScopeFacts(ctx, scope(session.BatchLiveResult{Complete: true, Tree: []session.BatchLiveNode{{Ref: session.BatchLiveRef{OwnerSessionID: "s1", RootID: "r1"}, State: "running"}}}, session.BatchPendingDebt{}, nil))
	require.True(t, f.WorkOpen && f.Complete)
	f = ReadBatchScopeFacts(ctx, scope(session.BatchLiveResult{Complete: true},
		session.BatchPendingDebt{Nodes: []session.BatchDebtNodeRef{{RootID: "r", NodeID: "n"}}}, nil))
	require.True(t, f.WorkOpen, "debt alone keeps the scope open")
	f = ReadBatchScopeFacts(ctx, scope(session.BatchLiveResult{Complete: true}, session.BatchPendingDebt{}, nil))
	require.True(t, f.Complete)
	require.False(t, f.WorkOpen, "neither live roots nor debt closes the scope")
	f = ReadBatchScopeFacts(ctx, scope(session.BatchLiveResult{Complete: false}, session.BatchPendingDebt{}, nil))
	require.True(t, f.WorkOpen, "incomplete reads keep the scope open")
	f = ReadBatchScopeFacts(ctx, scope(session.BatchLiveResult{Complete: true}, session.BatchPendingDebt{}, errors.New("x")))
	require.True(t, f.WorkOpen)
}

// TestBatchScopeIndependentOnceWake pins that scope lifetime (once wakes,
// completion holds) is NOT folded into WorkOpen: the batch scope fact knows
// only roots and debt, and the CLI caller combines them.
func TestBatchScopeIndependentOnceWake(t *testing.T) {
	t.Parallel()
	f := ReadBatchScopeFacts(context.Background(), BatchScopeDeps{
		Live: func(context.Context) session.BatchLiveResult {
			return session.BatchLiveResult{Complete: true}
		},
		Debt: func(context.Context) (session.BatchPendingDebt, error) {
			return session.BatchPendingDebt{}, nil
		},
	})
	require.True(t, f.Complete)
	require.False(t, f.WorkOpen, "an open once wake alone must not open WorkOpen")
}
