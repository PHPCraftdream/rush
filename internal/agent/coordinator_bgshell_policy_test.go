// W-DRAIN item 7 (B-dev6, docs/reviews/2026-09-29-async-phase4-round1.md):
// the policy table's "Background shell: only with AutoResumeOnJobDone" row
// was enforced ONLY at notifyBackgroundJobDone's own hint-time call
// (coordinator_background.go) -- release-recheck (afterRelease) and
// the 60s pass call wakeSession from a plain context.Background() with no
// way to know the debt they are about to react to came from a bg-shell
// notice, so they granted a Drain turn regardless of the config flag.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSessionDrainPolicy_BGShellOnlyDebt_AutoResumeOff_Refused is the core
// proof: a session whose ENTIRE outstanding debt is a single bg-shell-done
// notice, with AutoResumeOnJobDone off (the fixture's coordinator has no
// cfg at all, which drainPolicy treats as "autonomy not enabled" --
// the correct fail-safe default), must be refused a Drain turn exactly the
// way the direct hint-time call already refuses it.
//
// Revert-check performed: removed the `sessionDebtIsBGShellOnly` gate block
// from drainPolicy (coordinator_drain_policy.go) -- this test's
// `require.False(t, allowed)` FAILED (allowed was true: a release-recheck-
// triggered Drain would have run a full provider turn purely to react to a
// bg-shell notice the operator's config says must wait for the next natural
// turn). Restored the gate; re-ran, passed.
func TestSessionDrainPolicy_BGShellOnlyDebt_AutoResumeOff_Refused(t *testing.T) {
	coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "bgshell-only")
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID, session.NoticeKindBGShellDone, "background job finished", true, ""))

	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, allowed, "a release-recheck-triggered Drain over bg-shell-only debt must be refused when AutoResumeOnJobDone is off")
}

// TestSessionDrainPolicy_BGShellDebtPlusRealJobDebt_NotRefused is the
// regression guard: a session with a bg-shell notice ALONGSIDE real
// async-job debt must still get its ordinary turn -- the bg-shell-only gate
// must never swallow unrelated, legitimate debt just because a bg-shell
// notice happens to be mixed in.
func TestSessionDrainPolicy_BGShellDebtPlusRealJobDebt_NotRefused(t *testing.T) {
	coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "bgshell-plus-job")
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID, session.NoticeKindBGShellDone, "background job finished", true, ""))

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)

	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed, "real async-job debt alongside a bg-shell notice must still get an ordinary Drain turn")
}

// TestSessionDrainPolicy_NoDebtAtAll_NotRefusedByBGShellGate is a narrow
// guard on sessionDebtIsBGShellOnly's own empty-snapshot short-circuit: a
// session with NO debt at all must not be refused BY THIS gate (the
// existing empty-debt/no-turn path elsewhere governs it instead).
func TestSessionDrainPolicy_NoDebtAtAll_NotRefusedByBGShellGate(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "no-debt")
	require.NoError(t, err)

	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed, "an empty snapshot must never be refused by the bg-shell-only gate")
}
