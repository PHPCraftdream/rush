// W3 check-in scanner tests: one-shot receipts, fencing, retention
// survival, wake-after-commit, and goroutine lifecycle.
package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// batchTimeoutCheckinStores opens a two-conn DB like r9Stores (the second
// raw conn lets tests read committed rows outside the store's writer).
func batchTimeoutCheckinStores(t *testing.T) (*BatchStore, *sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	a, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(a)) })
	require.NoError(t, seedSession(ctx, db.New(a), "owner"))
	b, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "rush.db"))+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	b.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return NewBatchStore(a), b, ctx
}

// batchTimeoutCheckinReserve reserves one running root with ONE running
// task child whose TaskPayload is the wake_only shape the scanner reads
// (kind + seconds; the due anchor is batch_nodes.created_at, not the
// payload).
func batchTimeoutCheckinReserve(t *testing.T, s *BatchStore, ctx context.Context, callID string) (string, string) {
	t.Helper()
	payload := `{"tool":"bash","tool_call_id":"` + callID + `","session_id":"owner","timeout":{"kind":"wake_only","seconds":5}}`
	res, err := s.Reserve(ctx, BatchReservation{
		Owner: "owner", Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
			TaskPayload: sql.NullString{String: payload, Valid: true},
		}}}},
	})
	require.NoError(t, err)
	return res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
}

// batchTimeoutCheckinReservePayload reserves one running root + one running
// task child carrying exactly payload (NULL = no timeout payload).
func batchTimeoutCheckinReservePayload(t *testing.T, s *BatchStore, ctx context.Context, callID string, payload sql.NullString) (string, string) {
	t.Helper()
	res, err := s.Reserve(ctx, BatchReservation{
		Owner: "owner", Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
			TaskPayload: payload,
		}}}},
	})
	require.NoError(t, err)
	return res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
}

func batchTimeoutCheckinNoticeCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM session_notices").Scan(&n))
	return n
}

func batchTimeoutCheckinReceiptCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM batch_timeout_checkins").Scan(&n))
	return n
}

// TestBatchTimeoutCheckinCommitsOneRetainedNotice catches the mutation
// where the scanner chooses a terminal state or omits the retained notice:
// the due check-in (created_at + seconds past a clock advanced a minute)
// commits exactly one pending wake notice, the node stays running, the
// root stays untouched, and the receipt exists.
func TestBatchTimeoutCheckinCommitsOneRetainedNotice(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)
	root, child := batchTimeoutCheckinReserve(t, s, ctx, "call-due")
	svc := NewBatchTimeoutCheckin(conn)
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, batchTimeoutCheckinNoticeCount(t, conn))
	var kind, text string
	var wake int
	var delivery, jobToolCallID, owner string
	require.NoError(t, conn.QueryRow(
		"SELECT kind, text, wake, delivery, job_tool_call_id, owner FROM session_notices").
		Scan(&kind, &text, &wake, &delivery, &jobToolCallID, &owner))
	require.Equal(t, "timeout_wake_only", kind)
	require.Equal(t, 1, wake)
	require.Equal(t, "pending", delivery)
	require.Equal(t, "call-due", jobToolCallID, "the notice pairs with the root's tool_call_id for the void condition")
	require.Equal(t, "owner", owner)
	require.Contains(t, text, root)
	require.Contains(t, text, "batch_stop")
	require.Equal(t, 1, batchTimeoutCheckinReceiptCount(t, conn))
	// The scanner never touches node state or delivery: it only reports.
	var childState, rootDelivery, rootControl string
	require.NoError(t, conn.QueryRow(
		"SELECT state FROM batch_nodes WHERE node_id=?", child).Scan(&childState))
	require.Equal(t, "running", childState)
	require.NoError(t, conn.QueryRow(
		"SELECT delivery, control_state FROM batch_nodes WHERE node_id=?", root).Scan(&rootDelivery, &rootControl))
	require.Equal(t, "none", rootDelivery)
	require.Equal(t, "none", rootControl)
}

