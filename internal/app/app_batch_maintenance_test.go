// H4 W2: the App-owned batch dead-host maintenance pass. The startup pass
// recovers exactly once (wake=0); periodic passes carry the keyset cursor
// so budget-exhausted passes converge; stop joins and is idempotent.
package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newBatchMaintenanceApp builds a bare App literal over a temp data dir and
// a raw writer pool, with REAL session and message services (exact precedent:
// newRecoveryTestApp in internal/app/recovery_test.go) — batch_nodes.owner
// _session_id has a hard FK to sessions(id) (migrations/20261012000001_batch
// _projection.sql:7) with foreign_keys=ON (internal/db/connect.go:15-16), so
// Reserve needs a real sessions row for the owner.
func newBatchMaintenanceApp(t *testing.T) (*App, *sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	return &App{
		DB:       func() *sql.DB { return conn },
		dbConns:  []*sql.DB{conn},
		dataDir:  dataDir,
		Sessions: session.NewService(q, conn),
		Messages: message.NewService(q),
	}, conn, dataDir
}

// ensureBatchMaintenanceOwner idempotently creates the owner session: the
// cursor test seeds THREE roots for the SAME owner, so the second and third
// seeds must hit the existing row, not fail a UNIQUE constraint.
func ensureBatchMaintenanceOwner(t *testing.T, a *App, ctx context.Context, ownerID string) {
	t.Helper()
	if _, err := a.Sessions.Get(ctx, ownerID); err == nil {
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, err)
	}
	_, err := a.Sessions.CreateWithID(ctx, ownerID, "title")
	require.NoError(t, err)
}

// seedBatchMaintenanceRoot reserves and announces a live (state-parameterised)
// batch root + one task child on host, mirroring the B1 fixture's Reserve +
// BatchAnnounceStarted shape. The Reservation Owner is the real session ID.
func seedBatchMaintenanceRoot(t *testing.T, a *App, conn *sql.DB, owner, callID, host, state string) string {
	t.Helper()
	ctx := context.Background()
	ensureBatchMaintenanceOwner(t, a, ctx, owner)
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "maint-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: state, ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: state, ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root := res.Snapshot.Root.RootID
	start, err := store.BatchAnnounceStarted(ctx, a.Messages, owner, root,
		message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "started"}}, BackgroundJobNotice: true})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root
}

// batchMaintenanceRootState reads the root node's (state, outcome_code).
func batchMaintenanceRootState(t *testing.T, conn *sql.DB, rootID string) (string, string) {
	t.Helper()
	var state, outcome string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT state, outcome_code FROM batch_nodes WHERE root_id = ? AND kind = 'batch'`, rootID).Scan(&state, &outcome))
	return state, outcome
}

// batchMaintenanceNoticeStats reads (count, max wake) over the root's
// selected recovery notices.
func batchMaintenanceNoticeStats(t *testing.T, conn *sql.DB, rootID string) (int, int) {
	t.Helper()
	var count, wake int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*), coalesce(max(wake), 0) FROM batch_nodes WHERE root_id = ? AND notice_selected = 1`, rootID).Scan(&count, &wake))
	return count, wake
}

// TestBatchMaintenance_StartupPassRecoversOnceWakeZero pins startBatchMaintenance's
// synchronous startup pass: one recovery, one selected notice, wake=0 — and
// no loop at all without a data dir or DB writer.
func TestBatchMaintenance_StartupPassRecoversOnceWakeZero(t *testing.T) {
	a, conn, dataDir := newBatchMaintenanceApp(t)
	root := seedBatchMaintenanceRoot(t, a, conn, "sess-maint", "call-maint", "maint-dead-host", "running")
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "maint-dead-host"))
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	a.startBatchMaintenance()
	t.Cleanup(a.stopBatchMaintenance)
	_, ok := batchMaintenanceLoops.Load(a)
	require.True(t, ok, "flag on: the maintenance loop must be armed")

	state, outcome := batchMaintenanceRootState(t, conn, root)
	require.Equal(t, "interrupted", state)
	require.Equal(t, "host_lost", outcome)
	count, wake := batchMaintenanceNoticeStats(t, conn, root)
	require.Equal(t, 1, count, "exactly one selected recovery notice")
	require.Equal(t, 0, wake, "recovery never wakes a paid turn")
}

