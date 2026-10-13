package session

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// R11B CommitHook adapter acceptance tests, part 2: notices, host closing,
// Final, cross-host fencing, concurrency and fault identity.

// Scenario 9: added children of a NotifyEach root produce per-child notices
// with the right wake/delivery.
func TestBatchCommitNotifyEachPerChildNotices(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r",
		commitHookBatch(jobbatch.NotifyEach, commitHookTask(), commitHookTask()), "call-r")
	a1, a2 := commitHookAcceptPair(t, ctx, r, e, nil, nil, "r.1", "r.2")
	commitHookFinish(t, ctx, r, a1, jobbatch.Result{State: jobbatch.StateCompleted})
	settledSeq := commitHookRecord(t, ctx, r, jobbatch.RecordSettled, "r.1").Sequence
	leaf1 := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, int64(1), leaf1.NoticeSelected)
	require.Equal(t, int64(1), leaf1.Wake)
	require.Equal(t, "pending", leaf1.Delivery)
	// Duplicate selection while the root is still live: the projection_sequence
	// fence matches (settled sequence on r.1) but the notice_selected guard
	// refuses, so the replay never rewrites the latched notice.
	_, err := env.store.ReserveIdentity(ctx, BatchReservation{
		Owner: env.id.Owner, Host: env.id.Host, RunnerID: env.id.Runner,
		ToolCallID: "call-r", InputHash: batchCommitSpecHash(
			commitHookBatch(jobbatch.NotifyEach, commitHookTask(), commitHookTask())),
		Sequence: int64(settledSeq),
		Notices: []db.SelectBatchNodeNoticeParams{{
			NodeID: "r.1", NoticeKind: batchStoreTestString("task"),
			NoticeState:   batchStoreTestString("completed"),
			NoticeOrdinal: sql.NullInt64{Int64: 7, Valid: true}, Wake: 0,
		}},
	})
	require.ErrorIs(t, err, ErrBatchProjectionConflict, "duplicate notice selection refused")
	require.Equal(t, "0", commitHookReadNode(t, env.conn, "r.1").NoticeOrdinal,
		"latched notice not rewritten by the duplicate selection")
	commitHookFinish(t, ctx, r, a2, jobbatch.Result{State: jobbatch.StateFailed, Code: "boom"})
	commitHookWait(t, ctx, b)
	leaf2 := commitHookReadNode(t, env.conn, "r.2")
	root := commitHookReadNode(t, env.conn, "r")
	require.Equal(t, int64(1), leaf2.NoticeSelected)
	require.Equal(t, int64(1), leaf2.Wake, "each child notice wakes")
	require.Equal(t, int64(1), commitHookCount(t, env.conn,
		"SELECT count(*) FROM batch_nodes WHERE node_id = 'r.2' AND notice_state = 'failed'"))
	require.Equal(t, int64(1), root.NoticeSelected, "root settlement notified once")
	require.Equal(t, int64(3), commitHookCount(t, env.conn,
		"SELECT count(*) FROM batch_nodes WHERE root_id = 'r' AND notice_selected = 1"),
		"leaf r.1, leaf r.2 and root notified")
}

// Scenario 10 (M09, M10): HostClosing notices wake=0; already-latched causes
// are NOT rewritten; interruption facts are persisted.
func TestBatchCommitHostClosingWakeZeroLatchedCause(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r",
		commitHookBatch(jobbatch.NotifyEach, commitHookTask(), commitHookTask()), "call-r")
	stop1 := make(chan struct{})
	a1, a2 := commitHookAcceptPair(t, ctx, r, e, nil, func(context.Context) error {
		close(stop1)
		return nil
	}, "r.1", "r.2")
	// Latch cause=stop on r.1 and settle it cancelled.
	require.NoError(t, r.Stop(ctx, "r.1"))
	commitHookFinish(t, ctx, r, a1, jobbatch.Result{State: jobbatch.StateCancelled})
	closing := make(chan error, 1)
	go func() { closing <- r.Close(ctx) }()
	<-stop1 // r.2's physical stop is dispatched by the host-closing transition
	a2.report(jobbatch.Result{State: jobbatch.StateInterrupted})
	require.NoError(t, commitHookReceive(t, ctx, closing))
	commitHookWait(t, ctx, b)
	leaf1 := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, "stop", leaf1.Cause, "already-latched cause not rewritten by host closing")
	require.Equal(t, int64(1), leaf1.Wake, "earlier notice keeps its wake=1")
	leaf2 := commitHookReadNode(t, env.conn, "r.2")
	require.Equal(t, "interrupted", leaf2.State, "interruption facts persisted")
	require.Equal(t, "host_closing", leaf2.Cause)
	require.Equal(t, int64(0), leaf2.Wake, "host-closing notices never wake")
	require.Equal(t, "pending", leaf2.Delivery)
	seq, eventType, final := commitHookCheckpoint(t, env.conn, env.id.Runner)
	require.Equal(t, int64(1), final)
	require.Equal(t, "final", eventType)
	require.Positive(t, seq)
	// A later projection of the settled node writes the view verbatim; the
	// latched cause is never re-derived (direct adapter call, high sequence).
	require.NoError(t, env.adapter()(ctx, jobbatch.Transition{
		Sequence: 900,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1},
		Changed: []jobbatch.NodeView{{
			ID: "r.1", Root: "r", State: jobbatch.StateCancelled,
			Cause: jobbatch.CauseStop, Token: 1, Control: jobbatch.ControlAcked,
		}},
	}))
	require.Equal(t, "stop", commitHookReadNode(t, env.conn, "r.1").Cause,
		"verbatim cause preserved, never rewritten to host_closing")
}

