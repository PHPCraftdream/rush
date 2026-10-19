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
	for _, flag := range []string{"false", "true"} {
		for _, fork := range []bool{false, true} {
			name := "durable-worker"
			if fork {
				name = "ordinary-fork"
			}
			t.Run(flag+"/"+name, func(t *testing.T) {
				t.Setenv("RUSH_BATCH_RUNTIME", flag)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var c *coordinator
				var child, parent string
				if flag == "true" {
					batchEnv := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
					c = newCutoverCoordinator(t, batchEnv)
					parent = batchEnv.sessionID
					child = "await-batch-child"
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
				} else {
					env, coord, store := newAwaitWorkerEnv(t)
					c = coord
					parentRow, err := env.sessions.Create(ctx, "await identity parent")
					require.NoError(t, err)
					parent = parentRow.ID
					if fork {
						childRow, _, forkErr := env.sessions.ForkSessionTx(ctx, parent, session.ForkOptions{NewID: "await-fork-child", ParentID: parent})
						require.NoError(t, forkErr)
						child = childRow.ID
					} else {
						childRow, createErr := env.sessions.CreateTaskSession(ctx, "await-delegated-child", parent, "await child")
						require.NoError(t, createErr)
						child = childRow.ID
						_, err = store.Claim(ctx, session.ClaimParams{Owner: parent, ToolCallID: "delegation-call", Kind: session.JobKindAgent, Input: "x", ChildSessionID: child, ToolName: "agent"})
						require.NoError(t, err)
						_, err = store.Transition(ctx, session.TransitionParams{Owner: parent, ToolCallID: "delegation-call", State: "completed", Wake: true})
						require.NoError(t, err)
					}
					_, err = store.Claim(ctx, session.ClaimParams{Owner: child, ToolCallID: "child-work", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
					require.NoError(t, err)
				}
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
}

func awaitTaskCallIDs(refs []tools.AwaitTaskRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ToolCallID)
	}
	return ids
}