// TestBatchTimeoutCheckinOneShotAcrossScansAndRestarts catches the mutation
// where the receipt is forgotten or dedupe is per-service memory: the FIRST
// scan checks in (returns 1), every later scan and a full service restart
// return 0, and exactly one notice and one receipt exist after every scan.
func TestBatchTimeoutCheckinOneShotAcrossScansAndRestarts(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)
	batchTimeoutCheckinReserve(t, s, ctx, "call-once")
	svc := NewBatchTimeoutCheckin(conn)
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	// Scan 1: the one-shot fires exactly once.
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, batchTimeoutCheckinNoticeCount(t, conn))
	require.Equal(t, 1, batchTimeoutCheckinReceiptCount(t, conn))
	// Scans 2 and 3: the receipt fences the re-scan.
	for i := 0; i < 2; i++ {
		n, err = svc.ScanOnce(ctx)
		require.NoError(t, err)
		require.Zero(t, n, "re-scan %d must not re-check-in", i+2)
		require.Equal(t, 1, batchTimeoutCheckinNoticeCount(t, conn))
		require.Equal(t, 1, batchTimeoutCheckinReceiptCount(t, conn))
	}
	// A "restart": a fresh service instance on the same conn, with the SAME
	// advanced clock — the task IS due, so n==0 proves the durable receipt
	// (not a not-yet-due skip) fences the restart.
	fresh := NewBatchTimeoutCheckin(conn)
	fresh.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err = fresh.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, batchTimeoutCheckinNoticeCount(t, conn))
	require.Equal(t, 1, batchTimeoutCheckinReceiptCount(t, conn))
}

// TestBatchTimeoutCheckinSkipsNotYetDueAndTerminalAndVoid catches the
// mutation where the due filter or the state/delivery fences are omitted:
// future-due, terminal, and void-root nodes produce no check-in.
func TestBatchTimeoutCheckinSkipsNotYetDueAndTerminalAndVoid(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)

	// (a) Not yet due (created_at + 5s against the default clock): skipped.
	root, _ := batchTimeoutCheckinReserve(t, s, ctx, "call-future")
	svc := NewBatchTimeoutCheckin(conn)
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, batchTimeoutCheckinNoticeCount(t, conn))

	// (b) Terminal fence: advance the clock past the node's due, THEN
	// project the same node timed_out via the production projection — the
	// due node is excluded only because it is terminal.
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	childID := snap.Nodes[1].NodeID
	require.NoError(t, s.Project(ctx, BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []BatchRootProjection{{
			Owner: "owner", Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "timed_out", Cause: "timeout", OutcomeState: "timed_out", OutcomeSummary: "fixture", ControlState: "none"},
				{NodeID: childID, State: "timed_out", Cause: "timeout", OutcomeState: "timed_out", OutcomeSummary: "fixture", ControlState: "none"},
			},
		}},
	}))
	n, err = svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// (c) The production void path only ever voids notice-selected rows (the
	// rerun reconcile), never a still-running root, so the scanner's
	// r.delivery != 'void' fence is defence-in-depth with no production
	// writer for this state. Exercise it with a RAW update that satisfies
	// every batch_nodes CHECK: select a root notice (kind/state/cause set,
	// sequence > 0, ordinal set) and void it with wake = 0, exactly as
	// BatchRerunVoidNodesOfVoidedRoots writes it. A due node on such a root
	// writes no check-in.
	rootVoid, _ := batchTimeoutCheckinReserve(t, s, ctx, "call-void")
	_, err = conn.Exec(`UPDATE batch_nodes SET notice_selected = 1,
		notice_kind = 'root', notice_state = 'cancelled', notice_cause = 'stop',
		notice_sequence = 2, notice_ordinal = 1, notice_cancelled = 1,
		delivery = 'void', wake = 0, updated_at = ?
	  WHERE node_id = ?`, time.Now().Unix(), rootVoid)
	require.NoError(t, err)
	n, err = svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, batchTimeoutCheckinNoticeCount(t, conn))
}

// TestBatchTimeoutCheckinReceiptSurvivesNoticeRetention catches the
// mutation where dedupe is keyed on the notice row instead of the receipt:
// deleting the notice must not let a re-scan re-check-in.
func TestBatchTimeoutCheckinReceiptSurvivesNoticeRetention(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)
	batchTimeoutCheckinReserve(t, s, ctx, "call-retain")
	svc := NewBatchTimeoutCheckin(conn)
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = conn.Exec("DELETE FROM session_notices")
	require.NoError(t, err)
	n, err = svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "the receipt, not the notice row, is the one-shot guard")
	require.Zero(t, batchTimeoutCheckinNoticeCount(t, conn))
}

