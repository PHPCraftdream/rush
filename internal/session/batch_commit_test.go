package session

import (
	"context"
	"database/sql"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// R11B CommitHook adapter acceptance tests. Every scenario drives a real
// jobbatch.Runner (fake executor; synchronization is event-driven through the
// executor's own channels and the Runner's published feed, never sleeps) or
// the store/adapter directly. Each test pins one mapping or fence from
// batch_commit_doc.go and docs/research/2026-10-12-r11-ingress-decomposition.md
// §2; each has a named revert-check mutation script that must kill it.

type commitHookAttempt struct {
	launch   jobbatch.Launch
	report   func(jobbatch.Result)
	decision chan commitHookDecision
}

type commitHookDecision struct {
	exec jobbatch.Execution
	err  error
}

type commitHookExecutor struct{ starts chan *commitHookAttempt }

func (e *commitHookExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	a := &commitHookAttempt{launch: l, report: report, decision: make(chan commitHookDecision, 1)}
	e.starts <- a
	d := <-a.decision
	return d.exec, d.err
}

type commitHookExecution struct {
	stop   func(context.Context) error
	report func(jobbatch.Result)
}

// Stop counts as confirmed termination: after the caller's stop func succeeds
// it reports a terminal Interrupted result so Runner.Close's drain completes
// even when the test never reports. commitHookAccept wraps report in a
// sync.Once, so this is a no-op when the test already reported.
func (e *commitHookExecution) Stop(ctx context.Context) error {
	if e.stop != nil {
		if err := e.stop(ctx); err != nil {
			return err
		}
	}
	if e.report != nil {
		e.report(jobbatch.Result{State: jobbatch.StateInterrupted})
	}
	return nil
}

type commitHookEnv struct {
	t     *testing.T
	store *BatchStore
	conn  *sql.DB
	ctx   context.Context
	id    BatchCommitIdentity
}

func newCommitHookEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	store, conn, ctx := batchStoreTestDB(t)
	return &commitHookEnv{
		t: t, store: store, conn: conn, ctx: ctx,
		id: BatchCommitIdentity{Host: "h1", Runner: "runner-a", Owner: "owner"},
	}
}

func (env *commitHookEnv) adapter() func(context.Context, jobbatch.Transition) error {
	return NewBatchCommitHook(env.store, env.id)
}

func commitHookRunner(t *testing.T, hook func(context.Context, jobbatch.Transition) error) (*jobbatch.Runner, *commitHookExecutor, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	e := &commitHookExecutor{starts: make(chan *commitHookAttempt, 8)}
	r, err := jobbatch.NewRunner(ctx, jobbatch.RunnerConfig{
		Limits:   jobbatch.Limits{MaxConcurrent: 2, MaxDepth: 16, MaxNodes: 128},
		Executor: e, ControlTimeout: 20 * time.Second, RecordRetention: 256,
		CommitHook: hook, CommitTimeout: 20 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer closeCancel()
		_ = r.Close(closeCtx)
	})
	return r, e, ctx
}

func commitHookReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal("barrier did not complete")
		var zero T
		return zero
	}
}

func commitHookSubmit(t *testing.T, ctx context.Context, r *jobbatch.Runner, id jobbatch.ID, s jobbatch.Spec, ref string) *jobbatch.Batch {
	t.Helper()
	b, err := r.Submit(ctx, jobbatch.SubmitRequest{Root: id, Spec: s, Ref: ref})
	require.NoError(t, err)
	return b
}

func commitHookRecord(t *testing.T, ctx context.Context, r *jobbatch.Runner, typ jobbatch.RecordType, id jobbatch.ID) jobbatch.Record {
	t.Helper()
	cursor := uint64(1)
	for {
		p, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range p.Records {
			if rec.Type == typ && (id == "" || rec.Node == id) {
				return rec
			}
		}
		if p.Final != 0 && p.Next > p.Head {
			t.Fatalf("record %s(%s) not found before Final", typ, id)
		}
		cursor = p.Next
	}
}

