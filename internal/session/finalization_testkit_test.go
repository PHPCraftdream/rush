package session

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/dbtest"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// errFinalizationSeamStub marks the member failure the reader tests inject.
var errFinalizationSeamStub = errors.New("finalization seam stub failure")

// finalizationSeamScopeStub implements all four facts sources with fixed
// counters and one shared error, recording every transaction it saw.
type finalizationSeamScopeStub struct {
	tx                                                 *sql.Tx
	err                                                error
	descendants, queued, holds, debt                   int
	descendantCalls, queuedCalls, holdCalls, debtCalls int
}

func (s *finalizationSeamScopeStub) FinalizationActiveDescendantsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.descendantCalls++
	s.tx = tx
	return s.descendants, s.err
}

func (s *finalizationSeamScopeStub) FinalizationQueuedCallsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.queuedCalls++
	s.tx = tx
	return s.queued, s.err
}

func (s *finalizationSeamScopeStub) FinalizationPhysicalHoldsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.holdCalls++
	s.tx = tx
	return s.holds, s.err
}

func (s *finalizationSeamScopeStub) FinalizationReactionDebtTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.debtCalls++
	s.tx = tx
	return s.debt, s.err
}

var (
	_ FinalizationDescendantsFacts   = (*finalizationSeamScopeStub)(nil)
	_ FinalizationQueuedCallsFacts   = (*finalizationSeamScopeStub)(nil)
	_ FinalizationPhysicalHoldsFacts = (*finalizationSeamScopeStub)(nil)
	_ FinalizationReactionDebtFacts  = (*finalizationSeamScopeStub)(nil)
)

// finalizationSeamEnv is the lane environment: a fully migrated temp database
// with the fencing schema reapplied, the "owner" session seeded, and the
// finalization store and service bound to that connection.
type finalizationSeamEnv struct {
	conn  *sql.DB
	ctx   context.Context
	store *session.AgentFinalizationStore
	svc   session.Service
}

// newFinalizationSeamEnv builds finalizationSeamEnv on batchStoreTestDB and
// reapplies dbtest.ApplyFinalizationFencingSchema so lanes never hand-write DDL.
func newFinalizationSeamEnv(t *testing.T) *finalizationSeamEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(ctx, conn))
	conn.SetMaxOpenConns(1)
	return &finalizationSeamEnv{
		conn:  conn,
		ctx:   ctx,
		store: session.NewAgentFinalizationStore(conn),
		svc:   session.NewService(db.New(conn), conn),
	}
}

// finalizationSeamBindOwner binds an active finalization record for sessionID
// through the real BindAgentFinalization producer and returns the authority.
func finalizationSeamBindOwner(t *testing.T, env *finalizationSeamEnv, sessionID, logicalCallID string) *session.AgentFinalizationAuthority {
	t.Helper()
	authority, err := env.svc.BindAgentFinalization(env.ctx, session.AgentFinalizationIdentity{SessionID: sessionID, LogicalCallID: logicalCallID, Generation: 1})
	require.NoError(t, err)
	return authority
}

// finalizationSeamLiveChild creates a live child session under parentID via
// CreateTaskSession and binds its active record, returning the child session.
func finalizationSeamLiveChild(t *testing.T, env *finalizationSeamEnv, parentID, toolCallID string) session.Session {
	t.Helper()
	child, err := env.svc.CreateTaskSession(env.ctx, toolCallID, parentID, "child")
	require.NoError(t, err)
	_, err = env.svc.BindAgentFinalization(env.ctx, session.AgentFinalizationIdentity{SessionID: child.ID, LogicalCallID: toolCallID + "-call", Generation: 1})
	require.NoError(t, err)
	return child
}

// finalizationSeamQueuedCallFixture prepares the trusted ancestry fixture so
// lanes can enqueue real queued calls carrying continuations.
func finalizationSeamQueuedCallFixture(t *testing.T) finalizationQueueFixture {
	t.Helper()
	return newFinalizationQueueFixture(t)
}

