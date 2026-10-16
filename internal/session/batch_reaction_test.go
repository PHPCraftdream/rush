package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBatchOutboxDebtExclusionsAndStaleNoticeSnapshots(t *testing.T) {
	for _, field := range []string{"wake", "reacted", "void", "announcement"} {
		t.Run(field, func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchOutboxReserve(t, s, ctx, 1)
			m := message.NewService(db.New(conn))
			batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
			batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
			update := "wake=0"
			switch field {
			case "reacted":
				update = "reacted=1"
			case "void":
				update = "delivery='void'"
			case "announcement":
				update = "announced=0"
			}
			_, err := conn.ExecContext(ctx, "UPDATE batch_nodes SET "+update+" WHERE node_id=?", snap.Root.RootID)
			require.NoError(t, err)
			has, err := s.BatchReactionDebtExists(ctx, "owner")
			require.NoError(t, err)
			require.False(t, has)
			rows, err := s.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
			require.NoError(t, err)
			require.Empty(t, rows.Nodes)
		})
	}
	s, conn, ctx := batchStoreTestDB(t)
	q := db.New(conn)
	n, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "question", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET delivery='done',notice_message_id='first' WHERE id=?", n.ID)
	require.NoError(t, err)
	snap, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.NoError(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", snap))
	max, err := s.BatchReactionMaxWakeAttempts(ctx, "owner", snap)
	require.NoError(t, err)
	require.Equal(t, 1, max)
	for _, change := range []string{"notice_message_id='second'", "delivery='pending'", "delivery='void'", "wake=0", "reacted=1"} {
		_, err = conn.ExecContext(ctx, "UPDATE session_notices SET notice_message_id='first',delivery='done',wake=1,reacted=0,wake_attempts=5 WHERE id=?", n.ID)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "UPDATE session_notices SET "+change+" WHERE id=?", n.ID)
		require.NoError(t, err)
		require.NoError(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", snap))
		count, e := s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", snap, "stale")
		require.NoError(t, e)
		require.Zero(t, count)
		max, e = s.BatchReactionMaxWakeAttempts(ctx, "owner", snap)
		require.NoError(t, e)
		require.Zero(t, max)
		row, e := q.GetSessionNotice(ctx, n.ID)
		require.NoError(t, e)
		require.EqualValues(t, 5, row.WakeAttempts)
	}
}

func TestBatchOutboxReactionAndWakeZeroLateFailureAtomicity(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	_, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	n, err := db.New(conn).InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "question", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET delivery='done',notice_message_id='question' WHERE id=?", n.ID)
	require.NoError(t, err)
	msg, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "before"}}})
	require.NoError(t, err)
	changed := msg.Clone()
	changed.Parts = []message.ContentPart{message.TextContent{Text: "answer"}}
	_, err = conn.ExecContext(ctx, "CREATE TRIGGER batch_fail_notice_reaction BEFORE UPDATE OF reacted ON session_notices BEGIN SELECT RAISE(ABORT,'reaction failed'); END")
	require.NoError(t, err)
	require.ErrorContains(t, s.BatchReactionMarkReactedWithMessageUpdate(ctx, m, "owner", changed), "reaction failed")
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Zero(t, got.Root.Reacted)
	stored, err := m.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.Equal(t, "before", stored.FullText())
	captured, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.ErrorContains(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", captured), "reaction failed")
	got, err = s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Zero(t, got.Root.WakeAttempts)
	_, err = conn.ExecContext(ctx, "DROP TRIGGER batch_fail_notice_reaction")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "CREATE TRIGGER batch_fail_notice_wake BEFORE UPDATE OF wake ON session_notices BEGIN SELECT RAISE(ABORT,'wake failed'); END")
	require.NoError(t, err)
	require.ErrorContains(t, s.BatchReactionSetWakeZeroForOwners(ctx, []string{"owner"}), "wake failed")
	got, err = s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Root.Wake)
	_, err = conn.ExecContext(ctx, "DROP TRIGGER batch_fail_notice_wake")
	require.NoError(t, err)
	require.NoError(t, s.BatchReactionSetWakeZeroForOwners(ctx, []string{"owner"}))
	notice, err := db.New(conn).GetSessionNotice(ctx, n.ID)
	require.NoError(t, err)
	require.Zero(t, notice.Wake)
}

