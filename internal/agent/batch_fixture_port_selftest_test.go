// Batch-only self-tests for the batch fixture port helpers
// (batch_fixture_port_test.go): every helper's seeded state and every fault
// injector is proven observable on the batch store.
package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchFixturePortSelftest_JobDebtRoundTrip seeds job debt in both
// delivery states and asserts the readback through the batch store:
// terminal state, wake armed, nothing reacted, and the delivery column
// distinguishing pending from pulled-visible debt.
func TestBatchFixturePortSelftest_JobDebtRoundTrip(t *testing.T) {
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "selftest-debt-owner")
	require.NoError(t, err)

	// pending debt: terminal, wake armed, not yet delivered.
	seedFixtureJobDebt(t, env.conn, owner.ID, "call-pending", false)
	row := readFixtureJobDebt(t, env.conn, owner.ID, "call-pending")
	require.Equal(t, "completed", row.State, "seeded debt is terminal")
	require.EqualValues(t, 1, row.Wake, "seeded debt wakes the owner")
	require.EqualValues(t, 0, row.Reacted, "pending debt has not been reacted to")
	require.Equal(t, "pending", row.Delivery, "unpulled debt stays delivery=pending")

	// visible debt: pulled, still unreacted.
	seedFixtureJobDebt(t, env.conn, owner.ID, "call-visible", true)
	row = readFixtureJobDebt(t, env.conn, owner.ID, "call-visible")
	require.Equal(t, "completed", row.State)
	require.EqualValues(t, 1, row.Wake)
	require.EqualValues(t, 0, row.Reacted, "pulled debt is still outstanding reaction debt")
	require.Equal(t, "done", row.Delivery, "pulled debt is delivery=done")
}

// TestBatchFixturePortSelftest_RunningDelegation covers the two halves of the
// ScopeOpen contract for a LIVE delegation (owner -> child):
//
//   - "scope open": the delegation child link is live (the child link is what
//     the batch view reports), so the child's scope stays open.
//   - closed: an unnamed child has no scope to keep open, even though the
//     delegation row itself is still live (view.State == "running").
//
// ScopeOpen is the CHILD scope's open state, not the delegation's liveness.
func TestBatchFixturePortSelftest_RunningDelegation(t *testing.T) {
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-running-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-running-owner")
	require.NoError(t, err)

	t.Run("DelegationChildScopeOpenWithOwnedDebt", func(t *testing.T) {
		seedFixtureDelegation(t, env.conn, owner.ID, "call-running", child.ID, "command", "running", false)
		// Child-OWNED unreacted debt (pulled=false keeps delivery=pending,
		// i.e. never pulled, never reacted).
		seedFixtureJobDebtStoreOnly(t, env.conn, child.ID, "call-child-job", false)

		view := readFixtureDelegation(t, env.conn, owner.ID, "call-running")
		require.Equal(t, "running", view.State, "the delegation stays live")
		require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips")
		require.True(t, view.ScopeOpen, "a live child link keeps the child's scope open")
		require.EqualValues(t, 1, view.Announced, "the root announcement round-trips (BatchAnnounceStarted)")
		require.EqualValues(t, 0, view.Reacted, "nothing reacted on a live delegation")
	})

	t.Run("DelegationChildScopeClosedWithoutOwnedWork", func(t *testing.T) {
		// A SEPARATE child session: the open case above left a live link owned
		// by `child`, and the scope predicate is per child session. An empty
		// child id has no scope to keep open.
		view := readFixtureDelegation(t, env.conn, owner.ID, "call-running")
		require.False(t, view.ScopeOpen, "a child that owns no work and no debt has a closed scope")
		require.Equal(t, "running", view.State, "the delegation row itself is still live")
		require.EqualValues(t, 1, view.Announced, "the root announcement round-trips")
	})
}

// TestBatchFixturePortSelftest_TerminalDelegationNoNotice seeds a released
// delegation and asserts the terminal state, zero selected-notice, and the
// child identity round-trip.
func TestBatchFixturePortSelftest_TerminalDelegationNoNotice(t *testing.T) {
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-terminal-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-terminal-owner")
	require.NoError(t, err)
	seedFixtureDelegation(t, env.conn, owner.ID, "call-terminal", child.ID, "command", "completed", false)

	view := readFixtureDelegation(t, env.conn, owner.ID, "call-terminal")
	require.Equal(t, "completed", view.State, "the released delegation is terminal")
	require.EqualValues(t, 0, view.NoticeSelected, "seedBatchDelegation synthesizes no notice")
	require.EqualValues(t, 1, view.Announced, "the seed announces the root")
	require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips")
}

