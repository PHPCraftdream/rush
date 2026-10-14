// SessionNoticeStore coverage (package R12-D chunk 2): the ON-runtime
// session-notice machinery exercised against a database where async_jobs
// DOES NOT EXIST -- dropped right after db.Connect's migrations. Any
// accidental async_jobs query in the new path errors ("no such table:
// async_jobs"), which is what makes these tests real: they prove the
// notice pull, its void condition, the child-question reader, and the
// batch-facts debt read are all job-table-independent. batch_nodes rows
// (migration 20261012000001) drive the injected batch fact sources.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// newNoticeTestDB opens a real, migrated SQLite DB via db.Connect in a
// fresh temp dir, then DROPS async_jobs. Connect itself only runs
// migrations (goose Up) and opens the pool; nothing after Connect in these
// tests reads async_jobs, so the immediate drop is safe and turns any
// hidden dependency into a loud "no such table" error.
func newNoticeTestDB(t *testing.T) (*sql.DB, *db.Queries, context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, "DROP TABLE async_jobs")
	require.NoError(t, err, "async_jobs must exist after migrations, before the drop")
	return conn, db.New(conn), ctx
}

// newNoticeTestStore builds a SessionNoticeStore over newNoticeTestDB's
// connection with default batch wiring (empty host id, fail-closed host
// liveness for foreign hosts).
func newNoticeTestStore(t *testing.T) (*SessionNoticeStore, *sql.DB, *db.Queries, context.Context) {
	t.Helper()
	conn, q, ctx := newNoticeTestDB(t)
	return NewSessionNoticeStore(conn), conn, q, ctx
}

// requirePlaceholderCount is the mechanical guard against the string-splice
// drift that produced `..., ?, , 1, ...` here before R12-D pass 2: it counts
// the '?' placeholders in the statement and requires exactly one bound arg
// per placeholder. None of the statements it guards contain a '?' inside a
// string literal, so a plain count is exact.
func requirePlaceholderCount(t *testing.T, stmt string, args ...any) {
	t.Helper()
	require.Equal(t, len(args), strings.Count(stmt, "?"),
		"placeholder/arg mismatch: every ? must correspond to exactly one arg\nstmt: %s", stmt)
}

// seedBatchRoot inserts a CHECK-legal batch_nodes ROOT row (kind='batch',
// node_id=root_id, parent NULL, ordinal 0), self-referencing FKs satisfied
// by the row itself in the same insert. The projection CHECK
// (20261012000001_batch_projection.sql:78-83) allows a root either
// UNSELECTED (notice_selected=0, all notice metadata NULL, delivery='none',
// wake=0) or SELECTED (notice_selected=1, notice_kind/state/cause NOT NULL,
// notice_sequence>0, notice_ordinal NOT NULL, delivery!='none'). The notice
// metadata is always bound as ARGS (never spliced into the SQL text): the
// unselected shape binds notice_selected as a concrete 0 (the column is NOT
// NULL -- an explicit NULL would violate it) and NULL for the five nullable
// notice metadata columns; the selected shape
// binds 1,'task','completed',”,1,0. This is the raw-INSERT escape hatch: no
// production API produces an arbitrary live/void/terminal root under a
// caller-chosen owner/host/tool_call_id (Reserve always reserves a fresh
// tree; the void writes are rerun-owned), and GetLiveBatchRoot /
// ListLiveBatchRootsForOwner only need the root row itself.
func seedBatchRoot(t *testing.T, ctx context.Context, conn *sql.DB, hostID, rootID, toolCallID, state, delivery string) {
	t.Helper()
	if hostID == "" {
		hostID = "host-notice-test"
	}
	var noticeKind, noticeState, noticeCause, noticeSeq, noticeOrd any
	noticeSelected := any(0)
	if delivery != "none" {
		noticeSelected, noticeKind, noticeState = 1, "task", "completed"
		noticeCause, noticeSeq, noticeOrd = "", 1, 0
	}
	const stmt = `INSERT INTO batch_nodes (
		node_id, root_id, owner_session_id, host_id, ordinal, kind, mode,
		tool_call_id, input_hash, state, delivery,
		notice_selected, notice_kind, notice_state, notice_cause, notice_sequence, notice_ordinal,
		wake, created_at, updated_at
	) VALUES (?, ?, ?, ?, 0, 'batch', 'parallel',
	          ?, ?, ?, ?,
	          ?, ?, ?, ?, ?, ?,
	          ?, 1728600000, 1728600000)`
	args := []any{
		rootID, rootID, "owner-notice", hostID, toolCallID, "hash-" + rootID, state, delivery,
		noticeSelected, noticeKind, noticeState, noticeCause, noticeSeq, noticeOrd, 0,
	}
	requirePlaceholderCount(t, stmt, args...)
	_, err := conn.ExecContext(ctx, stmt, args...)
	require.NoError(t, err)
}

