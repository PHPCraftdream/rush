package cmd

// Batch delegation seed helpers for the cmd test package (modeled on
// internal/agent's seedDelegation and internal/app's f6ReserveDelegation,
// which are _test files of their own packages and cannot be imported here).
// Every seed goes through the production session.BatchStore.Reserve +
// BatchAnnounceStarted APIs, so the batch_nodes CHECKs/FKs and the live
// reader's host probe see exactly what a real delegation writes.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedCmdBatchDelegation reserves and announces a live running batch
// delegation (kind=batch root with tool call + input hash, one running task
// child linked to childID) for owner on the given host, and returns the root
// and child node ids.
func seedCmdBatchDelegation(t *testing.T, conn *sql.DB, owner, host, callID, childID string) (rootID, childNodeID string) {
	t.Helper()
	ctx := context.Background()
	bs := session.NewBatchStore(conn)
	res, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "cmd-fixture-runner", ToolCallID: callID,
		InputHash: "cmd-fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
			ChildSessionID: sql.NullString{String: childID, Valid: childID != ""},
			State:          "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	rootID, childNodeID = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := bs.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, rootID,
		message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "fixture delegation started"}},
			// A started delegation is a background-job announcement: the same
			// shape the production announce path persists.
			BackgroundJobNotice: true,
		})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return rootID, childNodeID
}

// seedCmdBatchRootFor reserves one bare pending batch root (kind=batch with
// tool call and input hash, no children -- a bare root row satisfies the
// batch_nodes CHECKs) for owner on the given host, and returns the root id.
func seedCmdBatchRootFor(t *testing.T, a *app.App, owner, host, callID string) string {
	t.Helper()
	res, err := session.NewBatchStore(a.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "cmd-fixture-runner",
		ToolCallID: callID, InputHash: "cmd-fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	return res.Snapshot.Root.RootID
}