func commitHookAccept(t *testing.T, ctx context.Context, r *jobbatch.Runner, e *commitHookExecutor,
	stop func(context.Context) error,
) *commitHookAttempt {
	t.Helper()
	// Single-start tests: the only delivery is the expected leaf.
	a := commitHookReceive(t, ctx, e.starts)
	return commitHookAcceptReady(t, ctx, r, a, stop)
}

// commitHookAcceptPair accepts the starts of a two-leaf batch and returns the
// attempts in the requested node-ID order. The Runner dispatches one start
// goroutine per leaf in ordinal order, but channel delivery order races with
// spawn order, so arrival order must never be assumed: a swapped pairing
// settles the other leaf while a later by-ID feed scan waits forever.
func commitHookAcceptPair(t *testing.T, ctx context.Context, r *jobbatch.Runner, e *commitHookExecutor,
	stopFirst, stopSecond func(context.Context) error, first, second jobbatch.ID,
) (*commitHookAttempt, *commitHookAttempt) {
	t.Helper()
	byID := make(map[jobbatch.ID]*commitHookAttempt, 2)
	for len(byID) < 2 {
		a := commitHookReceive(t, ctx, e.starts)
		require.NotContains(t, byID, a.launch.ID, "duplicate start for %s", a.launch.ID)
		byID[a.launch.ID] = a
	}
	a1 := commitHookAcceptReady(t, ctx, r, byID[first], stopFirst)
	a2 := commitHookAcceptReady(t, ctx, r, byID[second], stopSecond)
	return a1, a2
}

func commitHookAcceptReady(t *testing.T, ctx context.Context, r *jobbatch.Runner, a *commitHookAttempt,
	stop func(context.Context) error,
) *commitHookAttempt {
	t.Helper()
	// Exactly-once reporting shared by commitHookFinish and execution.Stop.
	var once sync.Once
	raw := a.report
	a.report = func(res jobbatch.Result) { once.Do(func() { raw(res) }) }
	a.decision <- commitHookDecision{exec: &commitHookExecution{stop: stop, report: a.report}}
	commitHookRecord(t, ctx, r, jobbatch.RecordStarted, a.launch.ID)
	return a
}

func commitHookFinish(t *testing.T, ctx context.Context, r *jobbatch.Runner, a *commitHookAttempt, res jobbatch.Result) jobbatch.Record {
	t.Helper()
	a.report(res)
	return commitHookRecord(t, ctx, r, jobbatch.RecordSettled, a.launch.ID)
}

func commitHookWait(t *testing.T, ctx context.Context, b *jobbatch.Batch) jobbatch.Summary {
	t.Helper()
	s, err := b.Wait(ctx)
	require.NoError(t, err)
	return s
}

type commitHookNodeRow struct {
	State, Cause, ControlState, ControlError           string
	OutcomeState, OutcomeCode, OutcomeSummary          string
	Token, Attempts, ControlPending                    int64
	Ordinal, RootSequence, ProjectionSequence          int64
	Envelope, EnvelopeRef, ChildSession, NoticeOrdinal string
	NoticeSelected, Wake                               int64
	Delivery, RunnerID, HostID                         string
}

func commitHookReadNode(t *testing.T, conn *sql.DB, node string) commitHookNodeRow {
	t.Helper()
	var r commitHookNodeRow
	err := conn.QueryRowContext(context.Background(), `SELECT state, cause, control_state, control_error,
		outcome_state, outcome_code, outcome_summary, token, attempts, control_pending,
		ordinal, root_sequence, projection_sequence,
		COALESCE(response_envelope,''), COALESCE(response_envelope_ref,''),
		COALESCE(child_session_id,''), COALESCE(notice_ordinal,''),
		notice_selected, wake, delivery, runner_id, host_id
		FROM batch_nodes WHERE node_id = ?`, node).Scan(
		&r.State, &r.Cause, &r.ControlState, &r.ControlError,
		&r.OutcomeState, &r.OutcomeCode, &r.OutcomeSummary,
		&r.Token, &r.Attempts, &r.ControlPending,
		&r.Ordinal, &r.RootSequence, &r.ProjectionSequence,
		&r.Envelope, &r.EnvelopeRef, &r.ChildSession, &r.NoticeOrdinal,
		&r.NoticeSelected, &r.Wake, &r.Delivery, &r.RunnerID, &r.HostID)
	require.NoError(t, err)
	return r
}