// TestSessionNoticeStore_PullUnboundNoticeDelivers proves the basic pull
// with async_jobs gone: an unbound wake_only notice (no job_tool_call_id,
// so the bound-notice check never runs) is delivered exactly once -- the
// row CASes to done, a history message appears, and a second pull returns
// nothing.
//
// Trace: Connect migrates -> DROP async_jobs -> seedSession('owner-notice')
// (sessions row; session_notices.owner FK target) -> InsertSessionNotice
// (wake_only, wake=1, no bound id) -> PullSessionNotices lists it pending,
// CASes pending->done in tx, creates the message in the SAME tx, commits ->
// first assertion: len(pulled)==1; second: session_notices.delivery='done';
// third: one message row; fourth: second pull is empty (delivery filter).
func TestSessionNoticeStore_PullUnboundNoticeDelivers(t *testing.T) {
	t.Parallel()
	store, _, q, ctx := newNoticeTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-notice"))
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice", NoticeKindWakeOnly, "wake up", true, ""))

	messages := message.NewService(q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-notice", sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.True(t, pulled[0].Wake)

	notice, err := q.ListSessionNoticesForOwner(ctx, "owner-notice")
	require.NoError(t, err)
	require.Len(t, notice, 1)
	require.Equal(t, "done", notice[0].Delivery)
	require.Equal(t, pulled[0].Message.ID, notice[0].NoticeMessageID.String)

	again, err := store.PullSessionNotices(ctx, messages, "owner-notice", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, again)
}

// TestSessionNoticeStore_BoundNoticeVoidRules pins the four bound-notice
// outcomes over batch root facts, all with async_jobs DROPPED:
//
//   - live root (state='running', delivery='pending')  -> KEPT (delivered)
//   - terminal root (state='completed')                -> VOIDED
//   - void delivery root (state='running', 'void')     -> VOIDED
//   - missing root (no batch_nodes row at all)         -> VOIDED
//
// Trace (each subtest): Connect -> DROP async_jobs -> seedSession ->
// seedBatchRoot for the live/terminal/void cases (CHECK-legal per its doc;
// owner_session_id='owner-notice' FKs to the sessions row) ->
// InsertSessionNotice(child_question, bound to the root's tool_call_id) ->
// PullSessionNotices: ListPending -> per-row tx: PullPendingSessionNotice
// CAS -> sessionNoticeVoidCondition -> batchBoundNoticeGone ->
// GetLiveBatchRoot (batch_nodes, NOT async_jobs) -> void path downgrades
// delivery done->void, keep path inserts the message -> assertions on
// message count and final notice delivery.
func TestSessionNoticeStore_BoundNoticeVoidRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		seed          func(t *testing.T, ctx context.Context, conn *sql.DB, toolCallID string)
		wantDelivered bool
	}{
		{"live root kept", func(t *testing.T, ctx context.Context, conn *sql.DB, tc string) {
			seedBatchRoot(t, ctx, conn, "", "root-live", tc, "running", "pending")
		}, true},
		{"terminal root voided", func(t *testing.T, ctx context.Context, conn *sql.DB, tc string) {
			seedBatchRoot(t, ctx, conn, "", "root-done", tc, "completed", "done")
		}, false},
		{"void delivery root voided", func(t *testing.T, ctx context.Context, conn *sql.DB, tc string) {
			seedBatchRoot(t, ctx, conn, "", "root-void", tc, "running", "void")
		}, false},
		{"missing root voided", func(t *testing.T, ctx context.Context, conn *sql.DB, tc string) {
			_ = tc // no batch_nodes row at all
		}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, conn, q, ctx := newNoticeTestStore(t)
			require.NoError(t, seedSession(ctx, q, "owner-notice"))
			tc.seed(t, ctx, conn, "call-bound-1")

			require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice",
				NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child-1): proceed?", false, "call-bound-1"))

			pulled, err := store.PullSessionNotices(ctx, message.NewService(q), "owner-notice", sessionNoticeParams)
			require.NoError(t, err)
			if tc.wantDelivered {
				require.Len(t, pulled, 1, "a live root must keep the notice")
			} else {
				require.Empty(t, pulled, "a gone/void root must void the notice")
			}

			notice, err := q.ListSessionNoticesForOwner(ctx, "owner-notice")
			require.NoError(t, err)
			require.Len(t, notice, 1)
			if tc.wantDelivered {
				require.Equal(t, "done", notice[0].Delivery)
			} else {
				require.Equal(t, "void", notice[0].Delivery)
			}
		})
	}
}

