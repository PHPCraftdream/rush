// The pending-inclusive debt read must execute the session_notices debt
// statement EXACTLY once per call: the previous shape read it twice (once
// through batchDebtSnapshot for the refs, once for the kinds). The counting
// DBTX sniffs the statement text, so restoring the second read fails this
// test with a count of two.
package session

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

type batchDebtReadCountingDB struct {
	db.DBTX
	debtNoticeReads int
}

func (c *batchDebtReadCountingDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if strings.Contains(query, "FROM session_notices") && strings.Contains(query, "wake = 1") {
		c.debtNoticeReads++
	}
	return c.DBTX.QueryContext(ctx, query, args...)
}

func TestBatchPendingInclusiveDebtRowsReadsNoticesOnce(t *testing.T) {
	t.Parallel()
	_, conn, ctx := batchStoreTestDB(t)
	counting := &batchDebtReadCountingDB{DBTX: db.New(conn)}
	_, err := batchReactionPendingInclusiveDebtRows(ctx, counting, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, counting.debtNoticeReads, "one notices read per pending-inclusive call")
}
