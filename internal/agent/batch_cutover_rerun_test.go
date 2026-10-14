// BatchRerunStopVoided coverage (R12-F cutover acceptance 3): the flag-on
// post-commit stop is EXACT-incarnation scoped. Real SQLite data dir, real
// batch node rows, real engine (jobbatch.Runner over a blocking executor).
//
//	(a) a rerun stops exactly the committed incarnation: the stale engine
//	    execution is cancelled, the new incarnation with the reused
//	    tool_call_id keeps running;
//	(b) a stale-incarnation completion is never delivered into the new
//	    incarnation: the old root is void-delivery, no history message is
//	    created, the new incarnation's rows stay untouched;
//	(c) two reused tool-call IDs stay distinct: incarnation scoping is the
//	    (tool_call_id, root_id) pair — only the retired root_id fences.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const batchRerunCutoverOwner = "rerun-cutover-owner"

// blockingBatchExecutor is a real jobbatch.Executor whose executions block
// until the engine cancels them (the stop under test) or the test releases
// them. Per-root channels make the cancellation observable per incarnation.
type blockingBatchExecutor struct {
	mu      sync.Mutex
	release map[string]chan struct{}
	stopped map[string]chan struct{}
	// stops counts every observed engine cancellation (channel len is always
	// 0, so idempotent second-pass stops are counted instead).
	stops atomic.Int64
	// reportLate: after the stop, ALSO submit a LATE "stale completion"
	// report (the completion-isolation subject of test b). Off by default so
	// test (a) observes a plain cancelled settlement.
	reportLate bool
}

func newBlockingBatchExecutor() *blockingBatchExecutor {
	return &blockingBatchExecutor{release: map[string]chan struct{}{}, stopped: map[string]chan struct{}{}}
}

func (e *blockingBatchExecutor) ch(m map[string]chan struct{}, key string) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := m[key]
	if !ok {
		ch = make(chan struct{})
		m[key] = ch
	}
	return ch
}

func (e *blockingBatchExecutor) releaseRoot(root string) { close(e.ch(e.release, root)) }
func (e *blockingBatchExecutor) stoppedRoot(root string) chan struct{} {
	return e.ch(e.stopped, root)
}

// Start blocks until the engine's cancellation (a stopped execution reports
// Cancelled and then, after a bounded delay, a LATE "stale" completion — the
// completion-isolation subject) or the test releases it (Completed).
func (e *blockingBatchExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	root := string(l.ID)
	// Leaf node ids are "<root>.<ordinal>"; the root id is the first segment.
	for i := 0; i < len(root); i++ {
		if root[i] == '.' {
			root = root[:i]
			break
		}
	}
	stopCh := e.stoppedRoot(root)
	go func() {
		select {
		case <-ctx.Done():
			e.stops.Add(1)
			close(stopCh)
			if e.reportLate {
				// The stale incarnation's LATE completion: submitted after
				// the stop, it must commit against the retired root only.
				time.Sleep(20 * time.Millisecond)
				report(jobbatch.Result{State: jobbatch.StateCompleted, Summary: "stale completion"})
				return
			}
			report(jobbatch.Result{State: jobbatch.StateCancelled})
		case <-e.ch(e.release, root):
			report(jobbatch.Result{State: jobbatch.StateCompleted, Summary: "released"})
		}
	}()
	return stoppedBatchExecution{}, nil
}

type stoppedBatchExecution struct{}

func (stoppedBatchExecution) Stop(context.Context) error { return nil }

func (stoppedBatchExecution) ReadOutput(_ context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return jobbatch.OutputChunk{Next: cursor, EOF: true}, nil
}

type batchRerunCutoverFx struct {
	t        *testing.T
	coord    *coordinator
	store    *session.BatchStore
	conn     *sql.DB
	messages message.Service
	install  *BatchInstall
	exec     *blockingBatchExecutor
	seq      int64
}