// finalizationSeamEnqueueQueuedCall enqueues a trusted queued call under rowID,
// producing the run-queue entry and its unconsumed continuation row.
func finalizationSeamEnqueueQueuedCall(t *testing.T, fixture finalizationQueueFixture, rowID string) {
	t.Helper()
	fixture.enqueue(t, rowID)
}

// finalizationSeamInsertDebt records a wake session notice, the reaction debt
// producer lane L3 settles.
func finalizationSeamInsertDebt(t *testing.T, env *finalizationSeamEnv, sessionID string) {
	t.Helper()
	require.NoError(t, NewSessionNoticeStore(env.conn).InsertSessionNotice(env.ctx, sessionID, NoticeKindWakeOnly, "finalization-debt", true, ""))
}

// finalizationSeamRequestCancel cancels sessionID through the real producer.
func finalizationSeamRequestCancel(t *testing.T, env *finalizationSeamEnv, sessionID string) {
	t.Helper()
	require.NoError(t, env.svc.RequestCancel(env.ctx, sessionID))
}

// finalizationSeamSetPhase forces the owner's record phase directly in the
// database, the deterministic lever the polling-gate tests drive AwaitSealed with.
func finalizationSeamSetPhase(t *testing.T, env *finalizationSeamEnv, sessionID, phase string) {
	t.Helper()
	result, err := env.conn.ExecContext(env.ctx, `UPDATE session_agent_finalizations SET phase=? WHERE session_id=?`, phase, sessionID)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed, "the phase lever must hit exactly the owner record")
}

func TestFinalizationScopeReaderFoldsCompleteFactsInTransaction(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1, queued: 2, holds: 3, debt: 4}
	reader := NewAgentFinalizationScopeReader("owner", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.NoError(t, err)
	require.True(t, facts.Complete)
	require.Equal(t, 1, facts.ActiveDescendants)
	require.Equal(t, 2, facts.QueuedCalls)
	require.Equal(t, 3, facts.PhysicalHolds)
	require.Equal(t, 4, facts.ReactionDebt)
	require.Same(t, tx, stub.tx, "every source must read inside the seal transaction")
}

func TestFinalizationScopeReaderNilMemberIsIncomplete(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1, queued: 2, holds: 3, debt: 4}
	for name, reader := range map[string]session.AgentFinalizationScopeReader{
		"no descendants": NewAgentFinalizationScopeReader("owner", nil, stub, stub, stub),
		"no queued":      NewAgentFinalizationScopeReader("owner", stub, nil, stub, stub),
		"no holds":       NewAgentFinalizationScopeReader("owner", stub, stub, nil, stub),
		"no debt":        NewAgentFinalizationScopeReader("owner", stub, stub, stub, nil),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := env.conn.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer tx.Rollback()
			facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
			require.NoError(t, err)
			require.False(t, facts.Complete)
			require.Zero(t, stub.descendantCalls, "an incomplete reader must not call any source")
		})
	}
}

func TestFinalizationScopeReaderEmptyOwnerIsIncomplete(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1}
	reader := NewAgentFinalizationScopeReader("", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.NoError(t, err)
	require.False(t, facts.Complete)
	require.Zero(t, stub.descendantCalls)
}

func TestFinalizationScopeReaderMemberErrorPropagates(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{err: errFinalizationSeamStub}
	reader := NewAgentFinalizationScopeReader("owner", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.ErrorIs(t, err, errFinalizationSeamStub)
	require.Equal(t, 1, stub.descendantCalls, "the first failing source stops the read")
	require.Zero(t, stub.queuedCalls, "later sources must not run after a member error")
}

func TestDefaultFinalizationTerminalGateAllowsEverything(t *testing.T) {
	gate := session.DefaultFinalizationTerminalGate()
	require.True(t, gate.AllowTerminal(t.Context(), "owner"))
	require.True(t, gate.AllowTerminal(t.Context(), ""))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, gate.AwaitSealed(canceled, "owner"), "the inert default never waits")
}

func TestPollingFinalizationTerminalGateAllowsWithoutActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	gate := session.NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	require.True(t, gate.AllowTerminal(t.Context(), "owner"))
	require.True(t, gate.AllowTerminal(t.Context(), ""))
	require.NoError(t, gate.AwaitSealed(t.Context(), "owner"))
}

func TestPollingFinalizationTerminalGateRefusesActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := session.NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	require.False(t, gate.AllowTerminal(t.Context(), "owner"))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, gate.AwaitSealed(canceled, "owner"), context.Canceled)
}

func TestPollingFinalizationTerminalGateTerminalPhasesAllowDelivery(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := session.NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	for _, phase := range []string{"accepted", "failed", "cancelled", "interrupted"} {
		finalizationSeamSetPhase(t, env, "owner", phase)
		require.True(t, gate.AllowTerminal(t.Context(), "owner"), phase)
	}
}

func TestPollingFinalizationTerminalGateAwaitSealedReturnsWhenRecordLeavesActive(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := session.NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- gate.AwaitSealed(ctx, "owner") }()
	finalizationSeamSetPhase(t, env, "owner", "cancelled")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitSealed did not observe the forced phase change")
	}
}

func TestPollingFinalizationTerminalGateAwaitSealedStopsOnCanceledContext(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := session.NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	ctx, cancel := context.WithCancel(env.ctx)
	cancel()
	require.ErrorIs(t, gate.AwaitSealed(ctx, "owner"), context.Canceled)
}

// batchBindFixture reserves one batch root (RunnerID "runner") with two task
// children under owner "owner", moves the second child's node (the
// delegation node) to 'starting', and creates one child session row parented
// to "owner". It returns the store, the raw writer pool, the data dir, the
// task node id and the root id.
func batchBindFixture(t *testing.T) (*BatchStore, *sql.DB, string, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))

	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	s := NewBatchStore(conn)
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	taskNode := r.Snapshot.Nodes[1].NodeID
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state = 'starting' WHERE node_id = ?", taskNode)
	require.NoError(t, err)

	require.NoError(t, seedSession(ctx, db.New(conn), "owner-child"))
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = 'owner' WHERE id = 'owner-child'")
	require.NoError(t, err)
	return s, conn, dir, taskNode, r.Snapshot.Root.RootID
}

func batchServedTestEnv(t *testing.T) (*BatchStore, *sql.DB, *sql.DB, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	require.NoError(t, seedSession(ctx, db.New(conn), "owner2"))
	// An independent handle: it can commit between the served read's
	// statements even while the writer connection holds the read transaction.
	raw, err := sql.Open("sqlite", storepath.DataDirPath(dataDir, storepath.ActiveFilename)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	return NewBatchStore(conn), conn, raw, ctx, dataDir
}

// pdcToolResultParams is the actual tool-result message a fused consume writes;
// every consume fixture uses it (Role Tool, the root's own call id "call").
func pdcToolResultParams(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Name: "bash", Content: text}},
	}
}

// pdcConsumeIdentity is the trusted root incarnation of a pdcReserveOneTask
// root: exactly the identity the production adapter validates and the CAS
// fences (owner/host/runner/call = owner/host/runner/call).
func pdcConsumeIdentity(rootID string) BatchToolResultIdentity {
	return BatchToolResultIdentity{Owner: "owner", RootID: rootID, NodeID: rootID, HostID: "host", RunnerID: "runner", ToolCallID: "call"}
}

// pdcReserveOneTask reserves a settled-eligible one-task root whose incarnation
// (host/runner/tool call) is fully populated, as the production runtime does.
func pdcReserveOneTask(t *testing.T, s *BatchStore, ctx context.Context) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	p.Root.Children = p.Root.Children[:1]
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

func batchOutboxReserve(t *testing.T, s *BatchStore, ctx context.Context, children int) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.Root.Children = nil
	for i := 0; i < children; i++ {
		p.Root.Children = append(p.Root.Children, batchStoreTestTask())
	}
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

func batchOutboxParams(row BatchNoticeRow) message.CreateMessageParams {
	return batchOutboxText(row.NodeID)
}

func batchOutboxText(text string) message.CreateMessageParams {
	return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}, BackgroundJobNotice: true}
}

