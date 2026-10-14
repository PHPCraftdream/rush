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

	"charm.land/fantasy"
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
	// state is truthful: the root was ACKED (announce_message_id set,
	// notice_selected=0), so the schema CHECK (batch_nodes.sql:78-83) makes a
	// void WRITE impossible — delivery stays 'none' and the row is terminal.
	row := fx.rootRow(oldRoot)
	require.Equal(t, "none", row.Delivery, "an acked, unselected root can never take delivery='void'")
	require.NotEqual(t, "running", row.State, "the stale root is terminal")

	// The durable fence holds at the OUTBOX: nothing is pullable for the
	// owner (the void root fails SelectBatchNodeNotice's delivery IN
	// ('pending','done') predicate; the new incarnation has selected nothing).
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

	// Quarantine through the DURABLE FENCE: the rerun marked (owner, oldRoot)
	// in the synchronized registry, so the fence knows the exact retired
	// root in the EXACT owning store — even though its delivery stayed 'none'.
	retired, err := batchRetiredRoots.check(ctx, batchRerunCutoverOwner, oldRoot)
	require.NoError(t, err)
	require.True(t, retired, "the retired root is fenced by the rerun's registry mark")
	// The fake done source is pre-finished so the INLINE arm routes through
	// electCompletion (LoadDone reports done) instead of blocking in AwaitDone.
	done := newFakeDone(fantasy.NewTextResponse("stale completion"))
	done.finish(fantasy.NewTextResponse("stale completion"))
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner:  batchRerunCutoverOwner,
		Done:   done,
		Elect:  &fakeElector{},
		Runner: fx.install.RunnerHandle,
	})
	require.NoError(t, err)
	inv := BatchInvocation{SessionID: batchRerunCutoverOwner}
	inv.Delivery.Mode = batchDeliveryDetached
	_, err = impl.DeliverBatch(ctx, jobbatch.ID(oldRoot), inv)
	require.ErrorIs(t, err, errBatchRetiredRoot, "the stale root's DETACHED delivery is refused — it can never elect started history")
	inv.Delivery.Mode = batchDeliveryInline
	_, err = impl.DeliverBatch(ctx, jobbatch.ID(oldRoot), inv)
	require.ErrorIs(t, err, errBatchRetiredRoot, "the stale root's INLINE delivery is refused — the late stale completion can never elect history")
}

// TestBatchRerunFence_VoidRootDeliveryRefused pins the DURABLE delivery fence
// (R12-F fixup P1): a rerun-retired root — marked in the synchronized
// batchRetiredRoots registry under its exact (owner, rootID), here seeded as
// the void arm's settled delivery='void' row shape — is refused by BOTH
// delivery arms, detached and inline, with errBatchRetiredRoot and no
// started or completion history; an UNMARKED root keeps the open behavior.
// The void-root row is a valid fence input: the void arm writes it (for
// selected roots), and the live arm retires unselected rows by mark alone.
//
// Revert-check: dropping the batchRetiredRoots.check consult in
// deliverDetached or electCompletion (or keying the fence by anything wider
// than the (owner, rootID) pair) lets the late stale completion elect
// history again — the error cases below become the no-error case and the
// message count grows.
//
// Parallel-safe: the registry is keyed by this test's unique root id and
// nothing restores a process global anymore.
func TestBatchRerunFence_VoidRootDeliveryRefused(t *testing.T) {
	t.Parallel()
	fx := newBatchRerunCutoverFx(t)
	ctx := context.Background()

	// Fixture: seed the ANNOUNCED retired root directly (the void arm's
	// settled row shape). SCHEMA TRACE (first real assertion): the INSERT
	// must satisfy every batch_nodes CHECK — root branch (kind='batch',
	// node_id=root_id, tool_call_id/input_hash NOT NULL, announced=1 needs
	// announce_message_id) AND the full notice_selected branch
	// (notice_kind/state/cause NOT NULL, notice_sequence>0,
	// notice_ordinal>=0, delivery='void', wake=0).
	rootID := string(batchRootID())
	_, err := fx.conn.ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash,
		 mode, state, announced, announce_message_id,
		 notice_selected, notice_kind, notice_state, notice_cause, notice_sequence, notice_ordinal,
		 delivery, wake, created_at, updated_at)
		VALUES (?, ?, ?, 'host-a', 0, 'batch', ?, 'hash-void-root',
		 'parallel', 'completed', 1, 'msg-void-root',
		 1, 'root', 'completed', '', 1, 0,
		 'void', 0, 1700000000, 1700000001)`,
		rootID, rootID, batchRerunCutoverOwner, "call-void-root")
	require.NoError(t, err, "the INSERT is the schema assertion: CHECKs fire here")

	// Mark the exact (owner, rootID) in the synchronized registry, then check
	// through it: the entry points at the owning store on the same conn.
	batchRetiredRoots.mark(batchRerunCutoverOwner, rootID, fx.store)
	retired, err := batchRetiredRoots.check(ctx, batchRerunCutoverOwner, rootID)
	require.NoError(t, err)
	require.True(t, retired, "a marked retired root is fenced by the registry")

	// The wired batchDeliveryImpl refuses detached delivery of that root.
	// The fake done source is pre-finished so the INLINE arm routes through
	// electCompletion (LoadDone reports done) instead of blocking in AwaitDone.
	done := newFakeDone(fantasy.NewTextResponse("stale"))
	done.finish(fantasy.NewTextResponse("stale"))
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner:  batchRerunCutoverOwner,
		Done:   done,
		Elect:  &fakeElector{},
		Runner: fx.install.RunnerHandle,
	})
	require.NoError(t, err)
	msgsBefore, err := fx.messages.List(ctx, batchRerunCutoverOwner)
	require.NoError(t, err)
	inv := BatchInvocation{SessionID: batchRerunCutoverOwner}
	inv.Delivery.Mode = batchDeliveryDetached
	_, err = impl.DeliverBatch(ctx, jobbatch.ID(rootID), inv)
	require.ErrorIs(t, err, errBatchRetiredRoot, "the retired root's detached delivery fails closed")
	msgsAfter, err := fx.messages.List(ctx, batchRerunCutoverOwner)
	require.NoError(t, err)
	require.Len(t, msgsAfter, len(msgsBefore), "no history message was created for the retired root")

	// Inline is fenced too: the late stale completion can never elect
	// completion history, detached or inline.
	inv.Delivery.Mode = batchDeliveryInline
	_, err = impl.DeliverBatch(ctx, jobbatch.ID(rootID), inv)
	require.ErrorIs(t, err, errBatchRetiredRoot, "the retired root's inline delivery fails closed")

	// An UNMARKED root keeps the open behavior: detached delivery proceeds
	// to elect started history (the flag-off / not-yet-stopped world).
	unmarked := string(batchRootID())
	_, err = fx.conn.ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash,
		 mode, state, created_at, updated_at)
		VALUES (?, ?, ?, 'host-a', 0, 'batch', ?, 'hash-unmarked',
		 'parallel', 'completed', 1700000000, 1700000001)`,
		unmarked, unmarked, batchRerunCutoverOwner, "call-unmarked")
	require.NoError(t, err)
	inv.Delivery.Mode = batchDeliveryDetached
	_, err = impl.DeliverBatch(ctx, jobbatch.ID(unmarked), inv)
	require.NoError(t, err)
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