// Scenario 11 (M11): Final writes the checkpoint and no synthetic settlement
// rows: nodes not mentioned by the final transitions stay untouched.
func TestBatchCommitFinalNoSyntheticSettlements(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r",
		commitHookBatch(jobbatch.NotifyAll, commitHookTask(), commitHookTask()), "call-r")
	a1, a2 := commitHookAcceptPair(t, ctx, r, e, nil, nil, "r.1", "r.2")
	commitHookFinish(t, ctx, r, a1, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookFinish(t, ctx, r, a2, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookWait(t, ctx, b)
	before := map[string]commitHookNodeRow{
		"r":   commitHookReadNode(t, env.conn, "r"),
		"r.1": commitHookReadNode(t, env.conn, "r.1"), "r.2": commitHookReadNode(t, env.conn, "r.2"),
	}
	require.NoError(t, r.Close(ctx))
	for id, row := range before {
		after := commitHookReadNode(t, env.conn, id)
		require.Equal(t, row, after, "Final must not synthesize settlements or touch rows")
	}
	seq, eventType, final := commitHookCheckpoint(t, env.conn, env.id.Runner)
	require.Equal(t, int64(1), final)
	require.Equal(t, "final", eventType)
	closed := commitHookRecord(t, ctx, r, jobbatch.RecordClosed, "")
	require.Equal(t, int64(closed.Sequence), seq, "checkpoint carries the Final sequence")
}

// Scenario 12 (M03): a live root owned by host A is refused to host B with
// the same owner/tool-call: ErrBatchForeignLiveHost, no new work, no rows.
func TestBatchCommitCrossHostAttachRefused(t *testing.T) {
	env := newCommitHookEnv(t)
	spec := jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"})
	r, e, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", spec, "call-r")
	before := commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes")
	_, err := env.store.ReserveIdentity(env.ctx, BatchReservation{
		Owner: env.id.Owner, Host: "h2", RunnerID: "runner-b",
		RootID: "r-b", ToolCallID: "call-r", InputHash: batchCommitSpecHash(spec), Sequence: 9,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none",
		}},
	})
	require.ErrorIs(t, err, ErrBatchForeignLiveHost)
	require.Equal(t, before, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"),
		"no new work on foreign live host")
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE node_id = 'r-b'"))
	// The submitted root was never accepted; let the executor's Start return so
	// the runner's drain can finish and the t.Cleanup close cannot hang.
	a := commitHookReceive(t, env.ctx, e.starts)
	a.decision <- commitHookDecision{exec: &commitHookExecution{}}
}