func batchOutboxSettle(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot, seq int64, ids ...string) {
	t.Helper()
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	for i, id := range ids {
		v := batchStoreTestUpdate(id, "completed")
		v.OutcomeSummary = id
		p.Updates = append(p.Updates, v)
		n := batchStoreTestNotice(id)
		n.NoticeOrdinal = sql.NullInt64{Int64: int64(i), Valid: true}
		if id == snap.Root.RootID {
			n.NoticeKind = batchStoreTestString("root")
		}
		p.Notices = append(p.Notices, n)
	}
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: seq, Roots: []BatchRootProjection{p}}))
}

func batchOutboxAck(t *testing.T, s *BatchStore, ctx context.Context, m message.Service, root string) {
	t.Helper()
	msg, err := s.BatchAnnounceStarted(ctx, m, "owner", root, batchOutboxText("started"))
	require.NoError(t, err)
	persisted, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, msg.ID, persisted.Root.AnnounceMessageID.String, "ack names actual persisted history")
	history, err := m.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.Equal(t, "started", history.FullText())
}

// pdcSettleOneTaskRoot is the local settle twin of the shared batchOutboxSettle
// (batch_pull_test.go), differing ONLY in passing the projection's RunnerID:
// the shared helper projects under BatchProjection{Host: "host"} with the zero
// RunnerID, but the sequence advance is host+runner fenced
// (batch_store.go batchProjectRoot -> AdvanceBatchRootSequence), so a fixture
// reserved under RunnerID "runner" — the SAME incarnation the production
// identity fence uses — must settle under that same incarnation.
func pdcSettleOneTaskRoot(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot) {
	t.Helper()
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	v := batchStoreTestUpdate(snap.Root.RootID, "completed")
	v.OutcomeSummary = snap.Root.RootID
	p.Updates = append(p.Updates, v)
	p.Notices = append(p.Notices, batchStoreTestNotice(snap.Root.RootID))
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", RunnerID: "runner", Sequence: 2, Roots: []BatchRootProjection{p}}))
}

// batchTerminalTestReserve reserves a ONE-item live root whose task child
// carries the given group/timeout and returns the root id.
func batchTerminalTestReserve(t *testing.T, s *BatchStore, ctx context.Context, group string, timeout time.Duration) string {
	t.Helper()
	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	p.Root.Children = []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
		Kind: "task", TaskKind: batchStoreTestString("shell"),
		TaskPayload: batchStoreTestString(`{"k":"v"}`),
		TaskGroup:   group, TimeoutNs: int64(timeout),
		State: "pending", ControlState: "none",
	}}}
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot.Root.RootID
}

// batchTerminalTestProject projects terminal facts (and an optional
// response envelope) onto the given rows. The runner MUST equal the
// reservation's runner: batchProjectRoot fences every update and the root
// sequence advance with the projection-level runner (batch_store.go:207-233).
func batchTerminalTestProject(t *testing.T, s *BatchStore, ctx context.Context, root, runner string, sequence int64, updates ...db.ProjectBatchNodeParams) {
	t.Helper()
	for i := range updates {
		updates[i].ControlState = "none"
		updates[i].UpdatedAt = 1
		updates[i].RootID = root
		updates[i].HostID = "host"
		updates[i].OwnerSessionID = "owner"
		updates[i].RunnerID = runner
	}
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", RunnerID: runner, Sequence: sequence, Roots: []BatchRootProjection{{Owner: "owner", Root: root, Updates: updates}}}))
}

// batchTerminalTestSettledRoot leaves the structurally settled ROOT row as
// the engine does: state failed with NO outcome of its own (settle computes
// the root state from counts/causes and never copies the task's outcome).
func batchTerminalTestSettledRoot(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "failed"}
}

// batchBindReadLink reads child_session_id through an INDEPENDENT sqlite
// pool on the same database file — proving the link is durable on disk, not
// just visible through the writer's in-memory page cache.
func batchBindReadLink(t *testing.T, dir, taskNode string) sql.NullString {
	t.Helper()
	raw, err := sql.Open("sqlite", storepath.DataDirPath(dir, storepath.ActiveFilename)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	defer raw.Close()
	var linked sql.NullString
	require.NoError(t, raw.QueryRowContext(context.Background(), "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&linked))
	return linked
}
