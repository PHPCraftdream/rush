// CreateTaskSession idempotency (task #1038, doc sec.3.8): the deterministic
// per-delegation session id can be claimed twice for the SAME parent (a
// retried tool call, or an ASYNC-01 dead-host recovery reusing the id) and
// must return the existing row, never a raw UNIQUE-constraint error; the
// same id under a DIFFERENT parent stays an error.
package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCreateTaskSession_IdempotentForSameParent pins the happy repeat: the
// second call with the SAME (toolCallID, parentSessionID) pair returns the
// row the first call created, not a constraint error.
//
// Revert-check performed: reverted CreateTaskSession to the pre-#1038 body
// (plain CreateSession, no isSessionsIDUniqueConstraintError branch). This
// test FAILED with a raw "UNIQUE constraint failed: sessions.id" error.
// Restored the step-6 version; re-ran, passed. Diffed session_lifecycle.go
// against git HEAD after restoring: matches the committed step-6 code.
func TestCreateTaskSession_IdempotentForSameParent(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := NewService(q, conn)
	ctx := context.Background()

	parent, err := s.Create(ctx, "parent")
	require.NoError(t, err)

	first, err := s.CreateTaskSession(ctx, "tool-call-1", parent.ID, "delegation")
	require.NoError(t, err)
	require.Equal(t, parent.ID, first.ParentSessionID)

	second, err := s.CreateTaskSession(ctx, "tool-call-1", parent.ID, "delegation retry")
	require.NoError(t, err, "a repeat claim for the SAME parent must return the existing row, not a constraint error")
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, parent.ID, second.ParentSessionID)
	require.Equal(t, "delegation", second.Title, "the EXISTING row's own title survives -- the retry's title argument is not applied")
}

// TestCreateTaskSession_DifferentParentStaysAnError pins the negative half:
// the same toolCallID reused under a DIFFERENT parent is never silently
// accepted.
func TestCreateTaskSession_DifferentParentStaysAnError(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := NewService(q, conn)
	ctx := context.Background()

	parentA, err := s.Create(ctx, "parent-a")
	require.NoError(t, err)
	parentB, err := s.Create(ctx, "parent-b")
	require.NoError(t, err)

	_, err = s.CreateTaskSession(ctx, "tool-call-1", parentA.ID, "delegation")
	require.NoError(t, err)

	_, err = s.CreateTaskSession(ctx, "tool-call-1", parentB.ID, "delegation")
	require.Error(t, err)
	require.Contains(t, err.Error(), "different parent")
}
