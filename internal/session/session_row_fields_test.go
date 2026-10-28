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

func TestSessionListAllCarriesFullRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	svc := session.NewService(db.New(conn), conn)

	created, err := svc.Create(ctx, "full row")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET cost_base = ?, smart_model_provider = ?,
		smart_model_id = ?, fast_model_provider = ?, fast_model_id = ? WHERE id = ?`,
		2.5, "test-provider", "test-smart", "fast-provider", "test-fast", created.ID)
	require.NoError(t, err)
	_, err = svc.IncrementCost(ctx, created.ID, 1.25)
	require.NoError(t, err)

	got, err := svc.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	want, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, want, got[0])
	require.Equal(t, 1.25, got[0].OwnCost)
	require.Equal(t, 2.5, got[0].CostBase)
	require.Equal(t, "test-provider", got[0].SmartModelProvider)
	require.Equal(t, "test-smart", got[0].SmartModelID)
	require.Equal(t, "fast-provider", got[0].FastModelProvider)
	require.Equal(t, "test-fast", got[0].FastModelID)

	var cost float64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT cost FROM sessions WHERE id = ?`, created.ID).Scan(&cost))
	require.Equal(t, 1.25, cost)
}

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
		t.Parallel()
		got, err := svc.Get(ctx, child.ID)
		require.NoError(t, err)
		check(t, child.ID, got)
	})
	t.Run("GetLast", func(t *testing.T) {
		t.Parallel()
		got, err := svc.GetLast(ctx)
		require.NoError(t, err)
		check(t, root.ID, got)
	})
	t.Run("List", func(t *testing.T) {
		t.Parallel()
		got, err := svc.List(ctx)
		require.NoError(t, err)
		check(t, root.ID, find(t, got, root.ID))
	})
	t.Run("ListSubSessions", func(t *testing.T) {
		t.Parallel()
		got, err := svc.ListSubSessions(ctx, root.ID)
		require.NoError(t, err)
		check(t, child.ID, find(t, got, child.ID))
	})
	t.Run("ListAll", func(t *testing.T) {
		t.Parallel()
		got, err := svc.ListAll(ctx)
		require.NoError(t, err)
		check(t, child.ID, find(t, got, child.ID))
	})
	t.Run("CreateTaskSession_existing", func(t *testing.T) {
		t.Parallel()
		got, err := svc.CreateTaskSession(ctx, "row-fields-child", root.ID, "again")
		require.NoError(t, err)
		check(t, child.ID, got)
	})
}
