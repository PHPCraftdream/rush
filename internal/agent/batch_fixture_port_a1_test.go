// A1-specific batch-only helpers (drain accounting / provider failures),
// built on the shared foundation in batch_fixture_port_test.go.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// a1BlockPull makes the debt pull fail by routing through the foundation's
// failFixturePull (a BEFORE INSERT ON messages trigger: the batch pull
// aborts on its delivery-history insert). The returned func unblocks the
// pull before the test ends: the foundation's own cleanup only drops the
// trigger at test end, and a caller may need the pull back mid-test.
func a1BlockPull(t *testing.T, conn *sql.DB) (unblock func()) {
	t.Helper()
	failFixturePull(t, conn)
	return func() {
		_, err := conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_batch_pull`)
		require.NoError(t, err)
	}
}

// a1CaptureDebtSnapshot captures the session's visible debt through the
// bound node facts' CaptureVisibleDebt (the drainAttempt.batchSnapshot twin;
// the accounting acts only on the batch snapshot). batchCaptured mirrors
// drainAttempt.batchCaptured.
func a1CaptureDebtSnapshot(t *testing.T, f *attemptFixture, ctx context.Context) (batch session.BatchDebtSnapshot, batchCaptured bool) {
	t.Helper()
	bf := coordinatorBatchCutoverFacts(f.coord)
	var err error
	batch, err = bf.CaptureVisibleDebt(ctx, f.sessID)
	require.NoError(t, err)
	return batch, true
}
