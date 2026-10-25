// W5 regression oracles for the review docs/plans section 2b (supervision
// check-ins already count as batch debt -- the chain supervision.go
// InsertSessionNotice(..., session.NoticeKindSupervision, text, true, "") ->
// coordinator_drain_policy.go drainDecision's SelectedDebtExists ->
// batch_cutover_facts_node.go -> internal/session/batch_reaction.go
// batchDebtSnapshot reading BOTH batch_nodes debt and retained session
// notices -> internal/db/sql/batch_outbox.sql BatchOutboxDebtNotices
// including every wake=1, reacted=0, delivery pending|done session notice
// with NO kind exclusion) and section 2e (unknownBatchCutoverFacts'
// conservative (true, nil) is intended today; P2 backlog).
//
// All tests bind the batch store directly (they never touch any
// environment variable).
// No assertion here has the form "every supervision tick invokes Run":
// legitimate refusals exist downstream of the debt/arbiter gates.
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

// w5SeedRunningRoot reserves and announces a one-child batch root whose root
// and child both stay RUNNING with no selected notice: live scope for owner,
// zero completion debt (the Reserve+BatchAnnounceStarted shape of
// seedBatchDelegation's live path).
func w5SeedRunningRoot(t *testing.T, conn *sql.DB, messages message.Service, owner, callID string) (root, child string) {
	t.Helper()
	ctx := context.Background()
	ensureFixtureSession(t, conn, owner)
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
	root, child = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, messages, owner, root, batchFixtureMessage("w5 running started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, child
}

// w5InsertSupervisionNotice inserts one pending wake-enabled supervision
// check-in session notice exactly as production does (supervision.go
// InsertSessionNotice(..., session.NoticeKindSupervision, text, true, "")).
func w5InsertSupervisionNotice(t *testing.T, conn *sql.DB, owner, text string) {
	t.Helper()
	ensureFixtureSession(t, conn, owner)
	require.NoError(t, session.NewSessionNoticeStore(conn).InsertSessionNotice(
		context.Background(), owner, session.NoticeKindSupervision, text, true, ""))
}

// w5NoticeDelivery reads one owner's session notices' delivery column
// directly.
func w5NoticeDelivery(t *testing.T, conn *sql.DB, owner string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(),
		`SELECT delivery FROM session_notices WHERE owner = ?1 ORDER BY id`, owner)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		require.NoError(t, rows.Scan(&d))
		out = append(out, d)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestBatchSupervisionNoticeDebt_EligibleCase is the positive oracle for the
// 2b chain: a pending wake-enabled supervision check-in alone is reaction
// debt (BatchOutboxDebtNotices' kind-agnostic predicate), the bound node
// facts surface it as SelectedDebtExists (drainDecision's input half), the
// arbiter allows exactly one launch, and the check-in is pulled into history
// exactly once with its wake bit intact.
func TestBatchSupervisionNoticeDebt_EligibleCase(t *testing.T) {
	// No t.Parallel: t.Setenv panics on a parallel test.
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-supervision-debt-owner")
	require.NoError(t, err)
	w5SeedRunningRoot(t, env.conn, env.messages, owner.ID, "call-w5-sup-1")
	w5InsertSupervisionNotice(t, env.conn, owner.ID, "check the soak")

	// Store-level debt half: the pending wake-enabled supervision notice
	// alone is reaction debt. Excluding kind='supervision' in
	// batch_outbox.sql's BatchOutboxDebtNotices or in the batchDebtSnapshot
	// composition makes this false.
	debt, err := session.NewBatchStore(env.conn).BatchReactionDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.True(t, debt, "the pending wake-enabled supervision notice alone is reaction debt")

	// Facts half (drainDecision's input): the bound node facts see it too.
	selected, err := coordinatorBatchCutoverFacts(coord).SelectedDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.True(t, selected, "the bound node facts report the supervision check-in as selected debt")

	// Arbiter: the eligible case is permitted -- exactly one launch would
	// follow (no assertion that any tick invokes Run; refusals exist).
	hasDebt, dv, err := coord.drainDecision(ctx, owner.ID, false)
	require.NoError(t, err)
	require.True(t, hasDebt)
	require.Equal(t, drainAllow, dv.kind)

	// Pull: exactly one check-in is delivered, once, with its wake bit.
	pulled, err := coordinatorBatchCutoverFacts(coord).PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "exactly one check-in is pulled")
	require.True(t, pulled[0].Wake, "the check-in's wake reaches the owner")
	pulled2, err := coordinatorBatchCutoverFacts(coord).PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled2, 0, "delivered exactly once")
}

// TestBatchSupervisionNoticeDebt_ClosedScopeVoidsCheckIn is the negative
// twin: with NO live batch work for the owner, the pending supervision
// check-in is debt before the pull, is voided instead of delivered, and a
// voided row leaves no debt (BatchOutboxDebtNotices excludes
// delivery='void'). Store-level and mode-independent: no flag.
func TestBatchSupervisionNoticeDebt_ClosedScopeVoidsCheckIn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-closed-scope-owner")
	require.NoError(t, err)
	w5InsertSupervisionNotice(t, env.conn, owner.ID, "check the quiet")

	debt, err := session.NewBatchStore(env.conn).BatchReactionDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.True(t, debt, "pending debt exists before the pull")

	live := batchFixtureLiveReader(env.conn, env.workingDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(env.conn), live, env.conn)
	out, err := facts.PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, out, 0, "closed scope voids the check-in instead of delivering it")

	require.Equal(t, []string{"void"}, w5NoticeDelivery(t, env.conn, owner.ID))

	debt, err = session.NewBatchStore(env.conn).BatchReactionDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.False(t, debt, "a voided row leaves no debt")
}