func TestBatchOutboxPendingVisibleAndQuestionReaction(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	has, err := s.BatchReactionDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.True(t, has)
	visible, err := s.BatchReactionVisibleDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.False(t, visible)
	captured, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.True(t, captured.Empty())
	pending, err := s.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, pending.Nodes, 1)
	// The retained question has an arbitrary legacy binding. Reaction must not
	// resolve it via async_jobs, and must not rewrite binding/delivery semantics.
	q := db.New(conn)
	n, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "which?", Wake: 1, JobToolCallID: sql.NullString{String: "absent-legacy-job", Valid: true}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, func() error {
		tx, e := conn.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		qt := db.New(tx)
		_, e = qt.PullPendingSessionNotice(ctx, db.PullPendingSessionNoticeParams{ID: n.ID, UpdatedAt: 2})
		if e != nil {
			return e
		}
		msg, e := m.CreateTx(ctx, tx, "owner", batchOutboxText("which?"))
		if e != nil {
			return e
		}
		_, e = qt.SetSessionNoticeMessageID(ctx, db.SetSessionNoticeMessageIDParams{ID: n.ID, NoticeMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: 2})
		if e != nil {
			return e
		}
		return tx.Commit()
	}())
	captured, err = s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, captured.Nodes)
	require.Len(t, captured.Notices, 1)
	out, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, out, 1)
	captured, err = s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, captured.Nodes, 1)
	require.Len(t, captured.Notices, 1)
	question, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "Need clarification: which?"}}})
	require.NoError(t, err)
	require.NoError(t, s.BatchReactionMarkReactedWithMessageUpdate(ctx, m, "owner", question))
	has, err = s.BatchReactionDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.False(t, has)
	notice, err := q.GetSessionNotice(ctx, n.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, notice.Reacted)
	require.Equal(t, "absent-legacy-job", notice.JobToolCallID.String)
	require.Equal(t, "done", notice.Delivery)
	owners, err := s.BatchReactionDebtOwners(ctx, []string{"owner", "missing"})
	require.NoError(t, err)
	require.Empty(t, owners)
}

func TestBatchOutboxStaleSnapshotFencesAndMaxAttempts(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	_, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	captured, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", captured))
	}
	max, err := s.BatchReactionMaxWakeAttempts(ctx, "owner", captured)
	require.NoError(t, err)
	require.Equal(t, 3, max)
	for _, field := range []string{"owner", "root", "node", "message"} {
		t.Run(field, func(t *testing.T) {
			stale := BatchDebtSnapshot{Owner: captured.Owner, Nodes: append([]BatchDebtNodeRef(nil), captured.Nodes...)}
			switch field {
			case "owner":
				stale.Owner = "other"
			case "root":
				stale.Nodes[0].RootID = "old"
			case "node":
				stale.Nodes[0].NodeID = "old"
			case "message":
				stale.Nodes[0].NoticeMessageID = "old"
			}
			count, e := s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", stale, "must not insert")
			if field == "owner" {
				require.ErrorIs(t, e, ErrBatchProjectionConflict)
			} else {
				require.NoError(t, e)
			}
			require.Zero(t, count)
		})
	}
	// Same tool-call text creates a fresh root; old accounting cannot affect it.
	fresh, err := s.Reserve(ctx, func() BatchReservation {
		p := batchStoreTestReservation()
		p.Root.Children = p.Root.Children[:1]
		return p
	}())
	require.NoError(t, err)
	require.NotEqual(t, snap.Root.RootID, fresh.Snapshot.Root.RootID)
	batchOutboxSettle(t, s, ctx, fresh.Snapshot, 2, fresh.Snapshot.Root.RootID)
	batchOutboxAck(t, s, ctx, m, fresh.Snapshot.Root.RootID)
	_, err = s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	// Re-pull identity change invalidates the old delivery snapshot.
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET delivery='pending', notice_message_id=NULL, wake_attempts=2 WHERE node_id=?", snap.Root.RootID)
	require.NoError(t, err)
	_, err = s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	max, err = s.BatchReactionMaxWakeAttempts(ctx, "owner", captured)
	require.NoError(t, err)
	require.Zero(t, max)
	require.NoError(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", captured))
	count, err := s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", captured, "stale")
	require.NoError(t, err)
	require.Zero(t, count)
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 2, got.Root.WakeAttempts)
	require.Zero(t, got.Root.Reacted)
	current, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, current.Nodes, 2)
	count, err = s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", current, "closed")
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	count, err = s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", current, "duplicate")
	require.NoError(t, err)
	require.Zero(t, count)
	failed, err := s.BatchReactionListReactedFailedText(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, failed, 2)
	max, err = s.BatchReactionMaxWakeAttempts(ctx, "owner", current)
	require.NoError(t, err)
	require.Zero(t, max)
}

