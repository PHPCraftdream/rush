package cmd

// R4C-3: `rush run --continue` continues "the most recently updated top-level
// session". GetLastSession had no top-level filter, so a delegated worker's
// child session that was updated after its root was taken for it.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// TestFoldRoleModel_ContinueNeverResolvesAChildSession: a root and a child
// updated LATER; --continue resolves the root for every role, so the pin comes
// from the root and the run continues the root instead of failing "cannot
// continue a child session" (worker/reviewer) or continuing the child (smart).
//
// Revert-check: dropping `parent_session_id IS NULL` from GetLastSession
// (internal/db/sql/sessions.sql, regenerated) resolves the child: the pin is
// empty and the resolved id is the child's.
func TestFoldRoleModel_ContinueNeverResolvesAChildSession(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	root, err := s.Create(ctx, "root")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "call-1", root.ID, "worker child")
	require.NoError(t, err)
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, root.ID,
		&session.ModelSlotUpdate{Provider: "root-prov", Model: "root-worker"},
		&session.ModelSlotUpdate{Provider: "root-prov", Model: "root-reviewer"}))
	// updated_at has one-second granularity: pin the order explicitly.
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET updated_at = 1000 WHERE id = ?`, root.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET updated_at = 2000 WHERE id = ?`, child.ID)
	require.NoError(t, err)

	last, err := s.GetLast(ctx)
	require.NoError(t, err)
	require.Equal(t, root.ID, last.ID, "GetLast is the most recently updated TOP-LEVEL session")

	a := &app.App{Sessions: s}
	for _, tc := range []struct {
		role string
		typ  config.SelectedModelType
		want string
	}{
		{"worker", config.SelectedModelTypeWorker, "root-prov/root-worker"},
		{"reviewer", config.SelectedModelTypeReviewer, "root-prov/root-reviewer"},
	} {
		model, resolved, useLast, err := foldRoleModel(ctx, a, tc.role, tc.typ, "", "", true)
		require.NoError(t, err, tc.role)
		require.Equal(t, tc.want, model, tc.role)
		require.Equal(t, root.ID, resolved, tc.role)
		require.False(t, useLast, tc.role)
	}
}
