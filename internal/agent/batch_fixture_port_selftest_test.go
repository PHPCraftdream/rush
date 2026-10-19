// Mode-agnostic self-tests for the batch fixture port helpers
// (batch_fixture_port_test.go). Every test runs in WHATEVER runtime the
// invoking environment selects via RUSH_BATCH_RUNTIME and asserts only what
// both authorities genuinely share; batchFlagEnabled() branches appear only
// where the two authorities' state layout truly differs (fault-injector
// trigger names and the table each trigger guards).
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

// TestBatchFixturePortSelftest_RunningDelegation seeds a running delegation
// and round-trips it through readFixtureDelegation: live state and an open
// child scope in BOTH modes (the authorities differ in where the row lives,
// not in the answer).
func TestBatchFixturePortSelftest_RunningDelegation(t *testing.T) {
	coord, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-running-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-running-owner")
	require.NoError(t, err)
	seedFixtureDelegation(t, env.conn, store, owner.ID, "call-running", child.ID, session.JobKindCommand, "bash", "running")

	view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-running")
	require.Equal(t, "running", view.State, "the delegation stays live in both modes")
	require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips in both modes")
	require.True(t, view.ScopeOpen, "a running delegation keeps the child's scope open; both authorities read it from their own store, so no flag branch is needed here")
	require.EqualValues(t, 0, view.Reacted, "nothing reacted on a live delegation in either mode")
	_ = coord // wired into ledger for the cross-process scope read
}

// TestBatchFixturePortSelftest_TerminalDelegationNoNotice seeds a released
// delegation and asserts the terminal state, zero selected-notice, and the
// child identity round-trip in both modes. A terminal-WITH-notice variant is
// deliberately skipped: seedBatchDelegation never synthesizes a completion
// notice, and the OFF legacy path cannot represent a selected notice on a
// settled delegation row without the pull machinery, so no dual-mode
// assertion exists (and no new seed API is added for it).
func TestBatchFixturePortSelftest_TerminalDelegationNoNotice(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "selftest-terminal-child")
	require.NoError(t, err)
	owner, err := env.sessions.Create(ctx, "selftest-terminal-owner")
	require.NoError(t, err)
	seedFixtureDelegation(t, env.conn, store, owner.ID, "call-terminal", child.ID, session.JobKindCommand, "bash", "completed")

	view := readFixtureDelegation(t, env.conn, store, ledger, owner.ID, "call-terminal")
	require.Equal(t, "completed", view.State, "the released delegation is terminal in both modes")
	require.EqualValues(t, 0, view.NoticeSelected, "seedBatchDelegation synthesizes no notice; the OFF transition sets no NoticeKind either")
	require.Equal(t, child.ID, view.ChildSessionID, "the child identity round-trips in both modes")
}

// TestBatchFixturePortSelftest_FaultInjectorsObservable proves each fault
// injector actually fires: after installing it, a matching write against the
// guarded table must fail with the fixture's 'fx:' prefix. The trigger name
// and guarded table differ per mode (that is the genuine authority
// difference), so those two choices are the only batchFlagEnabled() branches.
func TestBatchFixturePortSelftest_FaultInjectorsObservable(t *testing.T) {
	_, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "selftest-fault-owner")
	require.NoError(t, err)

	// Seed one debt row so the reaction/settle UPDATEs have a row to hit.
	seedFixtureJobDebtStoreOnly(t, env.conn, store, env.messages, owner.ID, "call-fault", false)

	// failFixtureReaction: a plain reaction write must abort.
	failFixtureReaction(t, env.conn)
	if batchFlagEnabled() {
		_, err := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE owner_session_id='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)
	} else {
		_, err := env.conn.ExecContext(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_legacy_reaction`)
	}

	// failFixtureSettle: settle-by-failure writes must abort too. The batch
	// mode also guards session_notices, so seed one notice row there and
	// prove that half fires as well.
	failFixtureSettle(t, env.conn)
	if batchFlagEnabled() {
		_, err := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE owner_session_id='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_node_reaction`)
		_, err = store.InsertSessionNoticeReturningID(ctx, owner.ID, session.NoticeKindBGShellDone, "bg", true, "")
		require.NoError(t, err)
		_, err = env.conn.ExecContext(ctx, `UPDATE session_notices SET reacted=1 WHERE owner='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
		_, _ = env.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS fx_batch_notice_reaction`)
	} else {
		_, err := env.conn.ExecContext(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+owner.ID+`'`)
		require.ErrorContains(t, err, "fx:")
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
