// H4 W2: the flag-ON batch reset path must run its owner-scope dead-host
// recovery BEFORE the R10c atomic refusal, so a dead host's running root is
// recovered to interrupted and the reset proceeds instead of refusing.
package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedBatchResetRoot reserves and announces a live running batch root + one
// task child for owner on host (the B1 Reserve + BatchAnnounceStarted shape).
func seedBatchResetRoot(t *testing.T, conn *sql.DB, owner, callID, host string) string {
	t.Helper()
	ctx := context.Background()
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "reset-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root := res.Snapshot.Root.RootID
	start, err := store.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, root,
		message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "started"}}, BackgroundJobNotice: true})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root
}

// TestBatchResetSessionHistory_RecoversDeadHostBeforeRefusal pins the H4 W2
// recovery block inside batchResetSessionHistory: the dead host's running
// root must be recovered BEFORE the reset's atomic refusal, so the reset
// wipes the owner instead of refusing.
func TestBatchResetSessionHistory_RecoversDeadHostBeforeRefusal(t *testing.T) {
	if !config.BatchRuntimeEnabled() {
		t.Skip("the batch reset path is flag-ON only; the OFF twin is the legacy reset suite")
	}
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "reset-batch-rec", "rec")
	require.NoError(t, err)

	lk, err := session.TryAcquireFileLock(session.HostLockPath(a.DataDirectory(), "reset-dead-host"))
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	root := seedBatchResetRoot(t, a.DB(), sess.ID, "reset-batch-rec-call", "reset-dead-host")

	var state string
	require.NoError(t, a.DB().QueryRow(`SELECT state FROM batch_nodes WHERE root_id = ? AND kind = 'batch'`, root).Scan(&state))
	require.Equal(t, "running", state, "precondition: the root is a nonterminal candidate")

	_, err = batchResetSessionHistory(ctx, a, sess.ID)
	require.NoError(t, err, "the dead host's running root must be recovered to interrupted BEFORE the reset's atomic refusal, so the reset proceeds")

	var nodes int
	require.NoError(t, a.DB().QueryRow(`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?`, sess.ID).Scan(&nodes))
	require.Zero(t, nodes, "the reset wiped the now-terminal root")

	left, err := a.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, left, "the reset wiped the owner's history")
}
