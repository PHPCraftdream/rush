// Batch-side timeout-wake parity tests. The legacy workLedger timeout
// machinery (work_ledger_timeout.go) has four observable behaviors; the jobbatch
// engine (internal/jobbatch/engine.go:88-89, EventDeadlineExpired ->
// CauseTimeout) drives the runner-side Project, and the coordinator only
// consumes the committed result through the batch outbox pull. These tests
// prove the batch-path parity for the behaviors that exist:
//
//   - terminate_and_wake parity: a projected runner timeout (state
//     timed_out, cause timeout) commits a selected notice that wakes the
//     owner and is delivered exactly once through the production formatter
//     (buildBatchNoticeMessageParams -> FormatAsyncCompletion).
//   - the wake_only check-in parity: the App-owned BatchTimeoutCheckin
//     scanner (internal/session/batch_timeout_checkin.go) commits ONE
//     retained check-in notice per overdue wake_only payload, wakes the
//     owner, dedupes across scans/restarts via a durable receipt, and
//     never touches the running node or the completion-notice path.
//     Terminate-and-wake intents ride TaskSpec.Timeout instead and are
//     never scanned.
//
// Every test binds the batch store directly (none of these tests read or
// set any environment variable).
package agent

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"charm.land/fantasy"
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

// TestBatchTimeoutWakeParity_WakeOnlyCheckinFiresOnceAndCompletionStillSelectsOnce
// is the REAL wake_only oracle (replacing the vacuous running-node test):
// the App-owned BatchTimeoutCheckin scanner commits exactly ONE retained
// check-in notice for an overdue wake_only payload, wakes the owner after
// the commit, survives re-scans and a service restart without duplicating,
// never touches the node's running state, and leaves the completion-notice
// selection path fully intact.
func TestBatchTimeoutWakeParity_WakeOnlyCheckinFiresOnceAndCompletionStillSelectsOnce(t *testing.T) {
	t.Parallel()
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "batch-timeout-wakeonly-owner")
	require.NoError(t, err)
	const callID = "call-batch-timeout-wakeonly"
	// The child's payload is built through the PRODUCTION direct-tool
	// normalization, so the fixture cannot drift from what ingress writes.
	spec, err := batchOneItemSpec(BatchInvocation{SessionID: owner.ID},
		fantasy.ToolCall{ID: callID, Name: "bash", Input: string(mustCanonical(t, `{"command":"sleep 60","timeout":5}`))},
		mustCanonical(t, `{"command":"sleep 60","timeout":5}`))
	require.NoError(t, err)
	require.NotNil(t, spec.Task)
	require.Zero(t, spec.Task.Timeout, "wake_only never sets the termination deadline")

	store := session.NewBatchStore(env.conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner.ID, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
			TaskPayload: sql.NullString{String: string(spec.Task.Payload), Valid: true},
		}}}},
	})
	require.NoError(t, err)
	root, child := res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, env.messages, owner.ID, root, batchFixtureMessage("fixture wakeonly started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)

	// The scanner is the check-in authority: a fake clock advances past the
	// node's created_at+5s due (no sleeps).
	var wakes []string
	service := session.NewBatchTimeoutCheckin(env.conn)
	service.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	service.SetWakeFunc(func(_ context.Context, ownerSessionID string) { wakes = append(wakes, ownerSessionID) })
	n, err := service.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the overdue wake_only node is checked in exactly once")

	var count int
	var kind string
	var wake int
	var delivery, jobToolCallID, noticeOwner string
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		"SELECT count(*), kind, wake, delivery, job_tool_call_id, owner FROM session_notices WHERE kind='timeout_wake_only'").
		Scan(&count, &kind, &wake, &delivery, &jobToolCallID, &noticeOwner))
	require.Equal(t, 1, count, "exactly one retained check-in notice exists")
	require.Equal(t, 1, wake)
	require.Equal(t, "pending", delivery)
	require.Equal(t, callID, jobToolCallID, "the notice void-pairs with the root's tool_call_id")
	require.Equal(t, owner.ID, noticeOwner)
	var receipts int
	require.NoError(t, env.conn.QueryRowContext(t.Context(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Equal(t, 1, receipts, "exactly one one-shot receipt exists")

	// The scanner never stops work: node stays running, root stays live.
	node := batchTimeoutWakeParityChild(t, env.conn, owner.ID, root, child)
	require.Equal(t, "running", node.State)
	snap, err := store.Read(ctx, owner.ID, root)
	require.NoError(t, err)
	require.Equal(t, "running", snap.Nodes[1].State, "the root snapshot still resolves with the child running")
	require.Equal(t, []string{owner.ID}, wakes, "the wake hook fired once, after the commit")

	// No duplicate on re-scan, and none after a full service restart.
	n, err = service.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	restarted := session.NewBatchTimeoutCheckin(env.conn)
	restarted.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err = restarted.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "the durable receipt survives a service restart")
	require.NoError(t, env.conn.QueryRowContext(t.Context(), "SELECT count(*) FROM session_notices WHERE kind='timeout_wake_only'").Scan(&count))
	require.Equal(t, 1, count)

	// The natural completion still selects and delivers its notice exactly
	// once: the check-in machinery did not corrupt the completion path.
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner.ID, Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "fixture done", ControlState: "none"},
				{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: "fixture done", ControlState: "none"},
			},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID:     child,
				NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
				NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
			}},
		}},
	}))
	pulled, err := store.BatchPullNotices(ctx, env.messages, owner.ID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the completion notice is delivered on the first pull")
	require.Equal(t, child, pulled[0].NodeID)
	require.True(t, pulled[0].Wake, "the completion notice still carries the wake")
	again, err := store.BatchPullNotices(ctx, env.messages, owner.ID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, again, 0, "the completion notice is selected exactly once")
}

