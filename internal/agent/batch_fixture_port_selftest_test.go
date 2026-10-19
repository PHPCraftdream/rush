// Mode-agnostic self-tests for the batch fixture port helpers
// (batch_fixture_port_test.go). Every test runs in WHATEVER runtime the
// invoking environment selects via RUSH_BATCH_RUNTIME and asserts only what
// both authorities genuinely share; batchFlagEnabled() branches appear only
// where the two authorities' state layout truly differs (fault-injector
// trigger names, the table each trigger guards, and the reaction write).
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchFixturePortSelftest_JobDebtRoundTrip seeds job debt in both
// delivery states and asserts the readback through whichever authority is
// selected: terminal state, wake armed, nothing reacted, and the delivery
// column distinguishing pending from pulled-visible debt.
func TestBatchFixturePortSelftest_JobDebtRoundTrip(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "selftest-debt-owner")
	require.NoError(t, err)

	// pending debt: terminal, wake armed, not yet delivered.
	seedFixtureJobDebt(t, env.conn, store, ledger, env.messages, owner.ID, "call-pending", false)
	row := readFixtureJobDebt(t, env.conn, store, owner.ID, "call-pending")
	require.Equal(t, "completed", row.State, "seeded debt is terminal in both modes")
	require.EqualValues(t, 1, row.Wake, "seeded debt wakes the owner in both modes")
	require.EqualValues(t, 0, row.Reacted, "pending debt has not been reacted to in either mode")
	require.Equal(t, "pending", row.Delivery, "unpulled debt stays delivery=pending in both modes")

	// visible debt: pulled, still unreacted.
	seedFixtureJobDebt(t, env.conn, store, ledger, env.messages, owner.ID, "call-visible", true)
	row = readFixtureJobDebt(t, env.conn, store, owner.ID, "call-visible")
	require.Equal(t, "completed", row.State)
	require.EqualValues(t, 1, row.Wake)
	require.EqualValues(t, 0, row.Reacted, "pulled debt is still outstanding reaction debt")
	require.Equal(t, "done", row.Delivery, "pulled debt is delivery=done in both modes")
}

// TestBatchFixturePortSelftest_RunningDelegation covers the two halves of the
// ScopeOpen contract for a LIVE delegation (owner -> child):
//
//   - "scope open": the child owns unreacted debt of its own, so the child's
//     scope stays open even though the delegation is merely live.
//   - closed: the child owns nothing, so the child's scope is closed -- the
//     delegation row itself is still live (view.State == "running").
//
// ScopeOpen is the CHILD scope's open state, not the delegation's liveness.
func TestBatchFixturePortSelftest_RunningDelegation(t *testing.T) {
	coord, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-running-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-running-owner")
	require.NoError(t, err)

	t.Run("DelegationChildScopeOpenWithOwnedDebt", func(t *testing.T) {
		seedFixtureDelegation(t, env.conn, store, owner.ID, "call-running", child.ID, session.JobKindCommand, "bash", "running", false)
		// Child-OWNED unreacted debt (pulled=false keeps delivery=pending,
		// i.e. never pulled, never reacted). StoreOnly is used so OFF mode
		// touches no in-memory ledger job; ON mode delegates to the batch
		// seed, which is store-only by construction.
		seedFixtureJobDebtStoreOnly(t, env.conn, store, env.messages, child.ID, "call-child-job", false)

		view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-running")
		require.Equal(t, "running", view.State, "the delegation stays live in both modes")
		require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips in both modes")
		require.True(t, view.ScopeOpen, "the child's OWN debt keeps its scope open in both modes")
		require.EqualValues(t, 1, view.Announced, "the root announcement round-trips in both modes (OFF: MarkAnnounced; ON: BatchAnnounceStarted)")
		require.EqualValues(t, 0, view.Reacted, "nothing reacted on a live delegation in either mode")
		// Static trace of the ScopeOpen=true answer (work_ledger_delegation.go):
		// childScopeDrained(child): l.running(child) == false (:225, no
		// in-memory job -- StoreOnly in OFF mode; batch seed in ON mode never
		// touches the ledger) -> coord.background/driver/IsSessionBusy gates
		// all false (:239-245, nothing drives the child here) ->
		// childScopeOpenAcrossProcesses(child) (:277) asks CLIScope(child):
		// no running row, but the child's unreacted debt exists
		// (coordinator_reaction_source.go:275-287: ReactionDebtExists /
		// SelectedDebtExists), WorkOpen stays false, and the drain verdict
		// allows the reaction, so Drain == DrainOwed (:282) -> scope open ->
		// childScopeOpenAcrossProcesses returns true -> childScopeDrained
		// returns false -> view.ScopeOpen = !drained = true.
		_ = coord // wired into ledger for the cross-process scope read
	})

	t.Run("DelegationChildScopeClosedWithoutOwnedWork", func(t *testing.T) {
		// A SEPARATE child session: the open case above left unreacted debt owned
		// by `child`, and the scope predicate is per child session.
		emptyChild, createErr := env.sessions.Create(ctx, "selftest-running-empty-child")
		require.NoError(t, createErr)
		seedFixtureDelegation(t, env.conn, store, owner.ID, "call-running-empty", emptyChild.ID, session.JobKindCommand, "bash", "running", false)

		view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-running-empty")
		// Trace: childScopeDrained(child): :225 false (no ledger job),
		// :239-245 false (no driver/busy), CLIScope: the empty child owns no
		// running work and no debt, so ReactionDebtExists/SelectedDebtExists
		// are false -> coordinator_reaction_source.go:286-287 returns early
		// with Drain != DrainOwed and WorkOpen false -> drained -> ScopeOpen
		// is false. The DELEGATION itself is unaffected: it is still live.
		require.False(t, view.ScopeOpen, "a child that owns no work and no debt has a closed scope in both modes")
		require.Equal(t, "running", view.State, "the delegation row itself is still live in both modes")
		require.EqualValues(t, 1, view.Announced, "the root announcement round-trips in both modes")
	})
}