func newBatchRerunCutoverFx(t *testing.T) *batchRerunCutoverFx {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	_, err = db.New(conn).CreateSession(ctx, db.CreateSessionParams{ID: batchRerunCutoverOwner, Title: batchRerunCutoverOwner})
	require.NoError(t, err)
	store := session.NewBatchStore(conn)
	messages := message.NewService(db.New(conn))
	exec := newBlockingBatchExecutor()
	profile, err := NewBatchProfileDefault()
	require.NoError(t, err)
	registries := NewBatchLeafRegistryStore()
	install, err := InstallBatchRuntime(ctx, BatchInstallDeps{
		Host: "host-a", RunnerID: "runner-1", Profile: profile,
		Registries: registries, Store: store, Messages: messages,
		Executor: exec,
		// Engine budgets: jobbatch.NewRunner fails closed on zero values, so
		// the fixture mirrors the production factory defaults
		// (internal/app/batch_install_factory.go).
		Limits:          jobbatch.Limits{MaxConcurrent: 16, MaxDepth: 8, MaxNodes: 256},
		CommitTimeout:   5 * time.Second,
		ControlTimeout:  60 * time.Second,
		RecordRetention: 256,
	})
	require.NoError(t, err)
	ingress, err := NewBatchWorkIngress(install.Runtime, registries)
	require.NoError(t, err)
	coord := &coordinator{}
	require.NoError(t, BindCoordinatorBatchRuntime(coord, ingress))
	return &batchRerunCutoverFx{t: t, coord: coord, store: store, conn: conn, messages: messages, install: install, exec: exec, seq: 1}
}

// submit starts one real engine root for callID (the commit hook reserves the
// real batch_nodes rows; tool_call_id = Ref).
func (f *batchRerunCutoverFx) submit(callID string) string {
	f.t.Helper()
	root := batchRootID()
	payload, err := json.Marshal(batchLeafPayload{Tool: "bash", ToolCallID: callID, SessionID: batchRerunCutoverOwner, Input: json.RawMessage(`{}`)})
	require.NoError(f.t, err)
	_, err = f.install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{
		Root: root,
		Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "test", Payload: payload}},
		Ref:  callID,
	})
	require.NoError(f.t, err)
	return string(root)
}

func (f *batchRerunCutoverFx) awaitRootState(root, state string) {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		snap, err := f.store.Read(context.Background(), batchRerunCutoverOwner, root)
		return err == nil && snap.Root.State == state
	}, 5*time.Second, 10*time.Millisecond, "root %s never reached state %s", root, state)
}

func (f *batchRerunCutoverFx) rootRow(root string) db.BatchNode {
	f.t.Helper()
	snap, err := f.store.Read(context.Background(), batchRerunCutoverOwner, root)
	require.NoError(f.t, err)
	require.Equal(f.t, root, snap.Root.RootID, "real batch_nodes root row must be readable (schema trace)")
	return snap.Root
}

// ack names the root's announce message, so the rerun reconcile's announce
// arm matches it.
func (f *batchRerunCutoverFx) ack(root, callID string) message.Message {
	f.t.Helper()
	msg, err := f.store.BatchAnnounceStarted(context.Background(), f.messages, batchRerunCutoverOwner, root, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: callID, Name: "bash", Content: "started"}},
	})
	require.NoError(f.t, err)
	return msg
}

// rerun commits the real R10c reconciliation. The TARGET is a real prior
// user message and the announce message is the TAIL (batch_history_rerun.go
// excludes the target from deletedIDs and matches the announce arm against
// deletedIDs only — so the ack must live in the tail, never the target).
// Returns the committed Voided set (the post-commit stop's input).
func (f *batchRerunCutoverFx) rerun(announce message.Message) session.BatchRerunTruncation {
	f.t.Helper()
	target, err := f.messages.Create(context.Background(), batchRerunCutoverOwner, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "rerun target"}},
	})
	require.NoError(f.t, err)
	trunc, err := f.store.BatchRerunReconcile(context.Background(), f.messages, session.RerunTruncateParams{
		Owner: batchRerunCutoverOwner, TargetID: target.ID, TailIDs: []string{announce.ID},
	})
	require.NoError(f.t, err)
	require.NotEmpty(f.t, trunc.Voided, "the committed reconciliation must route the old incarnation for retirement")
	return trunc
}