// Scenario 13: concurrent hooks of two stores on one DB serialize per root;
// one sequence wins, the other conflicts, no interleaved partial state.
func TestBatchCommitConcurrentHooksSerializePerRoot(t *testing.T) {
	env := newCommitHookEnv(t)
	_, err := env.store.ReserveIdentity(env.ctx, BatchReservation{
		Owner: env.id.Owner, Host: env.id.Host, RunnerID: env.id.Runner,
		RootID: "cr", ToolCallID: "call-cr", InputHash: "hash-cr", Sequence: 1,
		Root: BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none",
			},
			Children: []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: batchStoreTestString("test"), State: "pending", ControlState: "none",
			}}},
		},
	})
	require.NoError(t, err)
	store2 := NewBatchStore(env.conn)
	updA := db.ProjectBatchNodeParams{NodeID: "cr", State: "starting", Cause: "", ControlState: "none"}
	updB := db.ProjectBatchNodeParams{NodeID: "cr", State: "cancelling", Cause: "stop", ControlState: "pending"}
	type result struct {
		who string
		err error
	}
	results := make(chan result, 2)
	go func() {
		err := env.store.Project(env.ctx, BatchProjection{
			Host: env.id.Host, RunnerID: env.id.Runner, Sequence: 2,
			Roots: []BatchRootProjection{{Owner: env.id.Owner, Root: "cr", Updates: []db.ProjectBatchNodeParams{updA}}},
		})
		results <- result{"A", err}
	}()
	go func() {
		err := store2.Project(env.ctx, BatchProjection{
			Host: env.id.Host, RunnerID: env.id.Runner, Sequence: 2,
			Roots: []BatchRootProjection{{Owner: env.id.Owner, Root: "cr", Updates: []db.ProjectBatchNodeParams{updB}}},
		})
		results <- result{"B", err}
	}()
	var winner string
	for range 2 {
		res := <-results
		if res.err == nil {
			require.Empty(t, winner, "both concurrent sequences won")
			winner = res.who
		} else {
			require.ErrorIs(t, res.err, ErrBatchProjectionConflict)
		}
	}
	require.NotEmpty(t, winner, "exactly one writer wins")
	root := commitHookReadNode(t, env.conn, "cr")
	require.Equal(t, int64(2), root.RootSequence)
	require.Equal(t, int64(2), root.ProjectionSequence)
	// No interleaved partial state: the row equals the winner's full tuple.
	if winner == "A" {
		require.Equal(t, "starting", root.State)
		require.Equal(t, "", root.Cause)
		require.Equal(t, "none", root.ControlState)
	} else {
		require.Equal(t, "cancelling", root.State)
		require.Equal(t, "stop", root.Cause)
		require.Equal(t, "pending", root.ControlState)
	}
}

