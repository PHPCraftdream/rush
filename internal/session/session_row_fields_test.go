package session_test

// R7C-4 (store part): every read path must carry the row's ended_reason,
// budget columns and cancel flag, not only ListAll. `sessions show` loads
// through Get and printed neither "Ended:" nor the budget.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSessionReaders_CarryEndedReasonBudgetAndCancelFlag round-trips rows
// with ended_reason, all three budget columns and cancel_requested set
// through every reader that builds a Session from a row.
//
// Revert-check: dropping the five assignments from fromDBItem turns every
// reader red (before the fix only ListAll, which filled them itself, was
// green).
func TestSessionReaders_CarryEndedReasonBudgetAndCancelFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	svc := session.NewService(db.New(conn), conn)

	root, err := svc.Create(ctx, "root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(ctx, "row-fields-child", root.ID, "child")
	require.NoError(t, err)

	// A fresh row reads as zero values.
	fresh, err := svc.Get(ctx, root.ID)
	require.NoError(t, err)
	require.Empty(t, fresh.EndedReason)
	require.Zero(t, fresh.BudgetMaxCost)
	require.False(t, fresh.CancelRequested)

	for _, id := range []string{root.ID, child.ID} {
		require.NoError(t, svc.SetEndedReason(ctx, id, "canceled"))
		require.NoError(t, svc.SetBudget(ctx, id, 2.5, 1000, 90))
		require.NoError(t, svc.RequestCancel(ctx, id))
	}

	check := func(t *testing.T, want string, got session.Session) {
		t.Helper()
		require.Equal(t, want, got.ID)
		require.Equal(t, "canceled", got.EndedReason)
		require.InDelta(t, 2.5, got.BudgetMaxCost, 1e-9)
		require.Equal(t, int64(1000), got.BudgetMaxTokens)
		require.Equal(t, int64(90), got.BudgetTimeoutSec)
		require.True(t, got.CancelRequested)
	}
	find := func(t *testing.T, list []session.Session, id string) session.Session {
		t.Helper()
		for _, s := range list {
			if s.ID == id {
				return s
			}
		}
		require.FailNow(t, "session not listed", id)
		return session.Session{}
	}

	t.Run("Get", func(t *testing.T) {
		got, err := svc.Get(ctx, child.ID)
		require.NoError(t, err)
		check(t, child.ID, got)
	})
	t.Run("GetLast", func(t *testing.T) {
		got, err := svc.GetLast(ctx)
		require.NoError(t, err)
		check(t, root.ID, got)
	})
	t.Run("List", func(t *testing.T) {
		got, err := svc.List(ctx)
		require.NoError(t, err)
		check(t, root.ID, find(t, got, root.ID))
	})
	t.Run("ListSubSessions", func(t *testing.T) {
		got, err := svc.ListSubSessions(ctx, root.ID)
		require.NoError(t, err)
		check(t, child.ID, find(t, got, child.ID))
	})
	t.Run("ListAll", func(t *testing.T) {
		got, err := svc.ListAll(ctx)
		require.NoError(t, err)
		check(t, child.ID, find(t, got, child.ID))
	})
	t.Run("CreateTaskSession_existing", func(t *testing.T) {
		got, err := svc.CreateTaskSession(ctx, "row-fields-child", root.ID, "again")
		require.NoError(t, err)
		check(t, child.ID, got)
	})
}