// reserveNewIncarnation reserves a fresh live root for a REUSED tool_call_id
// once the old incarnation left the live key, and projects it running.
func (f *batchRerunCutoverFx) reserveNewIncarnation(callID string) session.BatchSnapshot {
	f.t.Helper()
	f.seq++
	p := session.BatchReservation{
		Owner: batchRerunCutoverOwner, Host: "host-a", RunnerID: "runner-2",
		ToolCallID: callID, InputHash: "hash-" + callID, Sequence: f.seq,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	}
	r, err := f.store.Reserve(context.Background(), p)
	require.NoError(f.t, err)
	require.False(f.t, r.Attached, "the new incarnation must be a FRESH root, never an attach to the retired one")
	f.seq++
	snap := r.Snapshot
	updates := []db.ProjectBatchNodeParams{{NodeID: snap.Root.NodeID, State: "running", ControlState: "none"}}
	for _, n := range snap.Nodes {
		// ListBatchNodes has no root exclusion: snap.Nodes includes the root
		// itself, already covered by the seed update above. A second update
		// with the same sequence would require projection_sequence < sequence
		// and match 0 rows (ErrBatchProjectionConflict).
		if n.NodeID == snap.Root.NodeID {
			continue
		}
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: n.NodeID, State: "running", ControlState: "none"})
	}
	require.NoError(f.t, f.store.Project(context.Background(), session.BatchProjection{
		Host: "host-a", RunnerID: "runner-2", Sequence: f.seq,
		Roots: []session.BatchRootProjection{{Owner: batchRerunCutoverOwner, Root: snap.Root.RootID, Updates: updates}},
	}))
	return snap
}

func (f *batchRerunCutoverFx) fence() *batchRerunFence { return batchRerunFenceFor(f.coord) }

// TestBatchRerunStopVoided_StopsExactlyCommittedIncarnation proves (a) and
// (c): the engine execution of the committed incarnation is cancelled, while
// a fresh incarnation reusing the SAME tool_call_id keeps running and is
// never fenced stale.
//
// Revert-check: key the stop by tool_call_id (work_ledger_rerun-style) or
// drop the exact-root guard, and the reused call id's new incarnation is
// cancelled / fenced with the old one.
func TestBatchRerunStopVoided_StopsExactlyCommittedIncarnation(t *testing.T) {
	t.Parallel()
	fx := newBatchRerunCutoverFx(t)
	ctx := context.Background()

	oldRoot := fx.submit("call-1")
	fx.awaitRootState(oldRoot, "running")
	// SCHEMA TRACE (first real assertion): the engine's commit hook reserved
	// a REAL batch_nodes root row — kind 'batch', node_id = root_id,
	// tool_call_id NOT NULL — read back through session.BatchStore.Read over
	// the real migrations (db.Connect).
	oldRow := fx.rootRow(oldRoot)
	require.Equal(t, "batch", oldRow.Kind)
	require.Equal(t, oldRoot, oldRow.NodeID)
	require.Equal(t, "call-1", oldRow.ToolCallID.String)
	require.Equal(t, "host-a", oldRow.HostID)

	trunc := fx.rerun(fx.ack(oldRoot, "call-1"))
	require.Len(t, trunc.Voided, 1)
	require.Equal(t, oldRoot, trunc.Voided[0].RootID, "the Voided entry carries the EXACT committed incarnation")
	require.Equal(t, "call-1", trunc.Voided[0].ToolCallID)
	require.Equal(t, "running", trunc.Voided[0].State)

	fx.coord.BatchRerunStopVoided(ctx, batchRerunCutoverOwner, trunc.Voided)

	// The REAL engine execution of exactly that incarnation was cancelled.
	select {
	case <-fx.exec.stoppedRoot(oldRoot):
	case <-time.After(5 * time.Second):
		t.Fatal("the committed incarnation's engine execution was never stopped")
	}
	fx.awaitRootState(oldRoot, "cancelled")
	// New incarnation on the reused tool_call_id: fresh root, running, and
	// NOT fenced stale by the retired one.
	newSnap := fx.reserveNewIncarnation("call-1")
	newRow := fx.rootRow(newSnap.Root.RootID)
	require.Equal(t, "running", newRow.State)
	require.False(t, fx.fence().stale(batchRerunCutoverOwner, "call-1", newSnap.Root.RootID),
		"the new incarnation's events on the reused call id must never be stale")
	require.True(t, fx.fence().stale(batchRerunCutoverOwner, "call-1", oldRoot),
		"the retired incarnation's events are stale")

	// A second committed pass over the SAME voided set is idempotent and
	// cannot touch the new incarnation.
	fx.coord.BatchRerunStopVoided(ctx, batchRerunCutoverOwner, trunc.Voided)
	require.Equal(t, "running", fx.rootRow(newSnap.Root.RootID).State, "the new incarnation keeps running")
	require.Positive(t, fx.exec.stops.Load(), "the old incarnation stays stopped (second pass is idempotent)")
}