// TestBatchFixturePortSelftest_TerminalDelegationNoNotice seeds a released
// delegation and asserts the terminal state, zero selected-notice, and the
// child identity round-trip in both modes.
func TestBatchFixturePortSelftest_TerminalDelegationNoNotice(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-terminal-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-terminal-owner")
	require.NoError(t, err)
	seedFixtureDelegation(t, env.conn, store, owner.ID, "call-terminal", child.ID, session.JobKindCommand, "bash", "completed", false)

	view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-terminal")
	require.Equal(t, "completed", view.State, "the released delegation is terminal in both modes")
	require.EqualValues(t, 0, view.NoticeSelected, "seedBatchDelegation synthesizes no notice; the OFF transition sets no NoticeKind either")
	require.EqualValues(t, 1, view.Announced, "announced comes from the ROOT row in ON mode and the legacy row in OFF mode; both seeds announce")
	require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips in both modes")
}

// TestBatchFixturePortSelftest_TerminalDelegationWithNotice seeds a released
// delegation WITH a selected completion notice (never pulled) and round-trips
// the notice state through readFixtureDelegation in both modes, then performs
// the mode-appropriate production-shaped reaction write and proves the
// selected notice survives it with reacted set.
func TestBatchFixturePortSelftest_TerminalDelegationWithNotice(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-terminal-notice-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-terminal-notice-owner")
	require.NoError(t, err)
	seedRoot, seedChild := seedFixtureDelegation(t, env.conn, store, owner.ID, "call-terminal-notice", child.ID, session.JobKindCommand, "bash", "completed", true)

	view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-terminal-notice")
	require.Equal(t, "completed", view.State, "the released delegation is terminal in both modes")
	require.EqualValues(t, 1, view.NoticeSelected, "the completion notice is selected in both modes (ON: Project Notices; OFF: Transition NoticeKind)")
	require.EqualValues(t, 0, view.Reacted, "the selected notice has not been reacted to yet")
	require.Equal(t, "pending", view.Delivery, "the notice is deliberately left unpulled: delivery=pending in both modes")
	require.EqualValues(t, 1, view.Wake, "the notice wakes the owner in both modes")
	require.EqualValues(t, 1, view.Announced, "the root was announced before the notice was selected in both modes")

	// React the selected notice by a direct column update (readback check only; it bypasses the production delivery guards, db/sql/batch_outbox.sql:54-57), with no
	// failFixture* trigger installed (nothing to collide with). OFF keys the
	// legacy row by owner+tool_call; ON keys the batch child node by id.
	if batchFlagEnabled() {
		require.NotEmpty(t, seedChild, "the batch seed returns the child node id")
		_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE node_id=?`, seedChild)
	} else {
		require.Empty(t, seedRoot, "the legacy seed returns no batch ids")
		_, err = env.conn.ExecContext(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id=? AND tool_call_id=?`, owner.ID, "call-terminal-notice")
	}
	require.NoError(t, err)

	view = readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-terminal-notice")
	require.EqualValues(t, 1, view.NoticeSelected, "reacting does not deselect the notice in either mode")
	require.EqualValues(t, 1, view.Reacted, "the reaction write is visible in both modes")
}

