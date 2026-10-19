// The pending-inclusive debt read must execute the session_notices debt
// statement EXACTLY once per public call: the previous shape read it twice
// (once through batchDebtSnapshot for the refs, once for the kinds). The
// count is taken at the DRIVER STATEMENT layer (batch_sqllog_test.go shim)
// over the WHOLE public BatchReactionPendingInclusiveDebtRows call, so a
// duplicate notices read reintroduced in the public wrapper OR the extracted
// helper makes the count 2 and fails this test; removing the read makes it 0.
package session

import (
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBatchPendingInclusiveDebtRowsReadsNoticesOnce(t *testing.T) {
	t.Parallel()
	_, conn, _, ctx, dataDir := batchServedTestEnv(t)
	logDB, log := newBatchSQLLogDB(t, filepath.Join(dataDir, "rush.db"))
	q := db.New(conn)
	// Seed node debt with the exact batch_reaction_test.go recipe: a root
	// reserved, settled with a notice (notice_selected=1, wake=1, delivery
	// done) and announced — >= 1 row for batchDebtNodesVoidAwareSQL.
	s := NewBatchStore(conn)
	m := message.NewService(q)
	snap := batchOutboxReserve(t, s, ctx, 1)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	// Seed one retained notice debt row for BatchOutboxDebtNotices (same
	// pattern as TestBatchOutboxDebtExclusionsAndStaleNoticeSnapshots).
	n, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "question", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET delivery='done',notice_message_id='first' WHERE id=?", n.ID)
	require.NoError(t, err)

	countingStore := NewBatchStore(logDB)
	rows, err := countingStore.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.NotEmpty(t, rows.Nodes)
	require.NotEmpty(t, rows.Notices)
	require.Equal(t, 1, batchSQLLogCountContaining(log, "FROM session_notices", "wake = 1"), "one notices read per pending-inclusive call")
}
