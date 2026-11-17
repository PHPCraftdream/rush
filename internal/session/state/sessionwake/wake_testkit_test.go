// Test-support helpers copied byte-for-byte from package sessionhosts
// (host_lock_test.go) when the wake-schedule store moved to its own state
// package: seedSession is unexported there, and the wake tests seed owner
// sessions through the same query.
package sessionwake_test

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
}