// TestBatchSupervisionNoticeDebt_CompletionClassForSleepAll pins the policy
// twin: a pending supervision check-in is real pending-inclusive debt, and
// it is completion-class for SleepAll (batch_cutover_facts.go's
// CompletionOnly switch counts session.NoticeKindSupervision): an armed
// sleep holds across a pending check-in instead of being broken as ordinary
// work. Dropping session.NoticeKindSupervision from that switch flips
// CompletionOnly to false. No assertion that any tick invokes Run --
// legitimate refusals exist downstream.
func TestBatchSupervisionNoticeDebt_CompletionClassForSleepAll(t *testing.T) {
	// No t.Parallel: t.Setenv panics on a parallel test.
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-sleepall-owner")
	require.NoError(t, err)
	w5SeedRunningRoot(t, env.conn, env.messages, owner.ID, "call-w5-sleep")
	w5InsertSupervisionNotice(t, env.conn, owner.ID, "check the soak")

	f, err := coord.readTurnFacts(ctx, owner.ID, siteCLI)
	require.NoError(t, err)
	require.True(t, f.Debt.PendingIncl, "supervision debt is real debt")
	require.True(t, f.Debt.CompletionOnly,
		"supervision is completion-class for SleepAll: an armed sleep holds across a pending check-in")
}

// TestBatchSupervisionNoticeDebt_DeliveryFailurePreservesPendingDebt: when
// the history insert fails (BEFORE INSERT ON messages ABORT trigger, what
// production pullOneSessionNotice's messages.CreateTx hits), the pull skips
// the row, the transaction rolls back to delivery='pending', and the debt
// survives. Store-level and mode-independent.
func TestBatchSupervisionNoticeDebt_DeliveryFailurePreservesPendingDebt(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-failed-pull-owner")
	require.NoError(t, err)
	w5SeedRunningRoot(t, env.conn, env.messages, owner.ID, "call-w5-failpull")
	w5InsertSupervisionNotice(t, env.conn, owner.ID, "check the soak")
	failBatchPull(t, env.conn)

	live := batchFixtureLiveReader(env.conn, env.workingDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(env.conn), live, env.conn)
	out, err := facts.PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err, "the pull skips failed rows instead of failing")
	require.Len(t, out, 0)
	require.Equal(t, []string{"pending"}, w5NoticeDelivery(t, env.conn, owner.ID))

	debt, err := session.NewBatchStore(env.conn).BatchReactionDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.True(t, debt, "a delivery failure preserves the pending debt")
}

// TestBatchCutoverUnknownFacts_ConservativeNoRunNoIdle pins section 2e: an
// UNBOUND flag-ON coordinator resolves to unknownBatchCutoverFacts, whose
// conservative (true, nil) SelectedDebtExists is INTENDED today
// (batch_cutover_facts.go): never a false idle, never a paid Run. P2
// backlog -- a future fix must distinguish a permanent missing binding from
// a transient DB read error at the callers, because wakeSession adds the
// session to the recheck set on the error path (coordinator_wake.go), so
// returning (true, errBatchFactsUnavailable) alone does not implement
// "never retry".
func TestBatchCutoverUnknownFacts_ConservativeNoRunNoIdle(t *testing.T) {
	// No t.Parallel: t.Setenv panics on a parallel test.
	env := testEnv(t)
	ctx := context.Background()
	// Deliberately NO bindBatchNodeFacts: the unbound shape.
	coord := newBatchRuntimeCoordinator()
	coord.sessions, coord.messages = env.sessions, env.messages
	t.Cleanup(func() {
		coord.waitRecheckWakes()
		coord.releaseWG.Wait()
		coord.waitRecheckWakes()
	})

	owner, err := env.sessions.Create(ctx, "w5-unknown-facts-owner")
	require.NoError(t, err)

	facts := coordinatorBatchCutoverFacts(coord)
	_, ok := facts.(unknownBatchCutoverFacts)
	require.True(t, ok, "an unbound flag-ON coordinator resolves to the documented fail-safe facts")

	debt, err := facts.SelectedDebtExists(ctx, owner.ID)
	require.NoError(t, err)
	require.True(t, debt, "conservative debt: never a false idle")

	_, capErr := facts.CaptureVisibleDebt(ctx, owner.ID)
	require.ErrorIs(t, capErr, ErrBatchFactsSourceUnavailable, "the unavailable-source error is terminal and surfaced")

	debt2, dv, decErr := coord.drainDecision(ctx, owner.ID, false)
	require.NoError(t, decErr)
	require.True(t, debt2, "unknown debt never reads as no debt")
	require.NotEqual(t, drainAllow, dv.kind,
		"no paid Run: the deferred verdict surfaces instead (PendingInclusiveDebt fails closed)")
}