// TestSessionNoticeStore_SupervisionArmUsesBatchRoots proves the
// supervision void condition reads batch root rows, not async_jobs: with
// one live root on host 'h-other', the notice is KEPT only when the
// injected hostNotDead predicate says that host is alive, and VOIDED when
// it does not (the A15 dead-host rule); a void-delivery root never counts
// (R2A-9). The own-host arm uses a store whose hostID is wired to the
// root's host.
//
// Trace (each case): Connect -> DROP async_jobs -> seedSession ->
// seedBatchRoot on the chosen host -> InsertSessionNotice(supervision,
// unbound) -> PullSessionNotices: void condition -> liveRootsForOwner ->
// ListLiveBatchRootsForOwner (batch_nodes) -> hostNotDeadFunc per row ->
// keep/void decided in the same tx as the CAS. The keep case must end
// delivery='done' and the void cases delivery='void' -- an empty pull alone
// would also match a swallowed void-check error (the row would stay
// 'pending'), so the final delivery is asserted explicitly. With async_jobs
// gone, a legacy ListRunningAsyncJobsForOwners call would error and the row
// would stay pending -- so the delivery moves here prove the batch read ran.
func TestSessionNoticeStore_SupervisionArmUsesBatchRoots(t *testing.T) {
	t.Parallel()
	build := func(t *testing.T) (*SessionNoticeStore, *db.Queries, context.Context) {
		store, _, q, ctx := newNoticeTestStore(t)
		require.NoError(t, seedSession(ctx, q, "owner-notice"))
		require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice", NoticeKindSupervision, "still working", true, ""))
		return store, q, ctx
	}

	// Other host alive -> kept.
	store, q, ctx := build(t)
	seedBatchRoot(t, ctx, store.sqlDB, "h-other", "root-sup", "call-sup", "running", "pending")
	store.SetHostNotDeadFunc(func(own, other string) bool { return other == "h-other" })
	pulled, err := store.PullSessionNotices(ctx, message.NewService(q), "owner-notice", sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "a live root on a not-dead host keeps the supervision notice")
	NoticeDelivery(t, q, "owner-notice", "done")

	// Other host dead -> voided.
	store2, q2, ctx2 := build(t)
	seedBatchRoot(t, ctx2, store2.sqlDB, "h-other", "root-sup", "call-sup", "running", "pending")
	store2.SetHostNotDeadFunc(func(own, other string) bool { return false })
	pulled2, err := store2.PullSessionNotices(ctx2, message.NewService(q2), "owner-notice", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled2, "a live root on a provably dead host voids the supervision notice")
	NoticeDelivery(t, q2, "owner-notice", "void", "the void rule must have returned true: a swallowed error would leave the row pending")

	// Only a void-delivery root -> nothing open scope -> voided (R2A-9).
	store3, q3, ctx3 := build(t)
	seedBatchRoot(t, ctx3, store3.sqlDB, "h-other", "root-sup", "call-sup", "running", "void")
	store3.SetHostNotDeadFunc(func(own, other string) bool { return true })
	pulled3, err := store3.PullSessionNotices(ctx3, message.NewService(q3), "owner-notice", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled3, "a void-delivery root is not open scope")
	NoticeDelivery(t, q3, "owner-notice", "void")
}

