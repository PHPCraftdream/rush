package session

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func r9Stores(t *testing.T) (*BatchStore, *BatchStore, *sql.DB, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	a, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(a)) })
	require.NoError(t, seedSession(ctx, db.New(a), "owner"))
	b, err := sql.Open("sqlite", filepath.ToSlash(storepath.DataDirPath(dir, storepath.ActiveFilename))+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	b.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return NewBatchStore(a), NewBatchStore(b), a, dir, ctx
}

func r9Announce(t *testing.T, conn *sql.DB, ctx context.Context, root string) {
	t.Helper()
	_, err := conn.ExecContext(ctx, "UPDATE batch_nodes SET announced=1,announce_message_id='ack' WHERE node_id=?", root)
	require.NoError(t, err)
}

func TestR9RecoveryConcurrentAllStates(t *testing.T) {
	a, b, conn, dir, ctx := r9Stores(t)
	p := batchStoreTestReservation()
	p.Root.Children = nil
	for _, state := range []string{"pending", "starting", "running", "cancelling"} {
		task := batchStoreTestTask()
		task.Values.State = state
		task.Values.Cause = "stop"
		p.Root.Children = append(p.Root.Children, task)
	}
	reservation, err := a.Reserve(ctx, p)
	require.NoError(t, err)
	snap := reservation.Snapshot
	r9Announce(t, conn, ctx, snap.Root.RootID)
	c, err := a.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	lock, err := TryAcquireFileLock(HostLockPath(dir, "host"))
	require.NoError(t, err)
	require.NoError(t, lock.Release())
	const n = 8
	outcomes := make([]BatchRecoveryOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := a
			if i%2 == 1 {
				store = b
			}
			outcomes[i], errs[i] = store.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
		}()
	}
	close(start)
	wg.Wait()
	var total BatchRecoveryOutcome
	for i, o := range outcomes {
		require.NoError(t, errs[i])
		total.Interrupted += o.Interrupted
		total.Notices += o.Notices
		total.Controls += o.Controls
	}
	require.EqualValues(t, 5, total.Interrupted)
	require.EqualValues(t, 1, total.Notices)
	require.EqualValues(t, 1, total.Controls)
	got, err := a.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	for _, node := range got.Nodes {
		require.Equal(t, "interrupted", node.State)
		require.Equal(t, "host_lost", node.OutcomeCode)
		require.Zero(t, node.Wake)
	}
	for _, node := range got.Nodes[1:] {
		require.Zero(t, node.NoticeSelected)
		require.Equal(t, "stop", node.Cause)
	}
	require.Equal(t, "pending", got.Root.Delivery)
	answered, err := a.BatchReadControl(ctx, BatchControlTarget{"owner", "host", c.RootID, c.NodeID}, c.RequestID)
	require.NoError(t, err)
	require.Equal(t, "host_lost", answered.AnswerCode)
	require.Positive(t, answered.AnswerSequence.Int64)
	out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out)
	messages := message.NewService(db.New(conn))
	build := func(row BatchNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.OutcomeCode}}}
	}
	pulled, err := a.BatchPullNotices(ctx, messages, "owner", build)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.False(t, pulled[0].Wake)
	pulled, err = b.BatchPullNotices(ctx, messages, "owner", build)
	require.NoError(t, err)
	require.Empty(t, pulled)
}

func TestR9RecoveryUnknownAliveOwnNoChanges(t *testing.T) {
	for _, status := range []string{"unknown", "alive", "own"} {
		t.Run(status, func(t *testing.T) {
			a, _, conn, dir, ctx := r9Stores(t)
			snap := batchStoreTestReserve(t, a, ctx)
			c, err := a.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
			require.NoError(t, err)
			switch status {
			case "unknown":
				require.NoError(t, os.MkdirAll(HostLockPath(dir, "host"), 0o755))
			case "alive":
				lock, e := TryAcquireFileLock(HostLockPath(dir, "host"))
				require.NoError(t, e)
				defer lock.Release()
			case "own":
				markOwnHostID("host")
				defer unmarkOwnHostID("host")
			}
			out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
			if status == "unknown" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, BatchRecoveryOutcome{}, out)
			got, e := a.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, e)
			require.Equal(t, snap, got)
			controls, e := a.PendingControls(ctx, "owner", "host", snap.Root.RootID)
			require.NoError(t, e)
			require.Equal(t, []db.BatchControlRequest{c}, controls)
			var count int
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestR9RecoveryPreservesUnannouncedCLIUntilRetention(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	// A terminal CLI root remains available to its caller until result delivery;
	// once delivered, recovery is inert and retention can remove the entire tree.
	hook := NewBatchCommitHook(a, BatchCommitIdentity{Host: "host", Runner: "runner", Owner: "owner"})
	runner, executor, runCtx := commitHookRunner(t, hook)
	batch := commitHookSubmit(t, runCtx, runner, "cli-root", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "retention"}), "batch-cli:retention")
	attempt := commitHookAccept(t, runCtx, runner, executor, nil)
	commitHookFinish(t, runCtx, runner, attempt, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookWait(t, runCtx, batch)
	root := "cli-root"
	var pending int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id=? AND notice_selected=1 AND delivery='pending'`, root).Scan(&pending))
	require.Equal(t, 1, pending)
	lock, err := TryAcquireFileLock(HostLockPath(dir, "host"))
	require.NoError(t, err)
	require.NoError(t, lock.Release())

	out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id=?`, root).Scan(&count))
	require.Equal(t, 2, count, "undelivered terminal CLI result remains available")
	require.NoError(t, a.BatchSettleCLIWakeDebt(ctx, "owner", root))
	var delivery string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT delivery FROM batch_nodes WHERE root_id=? AND notice_selected=1`, root).Scan(&delivery))
	require.Equal(t, "done", delivery)
	purged, err := a.BatchPurgeExpired(ctx, 1<<62)
	require.NoError(t, err)
	// Counts delegations like BatchCountExpired; children go by FK cascade.
	require.EqualValues(t, 1, purged, "purge reports deleted delegations")
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id=?`, root).Scan(&count))
	require.Zero(t, count)
}