// TestBatchMaintenance_PeriodicPassAdvancesCursorAndConverges pins the
// cursor rotation: a budget-exhausted pass parks its keyset cursor, later
// passes converge to every root, and a fully scanned pass resets it.
func TestBatchMaintenance_PeriodicPassAdvancesCursorAndConverges(t *testing.T) {
	oldRoots := batchMaintenanceMaxRoots
	batchMaintenanceMaxRoots = 1
	t.Cleanup(func() { batchMaintenanceMaxRoots = oldRoots })
	oldPage := batchMaintenancePageSize
	batchMaintenancePageSize = 1
	t.Cleanup(func() { batchMaintenancePageSize = oldPage })

	a, conn, dataDir := newBatchMaintenanceApp(t)
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "maint-cursor-dead"))
	require.NoError(t, err)
	require.NoError(t, lk.Release())
	var roots []string
	for i := 0; i < 3; i++ {
		roots = append(roots, seedBatchMaintenanceRoot(t, a, conn, "sess-cursor", "call-cursor-"+string(rune('a'+i)), "maint-cursor-dead", "running"))
	}

	a.startBatchMaintenance()
	t.Cleanup(a.stopBatchMaintenance)
	require.Equal(t, 1, batchMaintenanceInterruptedTotal(t, conn, roots), "the startup pass is pass #1: exactly one root recovered")

	v, ok := batchMaintenanceLoops.Load(a)
	require.True(t, ok)
	loop := v.(*batchMaintenanceLoop)
	loop.mu.Lock()
	require.NotEqual(t, session.BatchRecoveryCursorStart, loop.cursor, "a budget-exhausted pass must park its keyset cursor on the last processed root")
	loop.mu.Unlock()

	a.batchMaintenancePass()
	require.Equal(t, 2, batchMaintenanceInterruptedTotal(t, conn, roots))
	a.batchMaintenancePass()
	require.Equal(t, 3, batchMaintenanceInterruptedTotal(t, conn, roots))
	a.batchMaintenancePass()
	require.Equal(t, 3, batchMaintenanceInterruptedTotal(t, conn, roots), "a fully converged pass must not duplicate recoveries")
	count, _ := batchMaintenanceTotalNotices(t, conn, roots)
	require.Equal(t, 3, count, "no duplicate recovery notices")

	loop.mu.Lock()
	require.Equal(t, session.BatchRecoveryCursorStart, loop.cursor, "a fully scanned pass resets the cursor")
	loop.mu.Unlock()
}

// batchMaintenanceInterruptedTotal counts interrupted root rows among roots.
func batchMaintenanceInterruptedTotal(t *testing.T, conn *sql.DB, roots []string) int {
	t.Helper()
	count, _ := batchMaintenanceScan(t, conn, roots, `state = 'interrupted'`)
	return count
}

// batchMaintenanceTotalNotices counts selected notice rows among roots.
func batchMaintenanceTotalNotices(t *testing.T, conn *sql.DB, roots []string) (int, int) {
	t.Helper()
	return batchMaintenanceScan(t, conn, roots, `notice_selected = 1`)
}

func batchMaintenanceScan(t *testing.T, conn *sql.DB, roots []string, predicate string) (int, int) {
	t.Helper()
	args := make([]any, 0, len(roots))
	placeholders := ""
	for i, id := range roots {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	var count, wake int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*), coalesce(max(wake), 0) FROM batch_nodes WHERE kind = 'batch' AND `+predicate+` AND root_id IN (`+placeholders+`)`, args...).Scan(&count, &wake))
	return count, wake
}

// TestBatchMaintenance_StopJoinsAndIsIdempotent pins stopBatchMaintenance:
// it disarms the loop, is idempotent and nil-safe, and a restart arms a
// fresh loop.
func TestBatchMaintenance_StopJoinsAndIsIdempotent(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)

	a.startBatchMaintenance()
	_, ok := batchMaintenanceLoops.Load(a)
	require.True(t, ok)

	a.stopBatchMaintenance()
	_, ok = batchMaintenanceLoops.Load(a)
	require.False(t, ok, "a stop that forgets LoadAndDelete leaves the loop armed past shutdown")

	a.stopBatchMaintenance() // idempotent: must not panic

	a.startBatchMaintenance()
	_, ok = batchMaintenanceLoops.Load(a)
	require.True(t, ok, "a restart must arm a fresh loop")
	a.stopBatchMaintenance()
}

// TestBatchMaintenance_InstallArmsAndShutdownDisarms pins BOTH wiring call
// sites at composition level: installBatchRuntime arms the loop (first
// assertion against the startBatchMaintenance call site,
// app_batch_install.go:146) and the REAL production shutdown entry
// (ShutdownWithResult -> releaseResources -> stopBatchMaintenance,
// app_lifecycle.go, BEFORE the batchRuntime drain gate) disarms it.
// SEQUENTIAL: the sync.Map and the App's shutdown-once are process-global.
//
// Why the bare fixture CAN drive the production install entry: with the
// flag ON, newBatchRuntimeFactory's only hard preconditions are a store
// (batchHostStore() lazily builds one from dataDir+writer,
// app_batch_cutover_hosts.go:34), a message service, and a DB writer
// (batch_install_factory.go:33-46); everything else (profile, registries)
// gets closed defaults (:60-72), and InstallBatchRuntimeDeferred builds a
// DORMANT install (withRunner=false, batch_install_adapters.go:656-662) —
// no provider/config service is needed until a Runner is actually built.
// The fixture's REAL Sessions/Messages services (P1-4) complete the picture.
func TestBatchMaintenance_InstallArmsAndShutdownDisarms(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	ctx := context.Background()

	require.NoError(t, a.installBatchRuntime(ctx), "the production install entry must succeed on the bare fixture App")
	_, ok := batchMaintenanceLoops.Load(a)
	require.True(t, ok, "installBatchRuntime must arm the maintenance loop (app_batch_install.go:146)")

	res := a.ShutdownWithResult()
	require.False(t, res.Forced, "graceful shutdown of an idle installed App must not be forced")
	_, ok = batchMaintenanceLoops.Load(a)
	require.False(t, ok, "releaseResources must disarm the maintenance loop before the drain gate releases the writer (app_lifecycle.go stopBatchMaintenance call site)")
}