// NoticeDelivery asserts the owner's single notice row ended in the given
// delivery state (the supervision cases' swallowed-error guard).
func NoticeDelivery(t *testing.T, q *db.Queries, owner string, delivery string, msgAndArgs ...interface{}) {
	t.Helper()
	rows, err := q.ListSessionNoticesForOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, delivery, rows[0].Delivery, msgAndArgs...)
}

// TestSessionNoticeStore_BatchDebtRows proves the batch-facts debt variant
// is the R8 selected-debt read (BatchOutboxDebtNodes + BatchOutboxDebtNotices),
// seeded entirely through the real BatchStore API (Reserve -> BatchAnnounceStarted
// -> Project/SelectBatchNodeNotice), with async_jobs DROPPED:
//
//   - before announcement: selected nodes cannot exist (the announcement
//     gate r.announced=1), so only the pending wake_only notice is debt;
//   - after announce + root settlement (root state='completed', selected
//     wake notice delivery='pending'): NODE debt with full identity --
//     the terminal root is the review's regression case;
//   - after a CHILD is settled and its notice PULLED through the real R8
//     path (BatchPullNotices -> batchPullOneNotice: delivery='done' and
//     notice_message_id named via BatchOutboxNameMessage): debt contains
//     BOTH the root ref AND the child ref, the child with RootID != NodeID
//     and a NON-EMPTY NoticeMessageID -- the review's two failing oracles
//     ("implementation discards selected children" / "always-empty message
//     identity") now fail loudly;
//   - after the notices go 'void': no node debt (query filter);
//   - the notice half keeps id+kind (wake=1/reacted=0/delivery pending|done).
//
// Trace: Connect -> DROP async_jobs -> seedSession('owner', the FK target
// the R8 helpers reserve under) -> batchStoreTestReserve (Reserve: root
// kind='batch' + two task children, state pending, delivery 'none'
// unselected -- CHECK-legal) -> InsertSessionNotice(wake_only, pending) ->
// first read: BatchOutboxDebtNodes empty (no r.announced=1 row),
// BatchOutboxDebtNotices has the notice -> batchOutboxAck
// (BatchAnnounceStarted: root announced=1 + announce_message_id, satisfying
// CHECK announced=0 OR announce_message_id IS NOT NULL) -> second read:
// still no node debt (nothing selected) -> batchOutboxSettle(seq 2, root):
// Project marks the root terminal (state='completed') and selects its wake
// notice (notice_selected=1, kind/state/cause/sequence/ordinal set,
// delivery='pending', wake=1 -- CHECK-legal, same tx) -> third read: one
// BatchDebtNodeRef{RootID=NodeID=root, NoticeMessageID ""} plus the notice
// debt -> batchOutboxSettle(seq 3, child root.1) advances the root to
// sequence 3 and settles the child (state completed, notice selected, kind
// 'task' -- CHECK-legal), then bs.BatchPullNotices(message.NewService(q),
// owner, batchOutboxParams): the REAL R8 consumer pulls BOTH selected
// notices in root sequence/ordinal order, CASes each delivery pending->done
// and names each message via BatchOutboxNameMessage (same tx) -> fourth
// read: TWO node refs -- root (RootID==NodeID) and child (RootID=root,
// NodeID=root.1, NoticeMessageID non-empty) -- and the terminal settled root
// is STILL debt with its pulled (done) notice: the terminal-root-stays-debt
// regression is exercised by the seq 2 and seq 3 reads themselves, with no
// extra reprojection -> raw UPDATE delivery='void', wake=0 on the SELECTED
// rows only (CHECK-legal: selected rows with delivery!='none'; the
// rerun-only void write has no per-node API; the untouched second child
// stays unselected delivery='none') -> fifth read: no node debt, notice
// debt unchanged. Any accidental async_jobs read errors on the dropped
// table.
func TestSessionNoticeStore_BatchDebtRows(t *testing.T) {
	t.Parallel()
	store, conn, q, ctx := newNoticeTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner"))
	bs := NewBatchStore(conn)
	snap := batchStoreTestReserve(t, bs, ctx)
	root := snap.Root.RootID
	require.NoError(t, store.InsertSessionNotice(ctx, "owner", NoticeKindWakeOnly, "wake", true, ""))

	debt, err := store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, debt.Nodes, "no announced root: nothing selected can be debt")
	require.Len(t, debt.Notices, 1, "the pending wake_only notice is notice debt")
	require.Equal(t, NoticeKindWakeOnly, debt.Notices[0].Kind)

	batchOutboxAck(t, bs, ctx, message.NewService(q), root)
	debt, err = store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, debt.Nodes, "announced but nothing settled: still no node debt")

	batchOutboxSettle(t, bs, ctx, snap, 2, root)
	debt, err = store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, debt.Nodes, 1)
	require.Equal(t, root, debt.Nodes[0].RootID)
	require.Equal(t, root, debt.Nodes[0].NodeID)
	require.Empty(t, debt.Nodes[0].NoticeMessageID, "no notice message named yet")
	require.Len(t, debt.Notices, 1)

	// Terminal-root regression (the review's finding) is ALREADY exercised by
	// the read above: the root was settled terminal (completed) at sequence 2
	// and its wake notice is still pending -- the debt must persist. No extra
	// reprojection is needed (any further Project would need a sequence
	// strictly greater than the last one, 2).

	// Selected-CHILD + message-identity oracles (the review's finding):
	// settle a child at the NEXT sequence, then pull BOTH selected notices through the real R8
	// consumer so delivery='done' and notice_message_id is named. A debt
	// implementation that discarded selected children or always returned an
	// empty NoticeMessageID now fails here.
	child := snap.Nodes[1].NodeID
	require.NotEqual(t, root, child)
	batchOutboxSettle(t, bs, ctx, snap, 3, child)
	pulled, err := bs.BatchPullNotices(ctx, message.NewService(q), "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, pulled, 2, "the R8 pull must consume both selected notices (root + child)")
	debt, err = store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, debt.Nodes, 2, "both settled nodes (root and child) are debt")
	var rootRef, childRef *BatchDebtNodeRef
	for i := range debt.Nodes {
		switch debt.Nodes[i].NodeID {
		case root:
			rootRef = &debt.Nodes[i]
		case child:
			childRef = &debt.Nodes[i]
		}
	}
	require.NotNil(t, rootRef, "the root ref must be in debt")
	require.NotNil(t, childRef, "a settled SELECTED CHILD must be debt, not discarded")
	require.Equal(t, root, childRef.RootID)
	require.NotEqual(t, childRef.RootID, childRef.NodeID, "child identity: RootID != NodeID")
	require.NotEmpty(t, childRef.NoticeMessageID,
		"the pulled child notice must keep its named message identity (BatchOutboxNameMessage)")

	// The terminal root stays debt even with BOTH refs present and its
	// notice already pulled (delivery='done'): reassert after the R8 pull.
	debt, err = store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, debt.Nodes, 2, "terminal root and pulled child BOTH stay debt")

	// delivery='void' is never debt (only the rerun path writes it; the
	// raw UPDATE is CHECK-legal: the SELECTED rows keep full metadata and
	// delivery!='none'; the second, unselected child must NOT be touched --
	// its CHECK requires delivery='none' while unselected).
	stmt := `UPDATE batch_nodes SET delivery = 'void', wake = 0 WHERE root_id = ? AND notice_selected = 1`
	requirePlaceholderCount(t, stmt, root)
	_, err = conn.ExecContext(ctx, stmt, root)
	require.NoError(t, err)
	debt, err = store.BatchPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, debt.Nodes, "a voided notice is not debt")
	require.Len(t, debt.Notices, 1, "the session notice half is unchanged")
}

