// Test-support helper copied byte-for-byte from package sessionhosts
// (host_lock_test.go) after the host/batch state extraction: the batch
// tests seed owner sessions through the same query.
package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
}

// countMessages is the read-side helper the wake-schedule tests (now in
// package sessionwake_test) also carry; copied byte-for-byte so the batch
// pull tests keep their message-count assertions without crossing the
// package boundary.
func countMessages(t *testing.T, q *db.Queries, ctx context.Context, sessionID string) int64 {
	t.Helper()
	n, err := q.CountMessagesBySession(ctx, sessionID)
	require.NoError(t, err)
	return n
}
