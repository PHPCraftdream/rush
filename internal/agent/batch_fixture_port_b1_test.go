// B1 slot helpers (slot B1 of the fixture-porting split): B1-prefixed
// batch-only fixtures shared by the B1-owned test files
// (coordinator_bgshell_policy_test.go, coordinator_cli_scope_test.go,
// coordinator_scope_recovery_test.go, cliscope_completion_hold_test.go).
// Everything here seeds and reads through the batch store only.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// b1BGShellFixture builds a batch-only bg-shell fixture: a coordinator with a
// background manager and node facts bound over a fresh one-migration store.
// The bg-shell row store of the deleted legacy fixture is gone; callers that
// need notice rows use the coordinator's notice store
// (session.NewSessionNoticeStore(conn) over the returned conn).
func b1BGShellFixture(t *testing.T) (*coordinator, *sql.DB, *shell.BackgroundShellManager, string) {
	t.Helper()
	conn, dataDir := batchFixtureConn(t)
	// Callers start shells owned by the literal id "session"; its bg_shell_done
	// notices FK-anchor on sessions (id).
	ensureFixtureSession(t, conn, "session")
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(context.Background()) })
	coord := newBatchRuntimeCoordinator()
	coord.noticeStore = session.NewSessionNoticeStore(conn)
	coord.background = mgr
	bindBatchNodeFacts(t, coord, conn, dataDir)
	return coord, conn, mgr, t.TempDir()
}

// b1SeedCompletedJob seeds the owner's pending-inclusive wake=1 reaction
// debt: a command task's completion debt for childID=="", a terminal
// delegation child link otherwise.
func b1SeedCompletedJob(t *testing.T, conn *sql.DB, owner, callID string, kind string, childID string) {
	t.Helper()
	if childID != "" {
		seedFixtureDelegation(t, conn, owner, callID, childID, kind, "completed", false)
		return
	}
	seedFixtureJobDebt(t, conn, owner, callID, false)
}

// b1SeedRunningJob seeds a live RUNNING batch root (state running, announced)
// for owner: the owner has running work but no debt.
func b1SeedRunningJob(t *testing.T, conn *sql.DB, owner, callID string) {
	t.Helper()
	seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
}

// b1SeedDelegationChildLink seeds the durable delegation child-link naming
// the child: the live batch readers (drainPermitted -> isDurableDelegationChild
// -> facts.DelegationChildEver, delegatedChildDriven, CLIScope's live reads)
// answer from batch nodes only. The former in-process park/deliver half of
// the deleted workLedger seed is gone with the ledger.
func b1SeedDelegationChildLink(t *testing.T, conn *sql.DB, owner, callID, childID string) {
	t.Helper()
	seedFixtureDelegation(t, conn, owner, callID, childID, "agent", "running", false)
}

// b1SeedBatchRootOnHost reserves and announces a live (state-parameterised)
// batch root on an EXPLICIT host id -- the seedBatchDelegation twin for host
// identity, which hardcodes "fixture-host". Used by the dead-host scope
// recovery fixture to put running batch work exactly on the probed dead host.
func b1SeedBatchRootOnHost(t *testing.T, conn *sql.DB, owner, callID, host, state string) (root, child string) {
	t.Helper()
	ctx := context.Background()
	ensureFixtureSession(t, conn, owner)
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: state, ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: state, ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, child = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, root, batchFixtureMessage("fixture started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, child
}

// b1VisibleReactionDebtExists reads the VISIBLE (delivery='done')
// reaction-debt predicate -- what a Drain's turn-start decision uses and what
// TransferOnly-style oracles assert stays behind for the driving loop -- over
// the batch nodes on conn.
func b1VisibleReactionDebtExists(ctx context.Context, t *testing.T, conn *sql.DB, owner string) bool {
	t.Helper()
	visible, err := session.NewBatchStore(conn).BatchReactionVisibleDebtExists(ctx, owner)
	require.NoError(t, err)
	return visible
}