// TestSessionNoticeStore_InsertAndListRoundTrip proves
// InsertSessionNoticeReturningID + ListSessionNotices against the retained
// session_notices queries: the id is nonzero, the round-trip preserves
// kind/text/wake, and an inserted id is never reused.
//
// Trace: Connect -> DROP async_jobs -> seedSession -> two inserts (one
// bound, one not) -> assertions on returned ids and the listed rows.
func TestSessionNoticeStore_InsertAndListRoundTrip(t *testing.T) {
	t.Parallel()
	store, _, q, ctx := newNoticeTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-notice"))

	id1, err := store.InsertSessionNoticeReturningID(ctx, "owner-notice", NoticeKindSupervision, "s", true, "call-1")
	require.NoError(t, err)
	require.NotZero(t, id1)
	id2, err := store.InsertSessionNoticeReturningID(ctx, "owner-notice", NoticeKindWakeFailed, "f", false, "")
	require.NoError(t, err)
	require.NotEqual(t, id1, id2)

	rows, err := store.ListSessionNotices(ctx, "owner-notice")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, NoticeKindSupervision, rows[0].Kind)
	require.Equal(t, "s", rows[0].Text)
	require.Equal(t, NoticeKindWakeFailed, rows[1].Kind)
}

// TestSessionNoticeStore_PendingChildQuestionsOverBatchRoots proves the new
// child-question reader against BOTH real notice shapes:
//
//   - the ACTUAL batch frame (agent's batch_install_question.go marker line
//   - "SUB-AGENT QUESTION: "), published with job_tool_call_id NULL like
//     BatchPublishQuestion does -- parsed by ParseBatchChildQuestionNotice,
//     never dropped by the bound-notice void rule;
//   - the legacy "SUB-AGENT QUESTION (session ...): " frame on a
//     JobToolCallID-bound notice over a LIVE root -- surfaced; over a
//     TERMINAL root -- dropped (the pull path's own void rule).
//
// Trace: Connect -> DROP async_jobs -> seedSession -> seedBatchRoot live
// (running, selected, CHECK-legal) + terminal (completed, selected) ->
// three InsertSessionNotice rows (batch frame unbound; legacy frame bound
// to the live tool_call_id; legacy frame bound to the terminal one) ->
// PendingChildQuestions: ListPendingSessionNoticesForOwner (session_notices)
// -> per row: unbound rows parse the batch frame (no void check, mirroring
// the pull path); bound rows run sessionNoticeVoidCondition ->
// GetLiveBatchRoot (batch_nodes, NOT async_jobs) then the legacy parser ->
// assertions on which notices surface and the parsed identity fields.
func TestSessionNoticeStore_PendingChildQuestionsOverBatchRoots(t *testing.T) {
	t.Parallel()
	store, conn, q, ctx := newNoticeTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-notice"))
	seedBatchRoot(t, ctx, conn, "", "root-live", "call-live", "running", "pending")
	seedBatchRoot(t, ctx, conn, "", "root-dead", "call-dead", "completed", "done")

	batchFrame := "[batch-question node=root.1 token=7 sequence=3 depth=1 child=child-9]\n" +
		"SUB-AGENT QUESTION: continue?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	legacyFrame := func(child string) string {
		return "supervision preamble\n\nSUB-AGENT QUESTION (session " + child + "): continue?\n\nafter"
	}
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice", NoticeKindChildQuestion, batchFrame, true, ""))
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice", NoticeKindChildQuestion, legacyFrame("child-1"), false, "call-live"))
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-notice", NoticeKindChildQuestion, legacyFrame("child-2"), false, "call-dead"))

	qs, err := store.PendingChildQuestions(ctx, "owner-notice")
	require.NoError(t, err)
	require.Len(t, qs, 2, "the batch question and the live-root legacy notice surface; the terminal-root one is dropped")

	var batch, legacy ChildQuestion
	for _, row := range qs {
		if row.NodeID == "root.1" {
			batch = row
		} else if row.ChildSessionID == "child-1" {
			legacy = row
		}
	}
	require.Equal(t, "root.1", batch.NodeID, "the batch question must surface with its node identity")
	require.Equal(t, "child-9", batch.ChildSessionID)
	require.Equal(t, uint64(7), batch.Token)
	require.Equal(t, uint64(3), batch.Sequence)
	require.Equal(t, "continue?", batch.Question)
	require.Empty(t, batch.DelegationToolCallID, "batch questions are unbound")
	require.NotZero(t, batch.NoticeID)
	require.Equal(t, "call-live", legacy.DelegationToolCallID)
	require.Equal(t, "continue?", legacy.Question)
	require.NotZero(t, legacy.NoticeID)
}