// TestBatchRerunStopVoided_StaleCompletionNotDelivered proves (b): the late
// completion of the stopped stale incarnation commits ONLY against its own
// void-delivery root and never surfaces in history — the new incarnation's
// rows stay untouched.
//
// Revert-check: deliver the stopped execution's report into the live
// incarnation's history (or un-void the retired root) and the message count
// grows / the new incarnation's rows change.
func TestBatchRerunStopVoided_StaleCompletionNotDelivered(t *testing.T) {
	t.Parallel()
	fx := newBatchRerunCutoverFx(t)
	ctx := context.Background()

	// Sequence (R12-F fixup): submit + ack the OLD incarnation, rerun (the
	// live arm retires it), stop it, THEN reserve the NEW incarnation on the
	// reused call id; the stale completion settles afterwards and must stay
	// unpullable.
	oldRoot := fx.submit("call-1")
	fx.awaitRootState(oldRoot, "running")
	fx.exec.reportLate = true

	trunc := fx.rerun(fx.ack(oldRoot, "call-1"))
	msgsBefore, err := fx.messages.List(ctx, batchRerunCutoverOwner)
	require.NoError(t, err)
	fx.coord.BatchRerunStopVoided(ctx, batchRerunCutoverOwner, trunc.Voided)

	// The stale execution was stopped, then reported its LATE completion.
	select {
	case <-fx.exec.stoppedRoot(oldRoot):
	case <-time.After(5 * time.Second):
		t.Fatal("the stale execution was never stopped")
	}
	// The new incarnation is reserved AFTER the old root left the live key
	// (the (owner, tool_call_id) unique live index would otherwise attach,
	// so waiting is the only correct order). The assertions below prove the
	// late stale report could not contaminate the ALREADY-RUNNING
	// replacement rows — not that the report arrived before the reservation.
	require.Eventually(t, func() bool {
		row := fx.rootRow(oldRoot)
		return !batchRerunStopWorthy(row.State)
	}, 5*time.Second, 10*time.Millisecond, "the old root must leave the live key before the new incarnation reserves")
	newSnap := fx.reserveNewIncarnation("call-1")

	// The late report settles its OWN root (completed) or the engine's
	// cancellation won the race (cancelled); either way it is terminal.
	require.Eventually(t, func() bool {
		row := fx.rootRow(oldRoot)
		return row.State == "completed" || row.State == "cancelled"
	}, 5*time.Second, 10*time.Millisecond, "the stale root must settle")

	// SCHEMA TRACE (first real assertion of this test): the retired root's
	// row is real (read back via session.BatchStore.Read) and its durable
	// state is quarantined: the rerun's BatchVoidRetiredRoot marks the
	// retired root's notice void BEFORE the stale settlement can select it,
	// so delivery is 'void' and nothing is pullable.
	row := fx.rootRow(oldRoot)
	require.Equal(t, "void", row.Delivery, "the rerun's BatchVoidRetiredRoot marks the retired root's notice void before the stale settlement can select it")
	require.Zero(t, row.Wake, "the voided root wakes nothing: no outbox debt, no pull")

	// The durable void mark holds at the OUTBOX: nothing is pullable for the
	// owner (BatchOutboxPending requires delivery='pending', the row is
	// 'void'; the new incarnation has selected nothing).
	pulled, err := fx.store.BatchPullNotices(ctx, fx.messages, batchRerunCutoverOwner, func(row session.BatchNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "notice:" + row.NodeID}}}
	})
	require.NoError(t, err)
	require.Empty(t, pulled, "the retired root's notice must not be pullable after the stale settlement")

	// No history message was created by the stale completion.
	msgsAfter, err := fx.messages.List(ctx, batchRerunCutoverOwner)
	require.NoError(t, err)
	require.Len(t, msgsAfter, len(msgsBefore), "a stale completion must not be delivered into history")

	// The new incarnation on the reused call id is unaffected: fresh rows,
	// live delivery state, not fenced stale.
	newRow := fx.rootRow(newSnap.Root.RootID)
	require.Equal(t, "running", newRow.State)
	require.Equal(t, "none", newRow.Delivery)
	require.False(t, fx.fence().stale(batchRerunCutoverOwner, "call-1", newSnap.Root.RootID))
}