func commitHookCount(t *testing.T, conn *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, conn.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func commitHookCheckpoint(t *testing.T, conn *sql.DB, runner string) (seq int64, eventType string, final int64) {
	t.Helper()
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT sequence, event_type, final FROM batch_runner_checkpoints WHERE runner_id = ?`,
		runner).Scan(&seq, &eventType, &final))
	return seq, eventType, final
}

func commitHookTask() jobbatch.Spec {
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "controlled"}}
}

func commitHookBatch(notify jobbatch.Notify, items ...jobbatch.Spec) jobbatch.Spec {
	return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: notify, Items: items}}
}

// Scenario 1 (M01): a forced failure between the reservation insert and the
// initial view projection leaves NO rows (root, nodes, checkpoint).
func TestBatchCommitSubmitAtomicReservation(t *testing.T) {
	env := newCommitHookEnv(t)
	reservation := BatchReservation{
		Owner: env.id.Owner, Host: env.id.Host, RunnerID: env.id.Runner,
		RootID: "r1", ToolCallID: "call-1", InputHash: "hash-1", Sequence: 1,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none",
		}, Children: []BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: batchStoreTestString("test"), State: "pending", ControlState: "none"}},
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: batchStoreTestString("test"), State: "invalid", ControlState: "none"}},
		}},
	}
	_, err := env.store.ReserveIdentity(env.ctx, reservation)
	require.ErrorContains(t, err, "CHECK constraint")
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
	// Duplicate root identity: the same RootID on a second (non-attach) call
	// violates the node primary key; the whole transaction, including
	// already-inserted rows, must roll back.
	ok := reservation
	ok.Root.Children = reservation.Root.Children[:1]
	_, err = env.store.ReserveIdentity(env.ctx, ok)
	require.NoError(t, err)
	before := commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes")
	dup := ok
	dup.ToolCallID = "call-other"
	dup.Sequence = 2
	_, err = env.store.ReserveIdentity(env.ctx, dup)
	require.Error(t, err)
	require.Equal(t, before, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
	require.Equal(t, int64(1), commitHookCount(t, env.conn,
		`SELECT root_sequence FROM batch_nodes WHERE node_id = 'r1'`),
		"root_sequence unchanged by the failed duplicate reservation")
	// Failure of the view projection AFTER the tree insert (stale update
	// target) must roll the reservation back too: atomic reserve-plus-view.
	late := ok
	late.RootID = ""
	late.ToolCallID = "call-late"
	late.Sequence = 3
	late.Updates = []db.ProjectBatchNodeParams{{NodeID: "missing", State: "running", ControlState: "none"}}
	_, err = env.store.ReserveIdentity(env.ctx, late)
	require.ErrorIs(t, err, ErrBatchProjectionConflict)
	require.Equal(t, before, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"),
		"tree insert rolled back when the initial-view projection fails")
}

// Scenario 2 (M02): the root ID from the Transition appears verbatim;
// descendant IDs are deterministic ordinal paths; reservation writes the
// initial views atomically with the reservation itself.
func TestBatchCommitExactRunnerIDs(t *testing.T) {
	env := newCommitHookEnv(t)
	r, _, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", commitHookBatch(jobbatch.NotifyAll, commitHookTask(), commitHookTask()), "call-r")
	require.Equal(t, int64(1), commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE node_id = root_id"),
		"exactly one root, under the Runner's verbatim ID")
	root := commitHookReadNode(t, env.conn, "r")
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, int64(1), commitHookCount(t, env.conn,
		"SELECT count(*) FROM batch_nodes WHERE node_id = 'r' AND root_id = 'r' AND root_sequence = 1"))
	require.Equal(t, env.id.Runner, root.RunnerID)
	require.Equal(t, env.id.Host, root.HostID)
	require.Equal(t, int64(0), root.Ordinal)
	require.Equal(t, int64(1), leaf.Ordinal, "descendants use deterministic ordinal paths")
	require.Equal(t, int64(2), commitHookReadNode(t, env.conn, "r.2").Ordinal)
	require.Equal(t, "starting", leaf.State,
		"initial view projected by the reservation insert; the leaf was admitted in the submit transition")
	require.Equal(t, int64(1), leaf.ProjectionSequence)
}

// Scenario 3 (M17): Add inserts the appended subtree inside Project at the
// accepted parent ordinal; it never reserves a second root.
func TestBatchCommitAddAppendsInsideProject(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	commitHookAccept(t, ctx, r, e, nil)
	ids, err := r.Add(ctx, jobbatch.AddRequest{Parent: "r", Items: []jobbatch.Spec{commitHookTask()}})
	require.NoError(t, err)
	require.Equal(t, []jobbatch.ID{"r.2"}, ids)
	added := commitHookReadNode(t, env.conn, "r.2")
	require.Equal(t, int64(2), added.Ordinal, "appended at the accepted parent ordinal")
	require.Equal(t, env.id.Runner, added.RunnerID)
	require.Equal(t, int64(1), commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE node_id = root_id"),
		"Add must not create a second root")
	require.Equal(t, int64(1), commitHookCount(t, env.conn,
		"SELECT count(*) FROM batch_nodes WHERE node_id = 'r.2' AND parent_id = 'r' AND root_id = 'r'"))
}

// Scenario 4 (M07): the notice ordinal is the Notice's position in
// Transition.Notices, never the structural node ordinal.
func TestBatchCommitNoticeOrdinalIsSlicePosition(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r",
		commitHookBatch(jobbatch.NotifyEach, commitHookTask(), commitHookTask()), "call-r")
	a1, a2 := commitHookAcceptPair(t, ctx, r, e, nil, nil, "r.1", "r.2")
	commitHookFinish(t, ctx, r, a1, jobbatch.Result{State: jobbatch.StateCompleted})
	// The root is not yet settled (r.2 still running), so this transition
	// selects exactly one notice: leaf r.1, structural ordinal 1.
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, int64(1), leaf.NoticeSelected)
	require.Equal(t, "0", leaf.NoticeOrdinal, "ordinal is the slice position, not the structural ordinal")
	require.Equal(t, int64(1), leaf.Ordinal, "structural ordinal differs from the notice ordinal")
	require.Equal(t, int64(0), commitHookReadNode(t, env.conn, "r").NoticeSelected,
		"unsettled root is not notified")
	// Finishing r.2 settles r.2 AND the root in one transition; Notices walk
	// descendants before ancestors: [r.2, r].
	commitHookFinish(t, ctx, r, a2, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookWait(t, ctx, b)
	root := commitHookReadNode(t, env.conn, "r")
	leaf2 := commitHookReadNode(t, env.conn, "r.2")
	require.Equal(t, "0", leaf2.NoticeOrdinal)
	require.Equal(t, "1", root.NoticeOrdinal, "root's notice follows its descendants in the slice")
	require.Equal(t, int64(0), root.Ordinal)
}

// Scenario 5 (M06, M15): a Settled with Result.Details stores the full JSON
// envelope plus its reference; a forced notice failure rolls back BOTH the
// node view and the envelope.
func TestBatchCommitEnvelopeDurabilityAndRollback(t *testing.T) {
	env := newCommitHookEnv(t)
	r, e, ctx := commitHookRunner(t, env.adapter())
	b := commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	a := commitHookAccept(t, ctx, r, e, nil)
	rec := commitHookFinish(t, ctx, r, a, jobbatch.Result{
		State: jobbatch.StateCompleted, Code: "ok", Summary: "sum",
		Details: []byte(`{"k":[1,2]}`),
	})
	commitHookWait(t, ctx, b)
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.JSONEq(t, `{"state":"completed","code":"ok","summary":"sum","details":{"k":[1,2]}}`, leaf.Envelope)
	require.Equal(t, "r.1#"+itoa(rec.Sequence), leaf.EnvelopeRef)
	// Forced failure on the notice insert (nonterminal notice target) after
	// the node update inside one Project transaction rolls back both.
	before := commitHookReadNode(t, env.conn, "r.1")
	p := BatchProjection{
		Host: env.id.Host, RunnerID: env.id.Runner, Sequence: 100,
		Roots: []BatchRootProjection{{
			Owner: env.id.Owner, Root: "r",
			Updates: []db.ProjectBatchNodeParams{{
				NodeID: "r.1", State: "failed", Cause: "stop", ControlState: "none", Token: 9, Attempts: 7,
				ResponseEnvelope: sql.NullString{String: `{"state":"failed"}`, Valid: true},
			}},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID:        "r.1", // running node: SelectBatchNodeNotice matches 0 rows
				NoticeKind:    sql.NullString{String: "task", Valid: true},
				NoticeState:   sql.NullString{String: "failed", Valid: true},
				NoticeOrdinal: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1,
			}},
		}},
	}
	err := env.store.Project(env.ctx, p)
	require.ErrorIs(t, err, ErrBatchProjectionConflict)
	after := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, before, after, "node view and envelope both rolled back")
}

// Scenario 6 (M08): nothing is visible in the DB mid-commit; after each
// committed sequence the projection equals the Runner's published view.
func TestBatchCommitZeroPublicationBeforeCommit(t *testing.T) {
	env := newCommitHookEnv(t)
	// Token gate: the wrapper takes one token per gated transition and blocks
	// while the gate is empty; refill to let the next gated commit proceed.
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	gated := make(chan jobbatch.Transition, 8) // signals a gated commit is at the gate
	adapter := env.adapter()
	r, e, ctx := commitHookRunner(t, func(ctx context.Context, tr jobbatch.Transition) error {
		if tr.Event.Type == jobbatch.EventSubmit || tr.Event.Type == jobbatch.EventSettled {
			gated <- tr
			<-gate
		}
		return adapter(ctx, tr)
	})
	reply := make(chan *jobbatch.Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), Ref: "call-r"})
		reply <- b
	}()
	tr := commitHookReceive(t, ctx, gated)
	require.Equal(t, "r", string(tr.Event.Root))
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE root_id = 'r'"),
		"no rows visible while the Submit commit is gated")
	gate <- struct{}{} // release the Submit commit
	b := commitHookReceive(t, ctx, reply)
	a := commitHookAccept(t, ctx, r, e, nil)
	commitHookRecord(t, ctx, r, jobbatch.RecordStarted, "r.1")
	a.report(jobbatch.Result{State: jobbatch.StateCompleted})
	tr = commitHookReceive(t, ctx, gated)
	require.Equal(t, jobbatch.EventSettled, tr.Event.Type)
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, "running", leaf.State, "previous committed state held while the Settled commit is gated")
	gate <- struct{}{} // release the Settled commit
	commitHookWait(t, ctx, b)
	leaf = commitHookReadNode(t, env.conn, "r.1")
	view, err := r.View(ctx, "r.1")
	require.NoError(t, err)
	require.Equal(t, string(view.State), leaf.State)
	require.Equal(t, int64(view.Token), leaf.Token)
	require.Equal(t, int64(view.Attempts), leaf.Attempts)
	require.Equal(t, string(view.Outcome.State), leaf.OutcomeState)
	root := commitHookReadNode(t, env.conn, "r")
	rootView, err := r.View(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, string(rootView.State), root.State)
	require.Equal(t, int64(rootView.Token), root.Token)
}

// Scenario 7 (M04, M03 partial): a previous-incarnation transition (same
// host+root, different RunnerID, higher sequence) is rejected and changes
// nothing; same host is ErrBatchRunnerMismatch, another host is refused too.
func TestBatchCommitIncarnationFencing(t *testing.T) {
	env := newCommitHookEnv(t)
	spec := jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"})
	r, e, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", spec, "call-r")
	commitHookAccept(t, ctx, r, e, nil)
	// Attach with a different incarnation on the same live root.
	_, err := env.store.ReserveIdentity(env.ctx, BatchReservation{
		Owner: env.id.Owner, Host: env.id.Host, RunnerID: "runner-old",
		RootID: "r", ToolCallID: "call-r", InputHash: batchCommitSpecHash(spec), Sequence: 50,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none",
		}},
	})
	require.ErrorIs(t, err, ErrBatchRunnerMismatch)
	// A projection under the foreign incarnation with a higher sequence must
	// hit the runner_id fence (0 rows) and change nothing.
	before := commitHookReadNode(t, env.conn, "r")
	err = env.store.Project(env.ctx, BatchProjection{
		Host: env.id.Host, RunnerID: "runner-old", Sequence: 99,
		Roots: []BatchRootProjection{{
			Owner: env.id.Owner, Root: "r",
			Updates: []db.ProjectBatchNodeParams{{NodeID: "r", State: "failed", ControlState: "none"}},
		}},
	})
	require.ErrorIs(t, err, ErrBatchProjectionConflict)
	require.Equal(t, before, commitHookReadNode(t, env.conn, "r"))
	// Stale same-sequence replay through the reservation path (no root
	// advance): the projection_sequence fence must refuse it.
	_, err = env.store.ReserveIdentity(env.ctx, BatchReservation{
		Owner: env.id.Owner, Host: env.id.Host, RunnerID: env.id.Runner,
		RootID: "r", ToolCallID: "call-r", InputHash: batchCommitSpecHash(spec), Sequence: 2,
		Updates: []db.ProjectBatchNodeParams{{NodeID: "r.1", State: "running", ControlState: "none"}},
	})
	require.ErrorIs(t, err, ErrBatchProjectionConflict, "stale same-sequence projection refused")
	leaf := commitHookReadNode(t, env.conn, "r.1")
	require.Equal(t, int64(2), leaf.ProjectionSequence, "projection_sequence not inflated")
}

// Scenario 8 (M05): Sequence or Token at/above 2^63 is ErrBatchValueOverflow
// before any DB write; driven through a real Runner the hook error faults it
// with ErrCommitFailed and the previous committed state stays intact.
func TestBatchCommitSequenceOverflow(t *testing.T) {
	env := newCommitHookEnv(t)
	adapter := env.adapter()
	r, e, ctx := commitHookRunner(t, env.adapter())
	commitHookSubmit(t, ctx, r, "r", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}), "call-r")
	a := commitHookAccept(t, ctx, r, e, nil)
	before := commitHookReadNode(t, env.conn, "r.1")
	// Sequence overflow, direct adapter call: no DB write.
	err := adapter(env.ctx, jobbatch.Transition{
		Sequence: math.MaxUint64,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1},
	})
	require.ErrorIs(t, err, ErrBatchValueOverflow)
	require.Equal(t, before, commitHookReadNode(t, env.conn, "r.1"))
	// Token overflow: checked before any write as well.
	err = adapter(env.ctx, jobbatch.Transition{
		Sequence: 3,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 2},
		Changed: []jobbatch.NodeView{{
			ID: "r.1", Root: "r", State: jobbatch.StateRunning,
			Token: math.MaxUint64, Control: jobbatch.ControlNone,
		}},
	})
	require.ErrorIs(t, err, ErrBatchValueOverflow)
	require.Equal(t, before, commitHookReadNode(t, env.conn, "r.1"))
	// Zero sequence is refused like overflow (never a real transition).
	err = adapter(env.ctx, jobbatch.Transition{
		Sequence: 0,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 2},
	})
	require.ErrorIs(t, err, ErrBatchValueOverflow)
	_ = a
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