// TestSessionNoticeStore_BatchPublishQuestionEndToEnd publishes a child
// question through the PRODUCTION writer (BatchPublishQuestion, same
// package) against the dropped-async_jobs database and proves
// PendingChildQuestions surfaces it with the full batch identity. This is
// the end-to-end twin of the hand-seeded bound-legacy-frame cases above
// (which exercise the void rule BatchPublishQuestion rows never hit).
//
// Trace: Connect -> DROP async_jobs -> seedSession('owner', the FK target
// Reserve requires) -> bs.Reserve (batchStoreTestReservation with the root
// token pinned to 7: BatchPublishQuestion requires token > 0 and
// row_token == token) -> snapshot.Root.Token == 7 -> bs.Project (seq 2)
// moves child root.1 to state='running' WITH Token 7 (ProjectBatchNode
// overwrites token, so the live-state gate and token match both hold;
// state running is one of pending/running/cancelling) -> build the marker
// and text EXACTLY as the producer does (internal/agent/
// batch_install_question.go:35-36: marker must end in '\n') ->
// bs.BatchPublishQuestion('owner', root.1, 7, marker, text): in the writer
// tx it SELECTs the node row (token match + live state), then INSERTs the
// session_notices row with wake=1, delivery='pending', job_tool_call_id
// NULL (session_notices.owner FKs to the seeded sessions row) ->
// store.PendingChildQuestions: ListPendingSessionNoticesForOwner -> the
// row is UNBOUND, so the bound-notice void rule never runs and
// ParseBatchChildQuestionNotice parses the frame -> assert node/token/
// sequence/child identity and the one-line question "continue?".
func TestSessionNoticeStore_BatchPublishQuestionEndToEnd(t *testing.T) {
	t.Parallel()
	store, conn, q, ctx := newNoticeTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner"))
	bs := NewBatchStore(conn)

	p := batchStoreTestReservation()
	p.Root.Values.Token = 7
	r, err := bs.Reserve(ctx, p)
	require.NoError(t, err)
	snap := r.Snapshot
	require.Equal(t, int64(7), snap.Root.Token)
	child := snap.Nodes[1].NodeID

	proj := batchStoreTestUpdate(child, "running")
	proj.Token = 7 // ProjectBatchNode overwrites token; keep the incarnation live
	require.NoError(t, bs.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{
		Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{proj},
	}}}))

	// Mirrors internal/agent/batch_install_question.go:35-36 verbatim.
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n",
		child, 7, 3, 1, "child-9")
	text := marker + "SUB-AGENT QUESTION: " + "continue?" +
		"\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.NoError(t, bs.BatchPublishQuestion(ctx, "owner", child, 7, marker, text))

	qs, err := store.PendingChildQuestions(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, qs, 1, "the batch-published question must surface")
	require.Equal(t, child, qs[0].NodeID)
	require.Equal(t, "child-9", qs[0].ChildSessionID)
	require.Equal(t, uint64(7), qs[0].Token)
	require.Equal(t, uint64(3), qs[0].Sequence)
	require.Equal(t, "continue?", qs[0].Question)
	require.Empty(t, qs[0].DelegationToolCallID, "BatchPublishQuestion rows are unbound")
	require.NotZero(t, qs[0].NoticeID)
}