func TestR9RecoveryInterruptsLiveUnannouncedCLI(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	p := batchStoreTestReservation()
	p.OriginCLI = true
	reservation, err := a.Reserve(ctx, p)
	require.NoError(t, err)
	root := reservation.Snapshot.Root.RootID
	// A live root carries no notice yet: notice_state admits only terminal states.
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET state='running' WHERE root_id=?`, root)
	require.NoError(t, err)
	lock, err := TryAcquireFileLock(HostLockPath(dir, "host"))
	require.NoError(t, err)
	require.NoError(t, lock.Release())

	out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.EqualValues(t, 3, out.Interrupted)
	got, err := a.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.Nodes[1].State)
	require.Equal(t, "host_lost", got.Nodes[1].OutcomeCode)
	require.Zero(t, got.Root.Announced)
}

func TestR9RecoveryUnannouncedTerminalCascade(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	snap := batchStoreTestReserve(t, a, ctx)
	_, err := a.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state='completed'")
	require.NoError(t, err)
	out, err := a.BatchSweepDeadHosts(ctx, "", BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.EqualValues(t, 1, out["host"].DeletedRoots)
	for _, table := range []string{"batch_nodes", "batch_control_requests", "messages"} {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}

func TestR9RecoveryPartialCommitTerminalOutboxPreserved(t *testing.T) {
	for _, terminalRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "live-root", true: "terminal-root"}[terminalRoot], func(t *testing.T) {
			a, _, conn, dir, ctx := r9Stores(t)
			snap := batchStoreTestReserve(t, a, ctx)
			r9Announce(t, conn, ctx, snap.Root.RootID)
			state := "running"
			if terminalRoot {
				state = "completed"
			}
			p := BatchProjection{Host: "host", Sequence: 7, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, state), batchStoreTestUpdate(snap.Nodes[1].NodeID, "completed")}, Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(snap.Nodes[1].NodeID)}}}}
			if terminalRoot {
				p.Roots[0].Notices = append(p.Roots[0].Notices, batchStoreTestNotice(snap.Root.NodeID))
			}
			require.NoError(t, a.Project(ctx, p))
			before, err := a.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, err)
			out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
			require.NoError(t, err)
			require.EqualValues(t, map[bool]int{false: 2, true: 1}[terminalRoot], out.Interrupted)
			after, err := a.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, err)
			require.Equal(t, before.Nodes[1], after.Nodes[1])
			require.Equal(t, "host_lost", after.Nodes[2].OutcomeCode)
			if terminalRoot {
				require.Equal(t, before.Root, after.Root)
				require.Zero(t, out.Notices)
			} else {
				require.EqualValues(t, 1, out.Notices)
			}
		})
	}
}

func TestR9RecoveryRootRollbackOtherRootSucceeds(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	bad := batchStoreTestReserve(t, a, ctx)
	r9Announce(t, conn, ctx, bad.Root.RootID)
	p := batchStoreTestReservation()
	p.ToolCallID = "second"
	r, err := a.Reserve(ctx, p)
	require.NoError(t, err)
	r9Announce(t, conn, ctx, r.Snapshot.Root.RootID)
	c, err := a.CreateControl(ctx, "owner", "host", bad.Root.RootID, bad.Nodes[1].NodeID)
	require.NoError(t, err)
	before, err := a.Read(ctx, "owner", bad.Root.RootID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `CREATE TRIGGER fail_r9 BEFORE UPDATE ON batch_control_requests WHEN NEW.request_id = '`+c.RequestID+`' BEGIN SELECT RAISE(ABORT,'root failure'); END`)
	require.NoError(t, err)
	out, err := a.BatchRecoverDeadHost(ctx, "host", BatchSharedHostProbe(dir))
	require.ErrorContains(t, err, "root failure")
	require.EqualValues(t, 3, out.Interrupted)
	require.EqualValues(t, 1, out.Notices)
	after, err := a.Read(ctx, "owner", bad.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	controls, err := a.PendingControls(ctx, "owner", "host", bad.Root.RootID)
	require.NoError(t, err)
	require.Len(t, controls, 1)
	good, err := a.Read(ctx, "owner", r.Snapshot.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "interrupted", good.Root.State)
}
