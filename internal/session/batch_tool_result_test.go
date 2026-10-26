package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

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

// pdcAgeRoot updated_at-ages every row of the root: the retention candidate
// predicate (internal/db/sql/batch_recovery.sql BatchRetentionRoots) reads
// n.updated_at, never created_at.
func pdcAgeRoot(t *testing.T, conn db.DBTX, ctx context.Context, rootID string) {
	t.Helper()
	old := time.Now().Add(-2 * AsyncDataRetentionAge).Unix()
	_, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET updated_at = ? WHERE root_id = ?`, old, rootID)
	require.NoError(t, err)
}

// pdcRetentionContains reports whether the retention candidate list names root.
func pdcRetentionContains(t *testing.T, conn db.DBTX, ctx context.Context, rootID string) bool {
	t.Helper()
	roots, err := db.New(conn).BatchRetentionRoots(ctx, BatchRetentionCutoff(time.Now()))
	require.NoError(t, err)
	for _, r := range roots {
		if r.RootID == rootID {
			return true
		}
	}
	return false
}

// TestPdcConsumeToolResultConsumedTuple pins the consumed success tuple.
// Mutation caught: dropping any InlineElect predicate or an incarnation fence
// (host/runner/tool_call) from BatchOutboxConsumeWithToolResult lets this test
// pass with a half-consumed root (e.g. announced=0) or a foreign incarnation.
func TestPdcConsumeToolResultConsumedTuple(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := pdcReserveOneTask(t, s, ctx)
	pdcSettleOneTaskRoot(t, s, ctx, snap)
	m := message.NewService(db.New(s.writer))

	msg, consumed, err := s.BatchConsumeToolResult(ctx, m, "owner", pdcConsumeIdentity(snap.Root.RootID), pdcToolResultParams("done"))
	require.NoError(t, err)
	require.True(t, consumed, "the matching incarnation must win the CAS")

	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Root.Announced, "announced=1")
	require.Equal(t, "done", got.Root.Delivery, "delivery='done'")
	require.EqualValues(t, 0, got.Root.Wake, "wake=0")
	require.EqualValues(t, 1, got.Root.Reacted, "reacted=1")
	require.Equal(t, msg.ID, got.Root.AnnounceMessageID.String, "announce_message_id names the actual tool message")
	require.Equal(t, msg.ID, got.Root.NoticeMessageID.String, "notice_message_id names the actual tool message")

	history, err := m.Get(ctx, msg.ID)
	require.NoError(t, err)
	results := history.ToolResults()
	require.Len(t, results, 1, "exactly one tool-result part")
	require.Equal(t, "call", results[0].ToolCallID)
	require.Equal(t, "done", results[0].Content)
	require.False(t, results[0].IsError)
}

// TestPdcConsumeToolResultLostIncarnation pins lost-CAS semantics.
// Mutation caught: turning the :execrows CAS loss into an error (or skipping
// the message insert on 0 rows) breaks the read-only loser contract this test
// pins: a loser still persists its legitimate result without mutating the
// winner's durable identity.
func TestPdcConsumeToolResultLostIncarnation(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := pdcReserveOneTask(t, s, ctx)
	pdcSettleOneTaskRoot(t, s, ctx, snap)
	m := message.NewService(db.New(s.writer))

	wrong := pdcConsumeIdentity(snap.Root.RootID)
	wrong.HostID = "other-host"
	msg, consumed, err := s.BatchConsumeToolResult(ctx, m, "owner", wrong, pdcToolResultParams("loser-read"))
	require.NoError(t, err)
	require.False(t, consumed, "a foreign incarnation must lose the CAS")
	require.NotEmpty(t, msg.ID, "the loser's result message is still persisted as a read")

	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 0, got.Root.Announced, "winner state untouched: still unannounced")
	require.Equal(t, "pending", got.Root.Delivery)
	require.Equal(t, "", got.Root.NoticeMessageID.String)
	require.NotEqual(t, msg.ID, got.Root.AnnounceMessageID.String)
}

// TestPdcConsumeToolResultInvalidIdentity pins fail-closed identity checks.
// Mutation caught: relaxing the Go-side identity guard (role != Tool, node !=
// root, empty host) would let a non-tool message or a non-root row be written
// AND named as the durable delivery identity.
func TestPdcConsumeToolResultInvalidIdentity(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := pdcReserveOneTask(t, s, ctx)
	pdcSettleOneTaskRoot(t, s, ctx, snap)
	m := message.NewService(db.New(s.writer))
	before, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)

	cases := []struct {
		name     string
		owner    string
		identity BatchToolResultIdentity
		params   message.CreateMessageParams
	}{
		{"user-role", "owner", pdcConsumeIdentity(snap.Root.RootID), batchOutboxText("not-a-tool-result")},
		{"node-not-root", "owner", BatchToolResultIdentity{Owner: "owner", RootID: snap.Root.RootID, NodeID: snap.Nodes[1].NodeID, HostID: "host", RunnerID: "runner", ToolCallID: "call"}, pdcToolResultParams("x")},
		{"empty-host", "owner", BatchToolResultIdentity{Owner: "owner", RootID: snap.Root.RootID, NodeID: snap.Root.RootID, HostID: "", RunnerID: "runner", ToolCallID: "call"}, pdcToolResultParams("x")},
		{"owner-mismatch", "someone-else", pdcConsumeIdentity(snap.Root.RootID), pdcToolResultParams("x")},
	}
	for _, tc := range cases {
		_, _, err := s.BatchConsumeToolResult(ctx, m, tc.owner, tc.identity, tc.params)
		require.ErrorIs(t, err, ErrBatchToolResultIdentity, tc.name)
	}
	require.EqualValues(t, 0, countMessages(t, db.New(conn), ctx, "owner"), "no message row may exist")
	after, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, before, after, "root unchanged by refused consumes")
}

// TestPdcConsumeToolResultTriggerRollback pins atomicity.
// Mutation caught: committing the message insert before (or independently of)
// the consume CAS — any non-transactional ordering — leaves the tool message
// behind when the CAS statement aborts, which this BEFORE UPDATE trigger
// simulates.
func TestPdcConsumeToolResultTriggerRollback(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := pdcReserveOneTask(t, s, ctx)
	pdcSettleOneTaskRoot(t, s, ctx, snap)
	m := message.NewService(db.New(s.writer))
	_, err := conn.ExecContext(ctx, `CREATE TRIGGER pdc_abort_consume BEFORE UPDATE ON batch_nodes BEGIN SELECT RAISE(ABORT, 'pdc rollback'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS pdc_abort_consume`) })

	_, _, err = s.BatchConsumeToolResult(ctx, m, "owner", pdcConsumeIdentity(snap.Root.RootID), pdcToolResultParams("rolled-back"))
	require.Error(t, err, "the aborted consume must surface the tx error")
	require.EqualValues(t, 0, countMessages(t, db.New(conn), ctx, "owner"), "the rolled-back message must be absent")

	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 0, got.Root.Announced)
	require.Equal(t, "pending", got.Root.Delivery)
	require.Equal(t, "", got.Root.NoticeMessageID.String)
}

// TestPdcConsumeToolResultRetentionDebtAndPull pins the post-consume state's
// system-wide contracts, with a pre-consume negative control on the SAME
// fixture shape (fresh root per arm, sequential asserts, no state bleed).
// Mutation caught: writing wake=1/reacted=0 (or leaving delivery='pending' or
// the task child non-terminal) after consumption would make the consumed root
// reaction debt or non-retention-eligible: the pre-consume control proves the
// fixture itself is excluded, the post-consume assert proves ONLY the consume
// changed that.
func TestPdcConsumeToolResultRetentionDebtAndPull(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))

	// Arm 1 (negative control): settled one-task root with a PENDING task
	// child, un-consumed — must NOT be a retention candidate.
	pre := pdcReserveOneTask(t, s, ctx)
	pdcSettleOneTaskRoot(t, s, ctx, pre)
	pdcAgeRoot(t, conn, ctx, pre.Root.RootID)
	require.False(t, pdcRetentionContains(t, conn, ctx, pre.Root.RootID),
		"the un-consumed fixture (pending child / pending delivery) is not retention-eligible")

	// Arm 2: fresh root, same fixture, settled with the child TERMINAL, then
	// consumed — the exact post-consume state must be retention-eligible.
	snap := pdcReserveOneTask(t, s, ctx)
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	rootUpdate := batchStoreTestUpdate(snap.Root.RootID, "completed")
	rootUpdate.OutcomeSummary = snap.Root.RootID
	p.Updates = append(p.Updates, rootUpdate, batchStoreTestUpdate(snap.Nodes[1].NodeID, "completed"))
	p.Notices = append(p.Notices, batchStoreTestNotice(snap.Root.RootID))
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", RunnerID: "runner", Sequence: 2, Roots: []BatchRootProjection{p}}))
	_, consumed, err := s.BatchConsumeToolResult(ctx, m, "owner", pdcConsumeIdentity(snap.Root.RootID), pdcToolResultParams("done"))
	require.NoError(t, err)
	require.True(t, consumed)
	pdcAgeRoot(t, conn, ctx, snap.Root.RootID)
	require.True(t, pdcRetentionContains(t, conn, ctx, snap.Root.RootID),
		"the consumed, debt-free root is retention-eligible after the age cutoff")

	debt, err := s.BatchReactionDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.False(t, debt, "a consumed root carries no reaction debt")

	pulled, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a consumed root has no pullable notice left")
}

// TestPdcConsumeToolResultConcurrentSingleWinner pins cross-connection CAS.
// Mutation caught: a non-atomic read-then-write consume (or a CAS without the
// incarnation fence) lets both connections report consumption, duplicating the
// durable delivery identity across two messages.
func TestPdcConsumeToolResultConcurrentSingleWinner(t *testing.T) {
	a, b, ctx := batchOutboxTwoStores(t)
	snap := pdcReserveOneTask(t, a, ctx)
	pdcSettleOneTaskRoot(t, a, ctx, snap)
	ma, mb := message.NewService(db.New(a.writer)), message.NewService(db.New(b.writer))

	start := make(chan struct{})
	type outcome struct {
		consumed bool
		msgID    string
	}
	results := make(chan outcome, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i, store := range []*BatchStore{a, b} {
		go func(i int, store *BatchStore) {
			defer wg.Done()
			<-start
			ms := ma
			if i == 1 {
				ms = mb
			}
			msg, consumed, err := store.BatchConsumeToolResult(ctx, ms, "owner", pdcConsumeIdentity(snap.Root.RootID), pdcToolResultParams("racer"))
			if err != nil {
				errs <- err
				return
			}
			results <- outcome{consumed: consumed, msgID: msg.ID}
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	close(results)
	winners, losers := 0, 0
	var winnerMsgID string
	var ids []string
	for r := range results {
		ids = append(ids, r.msgID)
		if r.consumed {
			winners++
			winnerMsgID = r.msgID
		} else {
			losers++
		}
	}
	require.Len(t, ids, 2, "both racers persist a result")
	require.Equal(t, 1, winners, "exactly one CAS winner, in either order")
	require.Equal(t, 1, losers, "exactly one CAS loser, in either order")
	require.NotEqual(t, ids[0], ids[1], "both messages persisted, distinct ids")

	got, err := a.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Root.Announced)
	require.Equal(t, winnerMsgID, got.Root.AnnounceMessageID.String)
	require.Equal(t, winnerMsgID, got.Root.NoticeMessageID.String)
}
