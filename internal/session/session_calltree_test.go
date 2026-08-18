// Call-tree activity read accessors: empty-session behaviour of
// GetCallTreeActivity and the multi-chunk merging of
// GetCallTreeActivityBatch.
package session

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestGetCallTreeActivity_NoMessages confirms a freshly created session with
// no messages at all reports ok=false (nothing to report yet), not an error.
func TestGetCallTreeActivity_NoMessages(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "empty")
	require.NoError(t, err)

	act, ok, err := svc.GetCallTreeActivity(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, act.LatestUnix)
}

// TestGetCallTreeActivityBatch_Empty confirms calling the batch method with
// zero root IDs returns an empty, non-nil map without touching the DB.
func TestGetCallTreeActivityBatch_Empty(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	got, err := svc.GetCallTreeActivityBatch(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

// TestGetCallTreeActivityBatch_Chunking exercises the service-layer chunking
// of GetCallTreeActivityBatch: a root list larger than
// callTreeActivityBatchChunkSize must be split across multiple underlying
// queries and EVERY root's activity must come back in the merged map, not
// just the first chunk's. This guards against a regression where the
// chunking loop breaks early or drops trailing roots (e.g. an off-by-one on
// the final partial chunk). roots = chunkSize*3 + 7 exercises three full
// chunks plus a partial fourth.
func TestGetCallTreeActivityBatch_Chunking(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const roots = callTreeActivityBatchChunkSize*3 + 7

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	rootIDs := make([]string, 0, roots)
	for i := 0; i < roots; i++ {
		sid := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
			 VALUES (?, NULL, 'root', 100, 100)`, sid)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
			 VALUES (?, ?, 'assistant', '[]', 200, 200)`, uuid.NewString(), sid)
		require.NoError(t, err)
		rootIDs = append(rootIDs, sid)
	}
	require.NoError(t, tx.Commit())

	got, err := svc.GetCallTreeActivityBatch(ctx, rootIDs)
	require.NoError(t, err)
	require.Len(t, got, roots, "every root across all chunks must be present in the merged result")
}
