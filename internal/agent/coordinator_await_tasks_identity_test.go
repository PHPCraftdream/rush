package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAwaitTasksDispatchUsesDurableIdentity(t *testing.T) {
	// The legacy flag-off fixture branch (in-process session.AsyncJobStore
	// Claim/Transition over newAwaitWorkerEnv) was DELETEd with the work
	// ledger: the batch runtime is unconditional (P3), so only the
	// durable-worker path remains.
	for _, fork := range []bool{false, true} {
		name := "durable-worker"
		if fork {
			name = "ordinary-fork"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("RUSH_BATCH_RUNTIME", "true")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			batchEnv := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
			c := newCutoverCoordinator(t, batchEnv)
			parent := batchEnv.sessionID
			child := "await-batch-child"
			if fork {
				childRow, _, forkErr := c.sessions.ForkSessionTx(ctx, parent, session.ForkOptions{NewID: child, ParentID: parent})
				require.NoError(t, forkErr)
				child = childRow.ID
			} else {
				_, err := batchEnv.q.CreateSession(ctx, db.CreateSessionParams{ID: child, ParentSessionID: nullString(parent), Title: child})
				require.NoError(t, err)
				_, err = batchEnv.store.Reserve(ctx, session.BatchReservation{
					Owner: parent, Host: "host-await", RunnerID: "runner-await", ToolCallID: "outer", InputHash: "hash", Sequence: 1,
					Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("agent"), State: "running", ControlState: "none", ChildSessionID: nullString(child)}}}},
				})
				require.NoError(t, err)
			}
			_, err := batchEnv.store.Reserve(ctx, session.BatchReservation{
				Owner: child, Host: "host-await", RunnerID: "runner-await", ToolCallID: "child-work", InputHash: "child-hash", Sequence: 1,
				Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none", ToolCallID: nullString("child-work"), InputHash: nullString("child-hash")}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("bash"), State: "running", ControlState: "none"}}}},
			})
			require.NoError(t, err)
			coordinatorBatchBindings.Store(c, &batchCoordinatorBinding{live: batchEnv.live})
			t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
			cutoverBindFacts(t, c, batchEnv)
			workerSnapshot := false
			hook := new(func(string, string))
			*hook = func(string, string) { workerSnapshot = true; cancel() }
			awaitTasksWorkerSnapshotHook.Store(hook)
			t.Cleanup(func() { awaitTasksWorkerSnapshotHook.Store(nil) })
			res, err := c.AwaitTasks(ctx, child, "any", 0)
			if fork {
				require.NoError(t, err)
				require.False(t, res.Blocked)
				require.Equal(t, []string{"child-work"}, awaitTaskCallIDs(res.WaitingFor))
				require.False(t, workerSnapshot)
			} else {
				require.ErrorIs(t, err, context.Canceled)
				require.True(t, workerSnapshot, "durable child must enter worker snapshot path")
			}
		})
	}
}

func awaitTaskCallIDs(refs []tools.AwaitTaskRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ToolCallID)
	}
	return ids
}
