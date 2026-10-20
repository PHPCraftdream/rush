// Package N: durable selected-notice result projection. The notice pull must
// deliver the job's real result (the sole task child's OutcomeSummary) for an
// ordinary one-task structural root whose own summary is empty, while never
// borrowing for multi-task roots, non-task children, or roots that already
// carry their own summary.
//
// For ALL six tests below, the first production-facing assertion executed is
// require.NoError after db.Connect inside batchStoreTestDB
// (batch_store_test.go); the per-test comments name the first
// REGRESSION-SENSITIVE assertion and the mutation it catches.
package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchPullResultUpdate lets each settled node carry its OWN summary (and,
// for the root, its own notice state/cause), unlike batchOutboxSettle which
// reuses the node id as the summary.
type batchPullResultUpdate struct {
	id             string
	state          string
	summary        string
	noticeState    string
	noticeCause    string
	noticeStateSet bool
	noticeCauseSet bool
}

// batchPullResultSettle mirrors batchOutboxSettle but every node carries its
// own summary; the root alone is named as the "root" notice kind, tasks keep
// the zero-value NoticeKind.
func batchPullResultSettle(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot, seq int64, updates ...batchPullResultUpdate) {
	t.Helper()
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	for _, u := range updates {
		p.Updates = append(p.Updates, db.ProjectBatchNodeParams{
			NodeID: u.id, State: u.state, ControlState: "none",
			OutcomeSummary: u.summary, OutcomeState: u.state,
		})
		if u.id != snap.Root.RootID {
			// Real notice selection (internal/jobbatch/notice.go
			// selectNotices) suppresses a task child's notice when its
			// settled parent settles (default Notify != NotifyEach), so
			// only the root's notice is selected for these shapes.
			continue
		}
		n := batchStoreTestNotice(u.id)
		if u.noticeStateSet {
			n.NoticeState = batchStoreTestString(u.noticeState)
		}
		if u.noticeCauseSet {
			n.NoticeCause = batchStoreTestString(u.noticeCause)
		}
		// Root alone: named as the "root" notice kind, as selectNotices
		// names the root kind for a settled batch.
		n.NoticeKind = batchStoreTestString("root")
		p.Notices = append(p.Notices, n)
	}
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: seq, Roots: []BatchRootProjection{p}}))
}

// batchPullResultReserve reserves a root with the given number of task
// children (trimmed from the two-child default reservation).
func batchPullResultReserve(t *testing.T, s *BatchStore, ctx context.Context, children int) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.Root.Children = p.Root.Children[:children]
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

// batchPullResultPull announces the root then pulls, returning the notices;
// the message text reveals exactly what the projection produced.
func batchPullResultPull(t *testing.T, s *BatchStore, ctx context.Context, m message.Service, snap BatchSnapshot) ([]BatchPulledNotice, BatchNoticeRow) {
	t.Helper()
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	var got BatchNoticeRow
	out, err := s.BatchPullNotices(ctx, m, "owner", func(row BatchNoticeRow) message.CreateMessageParams {
		got = row
		return batchOutboxText(row.OutcomeSummary)
	})
	require.NoError(t, err)
	return out, got
}

func TestBatchPullResult_OneTaskRootEmptySummaryGetsTaskSummary(t *testing.T) {
	// Regression-sensitive: out[0].Message.FullText() == "PN-task-output".
	// Mutation caught: deleting the batchProjectSelectedNoticeSummary call in
	// batchPullOneNotice leaves the empty root summary and this assertion
	// fails.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchPullResultReserve(t, s, ctx, 1)
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Nodes[1].NodeID, state: "completed", summary: "PN-task-output"},
		batchPullResultUpdate{id: snap.Root.RootID, state: "completed", summary: ""},
	)
	out, got := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	require.Equal(t, "PN-task-output", out[0].Message.FullText())
	require.Equal(t, snap.Root.RootID, out[0].RootID)
	require.Equal(t, snap.Root.RootID, out[0].NodeID)
	require.Equal(t, "completed", got.State)
}