// TestBatchTimeoutCheckinWakeHookCalledAfterCommit catches the mutation
// where the wake fires before commit (waking for a notice that may not
// exist) or never fires.
func TestBatchTimeoutCheckinWakeHookCalledAfterCommit(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)
	batchTimeoutCheckinReserve(t, s, ctx, "call-wake")
	var owners []string
	svc := NewBatchTimeoutCheckin(conn)
	svc.SetWakeFunc(func(_ context.Context, ownerSessionID string) { owners = append(owners, ownerSessionID) })
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"owner"}, owners, "the hook fires exactly once, after the commit, with the owner")
}

// TestBatchTimeoutCheckinBudgetProgressesThroughBacklog catches the
// mutation where the budget resets per page, or the candidate query lacks
// the receipt/kind/due filters (selecting every live task node): with page
// size == budget, payload-less and already-receipted nodes ahead in keyset
// order starve the last overdue task behind them forever. The candidate
// counts here do not depend on created_at/node_id ordering, because any 2
// of the 3 due candidates commit in scan 1 and the third is the only
// remaining due candidate in scan 2.
func TestBatchTimeoutCheckinBudgetProgressesThroughBacklog(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchTimeoutCheckinStores(t)
	// FIRST two tasks without payload: they must never be candidates.
	batchTimeoutCheckinReservePayload(t, s, ctx, "call-np0", sql.NullString{})
	batchTimeoutCheckinReservePayload(t, s, ctx, "call-np1", sql.NullString{})
	// Three overdue wake_only tasks.
	duePayload := func(id string) sql.NullString {
		return sql.NullString{String: `{"tool":"bash","tool_call_id":"` + id + `","session_id":"owner","timeout":{"kind":"wake_only","seconds":5}}`, Valid: true}
	}
	_, b1 := batchTimeoutCheckinReservePayload(t, s, ctx, "call-b1", duePayload("call-b1"))
	_, b2 := batchTimeoutCheckinReservePayload(t, s, ctx, "call-b2", duePayload("call-b2"))
	_, b3 := batchTimeoutCheckinReservePayload(t, s, ctx, "call-b3", duePayload("call-b3"))
	// One malformed-payload task: excluded by the json_valid guard.
	batchTimeoutCheckinReservePayload(t, s, ctx, "call-bad", sql.NullString{String: `{"tool":"bash"`, Valid: true})

	svc := NewBatchTimeoutCheckin(conn)
	svc.pageSize = 2
	svc.scanBudget = 2
	svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })

	// Scan 1: only 2 candidates are processed (the two due tasks the query
	// yields first); the third due task stays outstanding.
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, 2, batchTimeoutCheckinReceiptCount(t, conn))

	// Scan 2: the two receipted nodes dropped OUT of the candidate set in
	// SQL, so the remaining overdue candidate is the only one left.
	n, err = svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 3, batchTimeoutCheckinReceiptCount(t, conn))

	// Scan 3: backlog drained.
	n, err = svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 3, batchTimeoutCheckinReceiptCount(t, conn))

	// Exactly one receipt per due child; none for the malformed task.
	for _, nodeID := range []string{b1, b2, b3} {
		var c int
		require.NoError(t, conn.QueryRow(
			"SELECT count(*) FROM batch_timeout_checkins WHERE node_id = ?", nodeID).Scan(&c))
		require.Equal(t, 1, c, "node %s must have exactly one receipt", nodeID)
	}
	var bad int
	require.NoError(t, conn.QueryRow(
		"SELECT count(*) FROM batch_timeout_checkins WHERE node_id IN (SELECT node_id FROM batch_nodes WHERE task_payload = ?)",
		`{"tool":"bash"`).Scan(&bad))
	require.Zero(t, bad, "the malformed-payload task must never check in")
}

// TestBatchTimeoutCheckinStartStopLifecycle proves the ticker goroutine
// exits on Stop and a second Start/Stop cycle is safe. Not parallel: it is
// the only lifecycle test and runs last. No timing assertions — only that
// Stop returns.
func TestBatchTimeoutCheckinStartStopLifecycle(t *testing.T) {
	_, conn, _ := batchTimeoutCheckinStores(t)
	svc := NewBatchTimeoutCheckin(conn)
	svc.Start()
	svc.Stop()
	svc.Start()
	svc.Stop()
	require.NoError(t, conn.Ping())
}