// TestBatchFixturePortSelftest_FaultInjectorsObservable proves each fault
// injector actually fires -- and that the two are DISTINGUISHABLE by the
// settle-write outcome:
//
//   - failFixtureReaction: the ordinary reaction write aborts with the
//     fixture's 'fx:' prefix, while the settle-by-failure write (reacted=1
//     AND reacted_failed=1) does NOT match its trigger condition
//     (NEW.reacted=1 AND NEW.reacted_failed=0) and succeeds.
//   - failFixtureSettle: blocks EVERY reaction write (`WHEN NEW.reacted=1`),
//     so BOTH the ordinary and the settle-by-failure write abort.
//
// The DISTINGUISHING property between the two injectors is the settle-write
// outcome: blocked only by the settle injector. (Regression caught: giving
// failFixtureSettle the ordinary predicate reacted=1 AND reacted_failed=0
// would let its settle write succeed -> this test goes red.) The trigger
// name and guarded table differ per mode (that is the genuine authority
// difference), so those two choices are the only batchFlagEnabled()
// branches. Each section seeds its OWN debt row (fresh call id) so no
// section inherits another's reacted/reacted_failed state.
func TestBatchFixturePortSelftest_FaultInjectorsObservable(t *testing.T) {
	_, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "selftest-fault-owner")
	require.NoError(t, err)
	reactionTable, settleTable := "async_jobs", "async_jobs"
	if batchFlagEnabled() {
		reactionTable, settleTable = "batch_nodes", "batch_nodes"
	}

	// --- failFixtureReaction: ordinary write aborts; settle write goes through.
	seedFixtureJobDebtStoreOnly(t, env.conn, store, env.messages, owner.ID, "call-fx-react", false)
	failFixtureReaction(t, env.conn)
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1 WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-react'`)
	require.ErrorContains(t, err, "fx:", "the ordinary reaction write must abort under failFixtureReaction")
	// The settle write sets reacted_failed=1, which the
	// NEW.reacted=1 AND NEW.reacted_failed=0 trigger condition does NOT match:
	// it must go through.
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1, reacted_failed=1 WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-react'`)
	require.NoError(t, err, "the settle-by-failure write is not matched by the reaction trigger and must succeed")
	if batchFlagEnabled() {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)
	} else {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_legacy_reaction`)
	}

	// --- failFixtureSettle: BOTH writes abort (WHEN NEW.reacted=1).
	seedFixtureJobDebtStoreOnly(t, env.conn, store, env.messages, owner.ID, "call-fx-settle", false)
	failFixtureSettle(t, env.conn)
	_, err = env.conn.ExecContext(ctx, `UPDATE `+settleTable+` SET reacted=1 WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-settle'`)
	require.ErrorContains(t, err, "fx:", "failFixtureSettle blocks the ordinary reaction write too")
	_, err = env.conn.ExecContext(ctx, `UPDATE `+settleTable+` SET reacted=1, reacted_failed=1 WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-settle'`)
	require.ErrorContains(t, err, "fx:", "failFixtureSettle blocks the settle-by-failure write -- this is the property that distinguishes it from failFixtureReaction")
	if batchFlagEnabled() {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)
		// The batch mode also guards session_notices: prove that half fires.
		_, err = store.InsertSessionNoticeReturningID(ctx, owner.ID, session.NoticeKindBGShellDone, "bg", true, "")
		require.NoError(t, err)
		_, err = env.conn.ExecContext(ctx, `UPDATE session_notices SET reacted=1, reacted_failed=1 WHERE owner='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_notice_reaction`)
	} else {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_legacy_settle`)
	}

	// failFixturePull: a delivery insert must abort. Both modes guard the
	// same messages table, so no flag branch is needed for the statement --
	// only for the trigger name being dropped.
	failFixturePull(t, env.conn)
	sess, err := env.sessions.Create(ctx, "selftest-pull-sink")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, batchFixtureMessage("selftest pull probe"))
	require.ErrorContains(t, err, "fx:")
	if batchFlagEnabled() {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_pull`)
	} else {
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_legacy_pull`)
	}
}
