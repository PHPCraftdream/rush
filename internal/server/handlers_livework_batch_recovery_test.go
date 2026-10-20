// H4 W2 observer-side proof: a dead-host root recovered by the background
// maintenance pass (here: one direct BatchRecoverPass over the App's real
// store — the App maintenance stand-in) must surface in the flat live-work
// SNAPSHOT as the terminal interrupted incarnation, with no history message
// involved: recovery commits no message row.
//
// Observer GAP this test deliberately does NOT cover (and cannot): recovery
// commits no message row, so events.go's shouldPushLiveWork never marks the
// session dirty and handlers_livework.go's pusher never re-pushes — a
// connected web client does NOT get a push; it learns the truth only on the
// next full snapshot request / reload. This test asserts what the snapshot
// REQUEST returns; the pusher is out of scope.
package server

import (
	"context"
	"database/sql"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedLiveWorkRecoveryRoot reserves and announces a running batch root + one
// command task child owned by ownerID on host — the Reserve payload satisfies
// batch_nodes' CHECKs/FK (root rows: kind='batch', mode set, tool_call_id and
// input_hash NOT NULL; children: tool_call_id NULL), and the owner is a REAL
// session id (owner_session_id REFERENCES sessions(id), foreign_keys=ON).
func seedLiveWorkRecoveryRoot(t *testing.T, a *appPkg.App, ctx context.Context, ownerID, callID, host string) string {
	t.Helper()
	conn := a.DB()
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: ownerID, Host: host, RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "recovery-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root := res.Snapshot.Root.RootID
	start, err := store.BatchAnnounceStarted(ctx, a.Messages, ownerID, root,
		message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "started"}}, BackgroundJobNotice: true})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root
}

// TestLiveWorkAfterRecovery_ShowsInterruptedRoot: pre-recovery the snapshot
// shows the dead host's root as running (exactly what batchRootItem maps for
// a fresh running root); after ONE background recovery pass the same root id
// surfaces as the terminal interrupted incarnation — recovered via the
// production store primitive BatchRecoverPass (the App maintenance stand-in:
// this IS the pass internal/app/app_batch_maintenance.go runs each tick).
func TestLiveWorkAfterRecovery_ShowsInterruptedRoot(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := context.Background()

	sess, err := a.Sessions.Create(ctx, "live work recovery")
	require.NoError(t, err)

	host := "livework-dead-host"
	dataDir := a.Config().Options.DataDirectory
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, host))
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	seedLiveWorkRecoveryRoot(t, a, ctx, sess.ID, "livework-recovery-call", host)

	// Pre-recovery: a fresh running root maps Status=state ("running"),
	// Reason=cause ("") and no FinishedAt (batchRootItem,
	// handlers_livework_batch.go:283).
	snap, err := buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.Len(t, snap.Commands, 1, "precondition: the running root is live work")
	require.Equal(t, "livework-recovery-call", snap.Commands[0].ToolCallID)
	require.Equal(t, "running", snap.Commands[0].Status, "batchRootItem maps Status straight from node state")
	require.Equal(t, "", snap.Commands[0].Reason, "a fresh running root's cause column is empty")
	require.Zero(t, snap.Commands[0].FinishedAt, "batchRootItem sets FinishedAt only when state != running")

	// One background recovery pass (the App maintenance stand-in: the same
	// production store primitive app_batch_maintenance.go invokes).
	_, _, done, err := session.NewBatchStore(a.DB()).BatchRecoverPass(ctx, session.BatchSharedHostProbe(dataDir),
		session.BatchRecoveryLimits{MaxRoots: 64, PageSize: 64}, session.BatchRecoveryCursorStart)
	require.NoError(t, err)
	require.True(t, done, "the pass must have scanned the whole candidate set")

	// Post-recovery: the root left the live feed (BatchLiveNodesForOwners
	// selects nonterminal roots only) and the history read serves its
	// interrupted incarnation. BatchRecoveryInterrupt sets
	// state='interrupted'/outcome_code='host_lost' and PRESERVES cause, so
	// the mapping yields Status="interrupted", Reason="" — and FinishedAt.
	snap, err = buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.Len(t, snap.Commands, 1, "the recovered root must not be dropped from the snapshot")
	require.Equal(t, "livework-recovery-call", snap.Commands[0].ToolCallID)
	require.Equal(t, "interrupted", snap.Commands[0].Status, "batchRootItem maps Status straight from the recovered node's state")
	require.Equal(t, "", snap.Commands[0].Reason, "BatchRecoveryInterrupt preserves the (empty) cause column")
	require.NotZero(t, snap.Commands[0].FinishedAt, "batchRootItem sets FinishedAt when state != running")
	require.NotNil(t, snap.Complete)
	require.True(t, *snap.Complete, "a full live+history+notices read is complete")
}