// TestBatchVoidRetiredRoot_QuarantinesOutbox pins the durable void mark
// (R12-F fixup P1): session.BatchStore.BatchVoidRetiredRoot turns a REAL
// reserved, announced, live root into delivery='void', wake=0 with
// notice_selected=1, so its notice is never pullable — BatchOutboxPending
// requires delivery='pending' — and the operation is idempotent.
//
// Revert-check: drop the BatchVoidRetiredRoot call in BatchRerunStopVoided
// (or break its delivery='void' SET) and the root's settlement selects a
// delivery='pending' notice that BatchPullNotices delivers into history.
//
// Parallel-safe: everything is scoped to this fixture's own database.
func TestBatchVoidRetiredRoot_QuarantinesOutbox(t *testing.T) {
	t.Parallel()
	fx := newBatchRerunCutoverFx(t)
	ctx := context.Background()

	// Reserve a REAL live tree. SCHEMA TRACE (first real assertion): the
	// Reserve INSERT into batch_nodes must satisfy the root CHECKs
	// (kind='batch', node_id=root_id, tool_call_id/input_hash NOT NULL,
	// mode 'parallel', state 'running').
	fx.seq++
	r, err := fx.store.Reserve(ctx, session.BatchReservation{
		Owner: batchRerunCutoverOwner, Host: "host-a", RunnerID: "runner-1",
		ToolCallID: "call-void", InputHash: "hash-call-void", Sequence: fx.seq,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "running", ControlState: "none",
		}},
	})
	require.NoError(t, err, "the Reserve INSERT is the schema assertion: root CHECKs fire here")
	root := r.Snapshot.Root.RootID
	rootRow := fx.rootRow(root)
	require.Equal(t, "running", rootRow.State)

	// Announce the root: the outbox precondition (BatchOutboxPending joins on
	// announced=1), so the quarantine assertion below is the REAL gate.
	fx.ack(root, "call-void")

	// The durable void mark: live unselected root gets the synthetic
	// select+void (CHECK-legal notice identity, delivery='void', wake=0).
	require.NoError(t, fx.store.BatchVoidRetiredRoot(ctx, batchRerunCutoverOwner, root))
	rootRow = fx.rootRow(root)
	require.Equal(t, "void", rootRow.Delivery, "the retired root's notice is voided, never pullable")
	require.Zero(t, rootRow.Wake, "the voided root wakes nothing: no outbox debt")
	require.Equal(t, int64(1), rootRow.NoticeSelected, "the void mark synthetically selects the notice")

	// Nothing is pullable for the owner despite announced=1.
	pulled, err := fx.store.BatchPullNotices(ctx, fx.messages, batchRerunCutoverOwner, func(row session.BatchNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "notice:" + row.NodeID}}}
	})
	require.NoError(t, err)
	require.Empty(t, pulled, "BatchOutboxPending requires delivery='pending'; the row is 'void'")

	// Idempotent: a second void mark is a tolerated no-op.
	require.NoError(t, fx.store.BatchVoidRetiredRoot(ctx, batchRerunCutoverOwner, root))
	rootRow = fx.rootRow(root)
	require.Equal(t, "void", rootRow.Delivery)
	require.Zero(t, rootRow.Wake)
	require.Equal(t, int64(1), rootRow.NoticeSelected)
}

// TestBatchRerunStopVoided_NotWiredIsNoOp: a coordinator with no bound batch
// runtime (flag-off construction, bare fixture) neither panics nor stops.
//
// Revert-check: require a binding unconditionally and every bare coordinator
// rerun panics or logs a spurious stop.
func TestBatchRerunStopVoided_NotWiredIsNoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		(&coordinator{}).BatchRerunStopVoided(context.Background(), "owner", []session.BatchRerunVoidedNode{
			{RootID: "root", NodeID: "root", ToolCallID: "call", State: "running", HostID: "host-a"},
		})
	})
	require.NotPanics(t, func() {
		(&coordinator{}).BatchRerunStopVoided(context.Background(), "owner", nil)
	})
}