func TestBatchOutboxFailureMarkerRollbackAndReactionRollback(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	_, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	n, err := db.New(conn).InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "question", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET delivery='done',notice_message_id='question-message' WHERE id=?", n.ID)
	require.NoError(t, err)
	captured, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, captured.Notices, 1)
	_, err = conn.ExecContext(ctx, "CREATE TRIGGER batch_fail_marker BEFORE INSERT ON session_notices WHEN NEW.kind='wake_failed' BEGIN SELECT RAISE(ABORT,'marker failed'); END")
	require.NoError(t, err)
	count, err := s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", captured, "failed")
	require.ErrorContains(t, err, "marker failed")
	require.Zero(t, count)
	visible, err := s.BatchReactionVisibleDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.True(t, visible)
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Zero(t, got.Root.Reacted)
	notice, err := db.New(conn).GetSessionNotice(ctx, n.ID)
	require.NoError(t, err)
	require.Zero(t, notice.Reacted)
	_, err = conn.ExecContext(ctx, "DROP TRIGGER batch_fail_marker")
	require.NoError(t, err)
	count, err = s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", captured, "failed")
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	msg, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "before"}}})
	require.NoError(t, err)
	changed := msg.Clone()
	changed.Parts = []message.ContentPart{message.TextContent{Text: "after"}}
	_, err = conn.ExecContext(ctx, "CREATE TRIGGER batch_fail_clear BEFORE UPDATE OF reacted_failed ON session_notices BEGIN SELECT RAISE(ABORT,'clear failed'); END")
	require.NoError(t, err)
	require.ErrorContains(t, s.BatchReactionMarkReactedWithMessageUpdate(ctx, m, "owner", changed), "clear failed")
	persisted, err := m.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.Equal(t, "before", persisted.FullText())
	failed, err := s.BatchReactionListReactedFailedText(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, failed, 2)
	_, err = conn.ExecContext(ctx, "DROP TRIGGER batch_fail_clear")
	require.NoError(t, err)
	require.NoError(t, s.BatchReactionMarkReactedWithMessageUpdate(ctx, m, "owner", changed))
	failed, err = s.BatchReactionListReactedFailedText(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, failed)
}

func TestBatchOutboxWakeZeroAndNoticeOwnerFence(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, seedSession(ctx, db.New(conn), "other"))
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	q := db.New(conn)
	n, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "other", Kind: NoticeKindChildQuestion, Text: "other", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET delivery='done',notice_message_id='other-message' WHERE id=?", n.ID)
	require.NoError(t, err)
	forged := BatchDebtSnapshot{Owner: "owner", Notices: []DebtNoticeRef{{ID: n.ID, NoticeMessageID: "other-message"}}}
	require.NoError(t, s.BatchReactionIncrementWakeAttempts(ctx, "owner", forged))
	count, err := s.BatchReactionSettleReactedFailedWithMarker(ctx, "owner", forged, "forged")
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, s.BatchReactionSetWakeZeroForOwners(ctx, []string{"owner"}))
	has, err := s.BatchReactionDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.False(t, has)
	has, err = s.BatchReactionDebtExists(ctx, "other")
	require.NoError(t, err)
	require.True(t, has)
	owners, err := s.BatchReactionDebtOwners(ctx, []string{"owner", "other"})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"other": true}, owners)
	notice, err := q.GetSessionNotice(ctx, n.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, notice.Wake)
	require.Zero(t, notice.Reacted)
	require.Zero(t, notice.WakeAttempts)
	require.NoError(t, s.BatchReactionSetWakeZeroForOwners(ctx, []string{"other"}))
	has, err = s.BatchReactionDebtExists(ctx, "other")
	require.NoError(t, err)
	require.False(t, has)
}