// TestBatchTimeoutWakeParity_TerminateIntentLatchesAndNeverChecksIn catches
// the mutation where the wake_only scanner acts on a termination intent: a
// terminate_and_wake input latches TaskSpec.Timeout, carries NO payload
// timeout, and produces ZERO check-ins even when scanned far past due.
func TestBatchTimeoutWakeParity_TerminateIntentLatchesAndNeverChecksIn(t *testing.T) {
	t.Parallel()
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "batch-timeout-term-owner")
	require.NoError(t, err)
	const callID = "call-batch-timeout-term"
	input := mustCanonical(t, `{"command":"sleep 60","timeout":{"seconds":30,"kind":"terminate_and_wake"}}`)
	spec, err := batchOneItemSpec(BatchInvocation{SessionID: owner.ID},
		fantasy.ToolCall{ID: callID, Name: "bash", Input: string(input)}, input)
	require.NoError(t, err)
	require.NotNil(t, spec.Task)
	require.Greater(t, spec.Task.Timeout, time.Duration(0), "the terminate intent latches the termination deadline")
	decoded, err := decodeLeafPayload(spec)
	require.NoError(t, err)
	require.Nil(t, decoded.Timeout, "a terminate intent never rides the durable payload")

	// Seed a node whose payload is that terminate JSON: the scanner must
	// ignore it structurally (no timeout field), not just by due time.
	store := session.NewBatchStore(env.conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner.ID, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
			TaskPayload: sql.NullString{String: string(spec.Task.Payload), Valid: true},
		}}}},
	})
	require.NoError(t, err)
	root := res.Snapshot.Root.RootID
	service := session.NewBatchTimeoutCheckin(env.conn)
	service.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err := service.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "the wake_only scanner never acts on a termination intent")
	var notices int
	require.NoError(t, env.conn.QueryRowContext(t.Context(), "SELECT count(*) FROM session_notices").Scan(&notices))
	require.Zero(t, notices)
	snap, err := store.Read(ctx, owner.ID, root)
	require.NoError(t, err)
	require.Equal(t, "running", snap.Nodes[1].State)
}
