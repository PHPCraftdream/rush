// B1 slot helpers (slot B1 of the fixture-porting split, see
// D:/system_artefact/Temp/shadow-1019/res-split.md sec.4): B1-prefixed
// dual-mode fixtures shared by the B1-owned test files
// (coordinator_bgshell_policy_test.go, coordinator_cli_scope_test.go,
// coordinator_scope_recovery_test.go, cliscope_completion_hold_test.go).
// Everything here selects its authority through the foundation dual-mode
// helpers (batch_fixture_port_test.go) and never touches RUSH_BATCH_RUNTIME.
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

// b1BGShellFixture mirrors newBGShellFixture (bgshell_claim_test.go, owned
// by another slot and deliberately binding-free) with one addition: an
// explicit bindBatchNodeFacts, so a flag-ON CLIScope against the returned
// coordinator answers from batch node facts instead of the
// unknownBatchCutoverFacts sentinel ("batch facts: node source unavailable").
// Shape is otherwise identical: legacy workLedger + AsyncJobStore rows are
// the retained bg-shell row infrastructure on BOTH paths; the binding only
// feeds the facts seam.
func b1BGShellFixture(t *testing.T) (*coordinator, *session.AsyncJobStore, *shell.BackgroundShellManager, string) {
	t.Helper()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	registry := newWorkLedger(nil)
	registry.store = store
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(context.Background()) })
	coord := &coordinator{asyncJobs: registry, background: mgr}
	bindBatchNodeFacts(t, coord, conn, dataDir)
	return coord, store, mgr, t.TempDir()
}

// b1SeedCompletedJob is the dual-mode seedCompletedJob: flag ON projects the
// job as batch nodes (a command task's completion debt for childID=="", a
// terminal delegation child link otherwise), OFF keeps the legacy
// claim/announce/transition async_jobs row. Either way the owner ends with
// pending-inclusive wake=1 reaction debt.
func b1SeedCompletedJob(t *testing.T, conn *sql.DB, store *session.AsyncJobStore, ledger *workLedger, owner, callID string, kind session.JobKind, childID string) {
	t.Helper()
	if childID != "" {
		seedFixtureDelegation(t, conn, store, owner, callID, childID, kind, string(kind), "completed", false)
		return
	}
	seedFixtureJobDebt(t, conn, store, ledger, nil, owner, callID, false)
}

// b1SeedRunningJob is the dual-mode RUNNING-row seed: flag ON reserves a live
// batch root (state running, announced) for owner, OFF claims a running
// async_jobs row. Either way the owner has running work but no debt.
func b1SeedRunningJob(t *testing.T, conn *sql.DB, store *session.AsyncJobStore, owner, callID string) {
	t.Helper()
	if batchFlagEnabled() {
		seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
		return
	}
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: owner, ToolCallID: callID, Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
}

// b1SeedDelegationChildLink is the durable half of childDelegationBase's
// delegation seed. The in-memory half (workLedger.Start + acknowledged) is
// process-local park/deliver machinery and stays in the fixture on BOTH
// paths; it also claims the legacy async_jobs row, which is the durable
// delegation record under flag OFF. Under flag ON the ON readers
// (drainPermitted -> isDurableDelegationChild -> facts.DelegationChildEver,
// delegatedChildDriven, CLIScope's live reads) never see async_jobs, so the
// delegation must also exist as a live batch node child-link naming the
// child: seedBatchDelegation(owner, callID, child, "agent", "running")
// reserves and announces exactly that. Never touches RUSH_BATCH_RUNTIME.
func b1SeedDelegationChildLink(t *testing.T, conn *sql.DB, store *session.AsyncJobStore, owner, callID, childID string) {
	t.Helper()
	if !batchFlagEnabled() {
		return // OFF: workLedger.Start already claimed the async_jobs row
	}
	seedFixtureDelegation(t, conn, store, owner, callID, childID, session.JobKindAgent, AgentToolName, "running", false)
}

// b1SeedBatchRootOnHost reserves and announces a live (state-parameterised)
// batch root on an EXPLICIT host id -- the seedBatchDelegation twin for host
// identity, which hardcodes "fixture-host". Used by the dead-host scope
// recovery fixture to put running batch work exactly on the probed dead host.
func b1SeedBatchRootOnHost(t *testing.T, conn *sql.DB, owner, callID, host, state string) (root, child string) {
	t.Helper()
	ctx := context.Background()
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

// b1VisibleReactionDebtExists is the dual-mode read of the VISIBLE
// (delivery='done') reaction-debt predicate -- what a Drain's turn-start
// decision uses and what TransferOnly-style oracles assert stays behind for
// the driving loop. Flag ON reads BatchReactionVisibleDebtExists over the
// batch nodes on conn (async_jobs is never consulted); OFF keeps the legacy
// async_jobs predicate on store.
func b1VisibleReactionDebtExists(ctx context.Context, t *testing.T, conn *sql.DB, store *session.AsyncJobStore, owner string) bool {
	t.Helper()
	if batchFlagEnabled() {
		visible, err := session.NewBatchStore(conn).BatchReactionVisibleDebtExists(ctx, owner)
		require.NoError(t, err)
		return visible
	}
	visible, err := store.VisibleReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	return visible
}