// TestParseBatchChildQuestionNotice_ProducerFrameFormat pins the parser
// against the producer's EXACT frame. internal/agent/
// batch_install_question.go:35-36 writes
//
//	marker + "SUB-AGENT QUESTION: " + n.Question + "\nAnswer with ..."
//
// i.e. ONE newline separates the question from the guidance sentence -- so
// the documented parse rule is: the question ends at the FIRST '\n' after
// the "SUB-AGENT QUESTION: " prefix. A multi-line model question is
// truncated at its first line by this format; that is the rule, not a bug.
func TestParseBatchChildQuestionNotice_ProducerFrameFormat(t *testing.T) {
	t.Parallel()
	// Copied verbatim from internal/agent/batch_install_question.go:35-36.
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n",
		"root.1", 7, 3, 1, "child-9")
	text := marker + "SUB-AGENT QUESTION: " + "line one\nline two" +
		"\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."

	got, ok := ParseBatchChildQuestionNotice(text)
	require.True(t, ok)
	require.Equal(t, "root.1", got.NodeID)
	require.Equal(t, "child-9", got.ChildSessionID)
	require.Equal(t, uint64(7), got.Token)
	require.Equal(t, uint64(3), got.Sequence)
	require.Equal(t, "line one", got.Question,
		"the question ends at the first newline; the guidance sentence is not part of it")
}
