// Batch-side timeout-wake parity tests. The legacy workLedger timeout
// machinery (work_ledger_timeout.go) has four observable behaviors; the
// RUSH_BATCH_RUNTIME path enforces timeouts differently -- the jobbatch
// engine (internal/jobbatch/engine.go:88-89, EventDeadlineExpired ->
// CauseTimeout) drives the runner-side Project, and the coordinator only
// consumes the committed result through the batch outbox pull. These tests
// prove the batch-path parity for the behaviors that exist:
//
//   - terminate_and_wake parity: a projected runner timeout (state
//     timed_out, cause timeout) commits a selected notice that wakes the
//     owner and is delivered exactly once through the production formatter
//     (buildBatchNoticeMessageParams -> FormatAsyncCompletion).
//   - the wake_only check-in has NO batch equivalent: a still-running batch
//     node past its deadline produces no notice and no wake until the
//     runner projects the terminal timeout. This is a deliberate, asserted
//     finding, not a fixture artifact.
//
// Every test binds the batch store directly, which works identically with
// RUSH_BATCH_RUNTIME unset and =1: the batch tables exist in both modes and
// none of these tests read or set the flag.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchTimeoutWakeParitySeed reserves and announces a one-child batch job
// (the same Reserve/Announce shape as seedBatchJobDebt), then projects the
// runner's timeout terminal: both root and child land in state timed_out
// with cause timeout, and the child gets a selected timeout notice
// (notice_cause timeout, notice_timed_out, wake=1) left delivery=pending.
// Returns (rootID, childNodeID).
func batchTimeoutWakeParitySeed(t *testing.T, conn *sql.DB, messages message.Service, owner, callID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, child := res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner, Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "timed_out", Cause: "timeout", OutcomeState: "timed_out", OutcomeSummary: "fixture timeout", ControlState: "none"},
				{NodeID: child, State: "timed_out", Cause: "timeout", OutcomeState: "timed_out", OutcomeSummary: "fixture timeout", ControlState: "none"},
			},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID:     child,
				NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "timed_out", Valid: true}, NoticeCause: sql.NullString{String: "timeout", Valid: true},
				NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeTimedOut: 1, Wake: 1,
			}},
		}},
	}))
	start, err := store.BatchAnnounceStarted(ctx, messages, owner, root, batchFixtureMessage("fixture timeout started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, child
}

// batchTimeoutWakeParityChild reads one node out of a batch snapshot by id.
func batchTimeoutWakeParityChild(t *testing.T, conn *sql.DB, owner, rootID, nodeID string) db.BatchNode {
	t.Helper()
	snap, err := session.NewBatchStore(conn).Read(context.Background(), owner, rootID)
	require.NoError(t, err)
	for _, n := range snap.Nodes {
		if n.NodeID == nodeID {
			return n
		}
	}
	t.Fatalf("batch node %s not found in root %s", nodeID, rootID)
	return db.BatchNode{}
}

// TestBatchTimeoutWakeParity_TerminatedNodeCommitsTimeoutNoticeWake is the
// batch-path parity for the legacy terminate_and_wake behavior: the timeout
// terminal (state timed_out, cause timeout) is durable BEFORE the wake, the
// committed notice arms wake=1 for the owner, the pull delivers it exactly
// once through the ONE shared formatter with the timeout wording, and a
// second pull is empty (a durable wake never re-fires).
func TestBatchTimeoutWakeParity_TerminatedNodeCommitsTimeoutNoticeWake(t *testing.T) {
	t.Parallel()
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "batch-timeout-wake-owner")
	require.NoError(t, err)
	const callID = "call-batch-timeout-wake"
	root, child := batchTimeoutWakeParitySeed(t, env.conn, env.messages, owner.ID, callID)

	// The committed terminal facts: timeout cause and selected wake armed
	// on the node row itself, before any delivery.
	node := batchTimeoutWakeParityChild(t, env.conn, owner.ID, root, child)
	require.Equal(t, "timed_out", node.State, "the projected runner timeout is terminal in the batch store")
	require.Equal(t, "timeout", node.Cause, "the terminal cause is recorded as timeout")
	require.EqualValues(t, 1, node.NoticeSelected, "the timeout notice is selected on the child node")
	require.Equal(t, "timeout", node.NoticeCause.String, "the notice cause is timeout")
	require.EqualValues(t, 1, node.Wake, "the committed row wakes the owner, like the legacy timed_out transition's wake=1")

	// Pull through the production agent formatter: the batch pull path
	// reuses FormatAsyncCompletion via buildBatchNoticeMessageParams, keyed
	// off row.State == "timed_out", exactly like the legacy job-notice pull.
	store := session.NewBatchStore(env.conn)
	pulled, err := store.BatchPullNotices(ctx, env.messages, owner.ID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the timeout notice is delivered on the first pull")
	require.Equal(t, root, pulled[0].RootID)
	require.Equal(t, child, pulled[0].NodeID)
	require.True(t, pulled[0].Wake, "the delivered notice carries the wake to the owner (legacy: notifyAsyncCompletion's Drain wake)")
	expected := FormatAsyncCompletion(AsyncCompletion{ToolCallID: callID, Content: "fixture timeout", TimedOut: true})
	require.Equal(t, expected, pulled[0].Message.FullText(), "the batch timeout delivery uses the same formatter/wording as the legacy timed_out completion")
	require.NotContains(t, pulled[0].Message.FullText(), "still running", "a TERMINAL timeout notice never uses the wake_only check-in wording")

	// Delivered exactly once: the wake is consumed with the notice.
	again, err := store.BatchPullNotices(ctx, env.messages, owner.ID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, again, 0, "the second pull is empty: a timeout wake is durable and consumed once")
	node = batchTimeoutWakeParityChild(t, env.conn, owner.ID, root, child)
	require.Equal(t, "done", node.Delivery, "the pulled notice is delivery=done")
}

// TestBatchTimeoutWakeParity_RunningNodeHasNoTimeoutWake asserts the parity
// gap for the legacy wake_only check-in (work_ledger_timeout.go's
// timeoutWakeOnly branch): a batch node that is still running past its
// deadline owns NO notice and NO wake -- the batch store has no
// session_notices "timeout_wake_only" equivalent and no timeoutNotified
// one-shot guard, because the timeout authority (the jobbatch engine's
// EventDeadlineExpired) always CANCELS the node; only the terminal timeout
// projection ever produces a notice. The check-in behavior is runner-side
// and out of the coordinator's reach.
func TestBatchTimeoutWakeParity_RunningNodeHasNoTimeoutWake(t *testing.T) {
	t.Parallel()
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "batch-timeout-running-owner")
	require.NoError(t, err)
	const callID = "call-batch-timeout-running"
	store := session.NewBatchStore(env.conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner.ID, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, child := res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, env.messages, owner.ID, root, batchFixtureMessage("fixture running started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)

	// Still running, past any deadline: nothing pullable, nothing armed.
	pulled, err := store.BatchPullNotices(ctx, env.messages, owner.ID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 0, "a still-running batch node past its deadline produces no check-in notice or wake")
	node := batchTimeoutWakeParityChild(t, env.conn, owner.ID, root, child)
	require.Equal(t, "running", node.State, "the node stays running until the runner projects the timeout terminal")
	require.EqualValues(t, 0, node.NoticeSelected, "no notice is selected while the node is running")
	require.EqualValues(t, 0, node.Wake, "no wake is armed while the node is running")
}
