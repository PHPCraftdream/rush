// R4C-3 (docs/reviews/2026-09-30-async-phase4-round4.md): `rush run --continue`
// resolves the most recently updated TOP-LEVEL session.
package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// R4C-3: `rush run --continue` (useLast) resolves the most recently updated
// TOP-LEVEL session; a delegated child updated later is never taken for it.
//
// Revert-check: dropping `parent_session_id IS NULL` from GetLastSession
// (internal/db/sql/sessions.sql, regenerated) resolves the child.
func TestResolveSession_UseLastSkipsChildSessions(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	child, err := h.app.Sessions.CreateTaskSession(ctx, "call-1", h.sessionID, "worker child")
	require.NoError(t, err)
	// updated_at has one-second granularity and a trigger rewrites it on every
	// UPDATE: drop the trigger (this DB is private) and pin the order.
	_, err = h.app.DB().ExecContext(ctx, `DROP TRIGGER update_sessions_updated_at`)
	require.NoError(t, err)
	_, err = h.app.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = 1000 WHERE id = ?`, h.sessionID)
	require.NoError(t, err)
	_, err = h.app.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = 2000 WHERE id = ?`, child.ID)
	require.NoError(t, err)

	sess, err := h.app.resolveSession(ctx, "", true)

	require.NoError(t, err)
	require.Equal(t, h.sessionID, sess.ID, "the root, not its later-updated child")
	require.Empty(t, sess.ParentSessionID)
}
