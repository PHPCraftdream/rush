// A1-specific dual-mode helpers (drain accounting / provider failures),
// built on the shared foundation in batch_fixture_port_test.go.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// a1BlockPull makes the debt pull fail in BOTH runtime modes by routing
// through the foundation's failFixturePull (a BEFORE INSERT ON messages
// trigger: the batch pull aborts on its delivery-history insert, the legacy
// PullJobNotices on the same insert -- the same observable "the pull failed"
// the old async_jobs delivery trigger produced). The returned func unblocks
// the pull before the test ends: the foundation's own cleanup only drops the
// trigger at test end, and a caller may need the pull back mid-test.
func a1BlockPull(t *testing.T, conn *sql.DB) (unblock func()) {
	t.Helper()
	failFixturePull(t, conn)
	name := "fx_batch_pull"
	if !batchFlagEnabled() {
		name = "fx_legacy_pull"
	}
	return func() {
		_, err := conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS `+name)
		require.NoError(t, err)
	}
}

// a1CaptureDebtSnapshot captures the session's visible debt through the
// authority the flag selects: ON, the bound node facts' CaptureVisibleDebt
// (the drainAttempt.batchSnapshot twin -- under ON async_jobs is empty and
// the accounting acts only on the batch snapshot); OFF, the legacy store's
// CaptureDebtSnapshot. batchCaptured mirrors drainAttempt.batchCaptured.
func a1CaptureDebtSnapshot(t *testing.T, f *attemptFixture, ctx context.Context) (snap session.DebtSnapshot, batch session.BatchDebtSnapshot, batchCaptured bool) {
	t.Helper()
	if batchFlagEnabled() {
		bf := coordinatorBatchCutoverFacts(f.coord)
		var err error
		batch, err = bf.CaptureVisibleDebt(ctx, f.sessID)
		require.NoError(t, err)
		return session.DebtSnapshot{}, batch, true
	}
	var err error
	snap, err = f.store.CaptureDebtSnapshot(ctx, f.sessID)
	require.NoError(t, err)
	return snap, session.BatchDebtSnapshot{}, false
}
