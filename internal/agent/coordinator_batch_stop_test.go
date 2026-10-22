package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorBatchStopZerosDelegatedChildWake(t *testing.T) {
	t.Run("batch-only", func(t *testing.T) {
		t.Setenv("RUSH_BATCH_RUNTIME", "1")
		env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
		ctx := context.Background()
		parent, child := env.sessionID, "batch-stop-child"
		_, err := env.q.CreateSession(ctx, db.CreateSessionParams{ID: child, ParentSessionID: nullString(parent), Title: child})
		require.NoError(t, err)
		_, err = env.store.Reserve(ctx, session.BatchReservation{
			Owner: parent, Host: "host-stop", RunnerID: "runner-stop", ToolCallID: "delegate-stop", InputHash: "parent-hash", Sequence: 1,
			Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("agent"), State: "running", ControlState: "none", ChildSessionID: nullString(child)}}}},
		})
		require.NoError(t, err)
		snap, err := env.store.Reserve(ctx, session.BatchReservation{
			Owner: child, Host: "host-stop", RunnerID: "runner-stop", ToolCallID: "child-work", InputHash: "child-hash", Sequence: 1,
			Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "completed", ControlState: "none", ToolCallID: nullString("child-work"), InputHash: nullString("child-hash")}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("bash"), State: "completed", ControlState: "none"}}}},
		})
		require.NoError(t, err)
		_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET notice_selected=1, notice_kind='root', notice_state='completed', notice_cause='', notice_sequence=1, notice_ordinal=0, delivery='done', wake=1, reacted=0 WHERE root_id=? AND node_id=root_id`, snap.Snapshot.Root.RootID)
		require.NoError(t, err)
		_, won, err := env.store.BatchDeliveryElectStarted(ctx, env.messages, child, snap.Snapshot.Root.RootID, message.CreateMessageParams{
			Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "batch stop test started"}},
			AutoResumed: true, BackgroundJobNotice: true,
		})
		require.NoError(t, err)
		require.True(t, won, "root announcement must be elected through the production delivery API")
		var wake int
		require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT wake FROM batch_nodes WHERE root_id=? AND node_id=root_id`, snap.Snapshot.Root.RootID).Scan(&wake))
		require.Equal(t, 1, wake, "seeded completion must be pending reaction debt")
		debt, err := env.store.BatchReactionVisibleDebtExists(ctx, child)
		require.NoError(t, err)
		require.True(t, debt, "seeded completion must be visible debt")

		c := newCutoverCoordinator(t, env)
		cutoverBindFacts(t, c, env)
		c.stopTree(parent)
		require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT wake FROM batch_nodes WHERE root_id=? AND node_id=root_id`, snap.Snapshot.Root.RootID).Scan(&wake))
		require.Zero(t, wake)
		require.True(t, c.arb.snapshot(child).Suspended)

		fresh := newCutoverCoordinator(t, env)
		cutoverBindFacts(t, fresh, env)
		facts := coordinatorBatchCutoverFacts(fresh)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		debt, err = facts.SelectedDebtExists(readCtx, child)
		cancel()
		require.NoError(t, err)
		require.False(t, fresh.arb.snapshot(child).Suspended)
		require.False(t, debt, "fresh coordinator must observe no durable reaction debt")
	})
}
