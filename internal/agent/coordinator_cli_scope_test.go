// CLIScope (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.5): the
// CLI loop's ONE between-turns answer. Reaction debt is split by the same
// session policy wakeSession and the Drain turn-start re-check apply, so the
// loop never waits for a turn the policy will never allow (a refused
// bg-shell-only notice used to keep `rush run` spinning until --timeout).
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedCompletedJob claims, announces and completes a wake=1 job for owner
// (pending-inclusive reaction debt), optionally naming childID as its
// delegation target.
func seedCompletedJob(t *testing.T, store *session.AsyncJobStore, owner, callID string, kind session.JobKind, childID string) {
	t.Helper()
	ctx := context.Background()
	_, err := store.Claim(ctx, session.ClaimParams{
		Owner: owner, ToolCallID: callID, Kind: kind, Input: "x", ChildSessionID: childID, ToolName: string(kind),
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, owner, callID))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: owner, ToolCallID: callID, State: "completed", ResultSummary: "done", Wake: true,
	})
	require.NoError(t, err)
}

func seedBGShellNotice(t *testing.T, store *session.AsyncJobStore, owner string) {
	t.Helper()
	require.NoError(t, store.InsertSessionNotice(context.Background(), owner, session.NoticeKindBGShellDone, "bg shell finished", true, ""))
}

// TestCLIScope_SplitsDebtByPolicy walks every refusal reason of the session
// policy (bg-shell-only with AutoResumeOnJobDone off, Stop's suspension, a
// released delegation child) plus the allowed shapes: refused debt is
// DeferredDebt and never TurnOwed, and running work stays WorkOpen either way.
//
// Revert-check performed: made CLIScope report every debt as TurnOwed (skip
// the policy) -- the bg-shell-only, suspended and released-child cases FAILED
// (TurnOwed was true, DeferredDebt false), which is exactly the old
// pending-inclusive-predicate behaviour behind the spin.
func TestCLIScope_SplitsDebtByPolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("nothing", func(t *testing.T) {
		coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{}, st)
	})

	t.Run("running job only", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "run-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{WorkOpen: true}, st)
	})

	t.Run("bg-shell-only debt is deferred", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		seedBGShellNotice(t, store, sess.ID)
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{DeferredDebt: true}, st)
	})

	t.Run("bg-shell debt beside running work is deferred and waiting", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		seedBGShellNotice(t, store, sess.ID)
		_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "run-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
		require.NoError(t, err)
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{WorkOpen: true, DeferredDebt: true}, st)
	})

	t.Run("bg-shell notice beside job debt makes the mixed debt owed", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		seedBGShellNotice(t, store, sess.ID)
		seedCompletedJob(t, store, sess.ID, "call-1", session.JobKindCommand, "")
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{TurnOwed: true}, st)
	})

	t.Run("plain job debt is owed", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		seedCompletedJob(t, store, sess.ID, "call-1", session.JobKindCommand, "")
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{TurnOwed: true}, st)
	})

	t.Run("Stop-suspended debt is deferred", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		sess, err := getEnv(ctx).sessions.Create(ctx, "s")
		require.NoError(t, err)
		seedCompletedJob(t, store, sess.ID, "call-1", session.JobKindCommand, "")
		coord.suspendAutoResume(sess.ID)
		st, err := coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{DeferredDebt: true}, st)
		coord.ResetAutoResumeCounter(sess.ID)
		st, err = coord.CLIScope(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{TurnOwed: true}, st, "a human message re-arms the turn")
	})

	t.Run("released delegation child debt is deferred", func(t *testing.T) {
		coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
		env := getEnv(ctx)
		parent, err := env.sessions.Create(ctx, "parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(ctx, "task-call-1", parent.ID, "child")
		require.NoError(t, err)
		seedCompletedJob(t, store, parent.ID, "deleg-1", session.JobKindAgent, child.ID) // released: terminal
		seedCompletedJob(t, store, child.ID, "child-call", session.JobKindCommand, "")   // the child's own debt
		st, err := coord.CLIScope(ctx, child.ID)
		require.NoError(t, err)
		require.Equal(t, CLIScopeState{DeferredDebt: true}, st)
	})
}

// TestCLIScope_ForeignLiveDriver_DebtDeferred: with a live driver in another
// process the policy refuses, so the loop treats the debt as deferred too.
//
// Revert-check performed: same policy-skipping CLIScope as above -- FAILED
// (TurnOwed).
func TestCLIScope_ForeignLiveDriver_DebtDeferred(t *testing.T) {
	ctx := context.Background()
	f := newForeignDriverFx(t, "cli-scope-foreign")
	f.claimAndFinish(t, ctx, "call-1")
	require.NoError(t, f.cli.ClaimSessionDriver(ctx, f.sessID))

	st, err := f.coord.CLIScope(ctx, f.sessID)
	require.NoError(t, err)
	require.Equal(t, CLIScopeState{DeferredDebt: true}, st)
}