func TestBatchPullResult_RootOwnSummaryPassesThrough(t *testing.T) {
	// Regression-sensitive: out[0].Message.FullText() == "PN-root-own".
	// Mutation caught: projecting unconditionally (dropping the
	// OutcomeSummary != "" guard) would emit the child text instead.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchPullResultReserve(t, s, ctx, 1)
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Nodes[1].NodeID, state: "completed", summary: "PN-task-other"},
		batchPullResultUpdate{id: snap.Root.RootID, state: "completed", summary: "PN-root-own"},
	)
	out, _ := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	require.Equal(t, "PN-root-own", out[0].Message.FullText())
}

func TestBatchPullResult_MultiTaskRootDoesNotBorrow(t *testing.T) {
	// Regression-sensitive: out[0].Message.FullText() == "".
	// Mutation caught: dropping the len(children) != 1 guard would emit
	// "PN-a", one arbitrary child's result, for a two-task batch.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchPullResultReserve(t, s, ctx, 2)
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Nodes[1].NodeID, state: "completed", summary: "PN-a"},
		batchPullResultUpdate{id: snap.Nodes[2].NodeID, state: "completed", summary: "PN-b"},
		batchPullResultUpdate{id: snap.Root.RootID, state: "completed", summary: ""},
	)
	out, _ := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	require.Equal(t, "", out[0].Message.FullText())
}

func TestBatchPullResult_FailedRootKeepsAuthoritativeStateAndCause(t *testing.T) {
	// Regression-sensitive: got.State == "failed" / got.Cause == "stop".
	// Mutation caught: a variant that overwrote row.State/Cause from the
	// child would fail the State/Cause assertions. This test uses a
	// summary-only builder (batchOutboxText of row.OutcomeSummary), so it
	// does NOT exercise the real FormatAsyncCompletion stop wording -- it
	// proves only that the pulled row keeps its own state/cause while
	// borrowing the child's summary content.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchPullResultReserve(t, s, ctx, 1)
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Nodes[1].NodeID, state: "completed", summary: "PN-partial"},
		batchPullResultUpdate{id: snap.Root.RootID, state: "failed", summary: "", noticeState: "failed", noticeCause: "stop", noticeStateSet: true, noticeCauseSet: true},
	)
	out, got := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	require.Equal(t, "PN-partial", out[0].Message.FullText())
	require.Equal(t, "failed", got.State)
	require.Equal(t, "stop", got.Cause)
}

func TestBatchPullResult_NoTaskChildUnchanged(t *testing.T) {
	// Regression-sensitive: require.NoError on the pull plus the empty
	// FullText(). Mutation caught: indexing children[0] without the length
	// check would panic / misbehave on a childless root.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	p := batchStoreTestReservation()
	p.Root.Children = nil
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	snap := r.Snapshot
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Root.RootID, state: "completed", summary: ""},
	)
	out, _ := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	require.Equal(t, "", out[0].Message.FullText())
}

func TestBatchPullResult_RepeatedPullDeliversExactlyOnce(t *testing.T) {
	// Regression-sensitive: messages count == 2 and the second pull is
	// empty. Mutation caught: breaking the delivery CAS (e.g. re-setting
	// delivery='pending') would double-deliver.
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchPullResultReserve(t, s, ctx, 1)
	batchPullResultSettle(t, s, ctx, snap, 2,
		batchPullResultUpdate{id: snap.Nodes[1].NodeID, state: "completed", summary: "PN-task-output"},
		batchPullResultUpdate{id: snap.Root.RootID, state: "completed", summary: ""},
	)
	out, _ := batchPullResultPull(t, s, ctx, m, snap)
	require.Len(t, out, 1)
	again, err := s.BatchPullNotices(ctx, m, "owner", func(row BatchNoticeRow) message.CreateMessageParams {
		return batchOutboxText(row.NodeID)
	})
	require.NoError(t, err)
	require.Empty(t, again)
	var count int
	require.NoError(t, s.writer.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE session_id='owner'").Scan(&count))
	require.Equal(t, 2, count, "started announcement plus exactly one notice")
}