// TestBatchFixturePortSelftest_TerminalDelegationWithNotice seeds a released
// delegation WITH a selected completion notice (never pulled) and round-trips
// the notice state through readFixtureDelegation, then performs the
// production-shaped direct reaction write and proves the selected notice
// survives it with reacted set.
func TestBatchFixturePortSelftest_TerminalDelegationWithNotice(t *testing.T) {
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-terminal-notice-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-terminal-notice-owner")
	require.NoError(t, err)
	seedRoot, seedChild := seedFixtureDelegation(t, env.conn, owner.ID, "call-terminal-notice", child.ID, "command", "completed", true)

	view := readFixtureDelegation(t, env.conn, owner.ID, "call-terminal-notice")
	require.Equal(t, "completed", view.State, "the released delegation is terminal")
	require.EqualValues(t, 1, view.NoticeSelected, "the completion notice is selected")
	require.EqualValues(t, 0, view.Reacted, "the selected notice has not been reacted to yet")
	require.Equal(t, "pending", view.Delivery, "the notice is deliberately left unpulled: delivery=pending")
	require.EqualValues(t, 1, view.Wake, "the notice wakes the owner")
	require.EqualValues(t, 1, view.Announced, "the root was announced before the notice was selected")
	require.NotEmpty(t, seedRoot)
	require.NotEmpty(t, seedChild)

	// React the selected notice by a direct column update (readback check
	// only; it bypasses the production delivery guards, db/sql/batch_outbox.sql:54-57),
	// with no failFixture* trigger installed (nothing to collide with). The
	// batch child node is keyed by id.
	_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE node_id=?`, seedChild)
	require.NoError(t, err)

	view = readFixtureDelegation(t, env.conn, owner.ID, "call-terminal-notice")
	require.EqualValues(t, 1, view.NoticeSelected, "reacting does not deselect the notice")
	require.EqualValues(t, 1, view.Reacted, "the reaction write is visible")
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
// would let its settle write succeed -> this test goes red.) Each section
// seeds its OWN debt row (fresh call id) so no section inherits another's
// reacted/reacted_failed state.
func TestBatchFixturePortSelftest_FaultInjectorsObservable(t *testing.T) {
	_, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "selftest-fault-owner")
	require.NoError(t, err)
	reactionTable := "batch_nodes"

	// --- failFixtureReaction: ordinary write aborts; settle write goes through.
	seedFixtureJobDebtStoreOnly(t, env.conn, owner.ID, "call-fx-react", false)
	failFixtureReaction(t, env.conn)
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1 WHERE owner_session_id=`+"'"+owner.ID+"'"+` AND (tool_call_id='call-fx-react' OR root_id IN (SELECT node_id FROM batch_nodes WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-react'))`)
	require.ErrorContains(t, err, "fx:", "the ordinary reaction write must abort under failFixtureReaction")
	// The settle write sets reacted_failed=1, which the
	// NEW.reacted=1 AND NEW.reacted_failed=0 trigger condition does NOT match:
	// it must go through.
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1, reacted_failed=1 WHERE owner_session_id='`+owner.ID+`' AND (tool_call_id='call-fx-react' OR root_id IN (SELECT node_id FROM batch_nodes WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-react'))`)
	require.NoError(t, err, "the settle-by-failure write is not matched by the reaction trigger and must succeed")
	_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)

	// --- failFixtureSettle: BOTH writes abort (WHEN NEW.reacted=1).
	seedFixtureJobDebtStoreOnly(t, env.conn, owner.ID, "call-fx-settle", false)
	failFixtureSettle(t, env.conn)
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1 WHERE owner_session_id='`+owner.ID+`' AND (tool_call_id='call-fx-settle' OR root_id IN (SELECT node_id FROM batch_nodes WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-settle'))`)
	require.ErrorContains(t, err, "fx:", "failFixtureSettle blocks the ordinary reaction write too")
	_, err = env.conn.ExecContext(ctx, `UPDATE `+reactionTable+` SET reacted=1, reacted_failed=1 WHERE owner_session_id='`+owner.ID+`' AND (tool_call_id='call-fx-settle' OR root_id IN (SELECT node_id FROM batch_nodes WHERE owner_session_id='`+owner.ID+`' AND tool_call_id='call-fx-settle'))`)
	require.ErrorContains(t, err, "fx:", "failFixtureSettle blocks the settle-by-failure write -- this is the property that distinguishes it from failFixtureReaction")
	_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)
	// The batch world also guards session_notices: prove that half fires.
	notices := session.NewSessionNoticeStore(env.conn)
	_, err = notices.InsertSessionNoticeReturningID(ctx, owner.ID, session.NoticeKindBGShellDone, "bg", true, "")
	require.NoError(t, err)
	_, err = env.conn.ExecContext(ctx, `UPDATE session_notices SET reacted=1, reacted_failed=1 WHERE owner='`+owner.ID+`'`)
	require.ErrorContains(t, err, "fx:")
	_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_notice_reaction`)

	// failFixturePull: a delivery insert must abort (same messages table).
	failFixturePull(t, env.conn)
	sess, err := env.sessions.Create(ctx, "selftest-pull-sink")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, batchFixtureMessage("selftest pull probe"))
	require.ErrorContains(t, err, "fx:")
	_, _ = env.conn.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, "fx_batch_pull"))
}