// Scenario 14 (M12, M13): a faulted hook returns the injected error UNCHANGED
// (same value/chain, never wrapped, never retried) and leaves the previous
// committed state intact; the Runner faults with ErrCommitFailed.
func TestBatchCommitFaultedHookErrorIdentity(t *testing.T) {
	env := newCommitHookEnv(t)
	// Error identity at the adapter boundary: a missing root surfaces
	// sql.ErrNoRows exactly as produced, unwrapped and unretried. The Changed
	// view forces batchCommitProject to resolve the (missing) root's owner.
	err := env.adapter()(env.ctx, jobbatch.Transition{
		Sequence: 5,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "missing.1", Token: 1},
		Changed: []jobbatch.NodeView{{
			ID: "missing.1", Root: "missing", State: jobbatch.StateRunning,
			Control: jobbatch.ControlNone,
		}},
	})
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, sql.ErrNoRows, err, "hook error returned as-is, not wrapped")
	// Transient writer failure: a trigger aborts the checkpoint insert with a
	// distinctive message; the hook must surface it unchanged (no retry loop,
	// no wrapping — a retry or %w wrapper changes the surfaced error text).
	_, err = env.conn.ExecContext(env.ctx,
		`CREATE TABLE batch_commit_fault(armed INTEGER NOT NULL DEFAULT 1)`)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `INSERT INTO batch_commit_fault(armed) VALUES (1)`)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER batch_commit_fault_once
		BEFORE INSERT ON batch_runner_checkpoints
		WHEN (SELECT armed FROM batch_commit_fault) = 1
		BEGIN SELECT RAISE(ABORT, 'batch commit: injected once'); END`)
	require.NoError(t, err)
	err = env.adapter()(env.ctx, jobbatch.Transition{
		Sequence: 900,
		Event:    jobbatch.Event{Type: jobbatch.EventCapacityAvailable},
	})
	require.ErrorContains(t, err, "injected once", "writer error surfaced as-is")
	require.NotContains(t, err.Error(), "retry", "no retry loop may rewrite the error")
	_, err = env.conn.ExecContext(env.ctx, `DROP TRIGGER batch_commit_fault_once`)
	require.NoError(t, err)
	// Real Runner: the same injected error faults it; previous state intact.
	injected := errors.New("private hook failure")
	var hookCalls atomic.Int32
	adapter := env.adapter()
	r, e, ctx := commitHookRunner(t, func(ctx context.Context, tr jobbatch.Transition) error {
		if tr.Event.Type == jobbatch.EventSettled {
			hookCalls.Add(1)
			return injected
		}
		return adapter(ctx, tr)
	})
	b := commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	a := commitHookAccept(t, ctx, r, e, nil)
	before := commitHookReadNode(t, env.conn, "r.1")
	a.report(jobbatch.Result{State: jobbatch.StateCompleted})
	for hookCalls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	commitHookReceive(t, ctx, r.Faulted())
	require.ErrorIs(t, r.FaultErr(), jobbatch.ErrCommitFailed)
	require.Equal(t, before, commitHookReadNode(t, env.conn, "r.1"),
		"previous committed state intact after the faulted hook")
	_, waitErr := b.Wait(ctx)
	require.ErrorIs(t, waitErr, jobbatch.ErrCommitFailed)
	require.ErrorIs(t, r.Close(ctx), jobbatch.ErrCommitFailed)
	// No retry: the hook observed the settled transition exactly once.
	require.Equal(t, int32(1), hookCalls.Load())
}

// Scenario 15 (M14): a Settled/other update of a row preserves an unrelated
// node's existing response_envelope and child_session_id (COALESCE path).
func TestBatchCommitPreservesEnvelopeAndChildSession(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	_, err := env.conn.ExecContext(env.ctx,
		`UPDATE batch_nodes SET response_envelope = ?, child_session_id = ? WHERE node_id = 'r.1'`,
		`{"state":"completed","details":{"earlier":true}}`, "child-session")
	require.NoError(t, err)
	commitHookAccept(t, ctx, r, e, nil) // Started projects r.1 again (seq 2)
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, "running", leaf.State)
	require.Equal(t, `{"state":"completed","details":{"earlier":true}}`, leaf.Envelope,
		"COALESCE must preserve the stored envelope")
	require.Equal(t, "child-session", leaf.ChildSession,
		"COALESCE must preserve the stored child_session_id")
	require.Equal(t, int64(2), leaf.ProjectionSequence)
}

// Scenario 16: StartRejected persists the rejected state without outcome
// synthesis beyond the reported facts; ControlReported persists control
// facts; StopRequested never marks the root terminal.
func TestBatchCommitStartRejectedControlReportedStopRequested(t *testing.T) {
	env := newCommitHookEnv(t)
	// StartRejected: the executor refuses the start.
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	a := commitHookReceive(t, ctx, e.starts)
	a.decision <- commitHookDecision{err: errors.New("no capacity for this")}
	rec := commitHookRecord(t, ctx, r, jobbatch.RecordStartRejected, "r.1")
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, "failed", leaf.State, "rejected start persists the rejected state")
	require.Equal(t, "failed", leaf.OutcomeState)
	require.Equal(t, rec.Code, leaf.OutcomeCode)
	require.Equal(t, "none", leaf.ControlState)
	commitHookWait(t, ctx, b)
	require.NoError(t, r.Close(ctx))
	// ControlReported + StopRequested on a fresh root.
	env2 := newCommitHookEnv(t)
	r2, e2, ctx2 := commitHookRunner(t, env2.adapter())
	b2 := commitHookSubmit(t, ctx2, r2, "s", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-s")
	stopCalled := make(chan struct{})
	a2 := commitHookAccept(t, ctx2, r2, e2, func(context.Context) error {
		close(stopCalled)
		return errors.New("stop rejected by executor")
	})
	require.NoError(t, r2.Stop(ctx2, "s.1"))
	<-stopCalled
	commitHookRecord(t, ctx2, r2, jobbatch.RecordControl, "s.1")
	leaf2 := commitHookReadNode(t, env2.conn, "s.1")
	require.Equal(t, "failed", leaf2.ControlState, "control facts persisted")
	require.Equal(t, "stop_failed", leaf2.ControlError)
	root2 := commitHookReadNode(t, env2.conn, "s")
	require.Equal(t, "cancelling", root2.State, "StopRequested never marks the root terminal")
	require.Zero(t, root2.NoticeSelected)
	select {
	case <-b2.Done():
		t.Fatal("StopRequested settled the root")
	default:
	}
	a2.report(jobbatch.Result{State: jobbatch.StateCancelled})
	commitHookWait(t, ctx2, b2)
	require.NoError(t, r2.Close(ctx2))
}
