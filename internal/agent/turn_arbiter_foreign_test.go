// The WS-1 fact in the turn arbiter (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1 "drain/reaction"): a session
// that belongs to another workspace may not be driven here, so the arbiter
// defers its debt WITHOUT settling it and WITHOUT a recheck. Pure-function
// tests, like the rest of turn_arbiter_test.go; the two fixture-backed ones at
// the bottom walk the fact through readTurnFacts and the accounting.

package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDecide_ForeignWorkspaceDefersWithoutRecheck: the launch side's WS-1
// rule. A foreign session's debt is not this process's to react to, so the
// verdict is a VDefer that carries no RecheckAt (there is nothing to wait for
// -- the owner reacts in its own checkout) and that drainVerdictOf maps to a
// defer WITHOUT a tick.
//
// Revert-check: deleting the `if f.Session.ForeignWorkspace` block at the top
// of decide (internal/agent/turn_arbiter.go) makes the VDefer assertions
// below fail: the snapshot takes rule 1's ordinary path and runs.
func TestDecide_ForeignWorkspaceDefersWithoutRecheck(t *testing.T) {
	f := baseFacts()
	f.Site = siteFact
	f.Session.ForeignWorkspace = true

	v := decide(f)
	require.Equal(t, VDefer, v.Kind, "a foreign session must never be launched for")
	require.Equal(t, foreignWorkspaceReason, v.Reason)
	require.True(t, v.RecheckAt.IsZero(), "nothing to recheck: the debt is the owner's")
	require.False(t, v.ReopenOnHint)
	require.Empty(t, v.Rows.Jobs)
	require.Empty(t, v.Rows.Notices, "a defer settles nothing")

	// Every launch site, not just siteFact.
	for _, site := range []LaunchSite{siteTick, siteRelease, siteCommit, siteCLI, siteChild} {
		f.Site = site
		require.Equal(t, VDefer, decide(f).Kind, "site %d must also defer", site)
	}

	// The adapter the launchers read must not ask for a tick: "recheck" is
	// what turns a defer into a busy loop, and the reason here is permanent.
	dv := drainVerdictOf(v)
	require.False(t, dv.recheck, "a foreign workspace must not be worth a tick")
	require.Equal(t, drainDeferred, dv.kind)

	// And it is not the paced/stuck kinds: both imply OUR gate is the problem,
	// which it is not.
	require.NotEqual(t, drainPaced, dv.kind)
	require.NotEqual(t, drainStuck, dv.kind)
}

// TestDecide_ForeignWorkspaceBeatsEveryOtherRule: ownership is the
// precondition for having an opinion about the debt at all, so the fact is
// checked before rules 1-13 -- a foreign session with a live foreign driver, a
// hold, a suspension, a spent cap and a dormant gate all answer the same way.
//
// Revert-check: moving the check below rule 1 (or dropping it) makes the
// no-debt case answer VNone and the held case answer "rerun in progress".
func TestDecide_ForeignWorkspaceBeatsEveryOtherRule(t *testing.T) {
	f := baseFacts()
	f.Site = siteTick
	f.Session.ForeignWorkspace = true
	f.Debt.PendingIncl = false
	require.Equal(t, foreignWorkspaceReason, decide(f).Reason,
		"ownership outranks rule 1's no-debt short-circuit")

	f = baseFacts()
	f.Site = siteTick
	f.Session.ForeignWorkspace = true
	f.Session.ForeignDriverLive = true
	f.Session.Held = true
	f.Session.Suspended = true
	f.Debt.BGShellOnly = true
	f.Gate.FreeStreak = turnDormantStreak
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, foreignWorkspaceReason, v.Reason,
		"no other rule may pre-empt the ownership refusal")
}

// TestDecideAccount_ForeignWorkspaceDoesNotPaceOrSettle: the accounting side.
// A leg refused because the session is foreign is not evidence about the debt
// (the debt is answered in its owner's checkout), so it must fall into
// neither A1's pacing nor A6/A7's settle/pace rows.
//
// Revert-check: deleting the WS-1 block at the top of decideAccount
// (internal/agent/turn_arbiter.go) makes the NotAttempted+Refused case answer
// "launch refused" WITH a RecheckAt, which the assertions below reject.
func TestDecideAccount_ForeignWorkspaceDoesNotPaceOrSettle(t *testing.T) {
	f := baseFacts()
	f.Site = siteAccount
	f.Session.ForeignWorkspace = true
	f.Attempt = AttemptFacts{NotAttempted: true, Refused: true}

	v := decideAccount(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, foreignWorkspaceReason, v.Reason)
	require.NotEqual(t, "launch refused", v.Reason,
		"a foreign refusal is not a launch refusal worth pacing our own gate for")
	require.True(t, v.RecheckAt.IsZero(), "no recheck: the debt is the owner's")
	require.Empty(t, v.Rows.Jobs)
	require.Empty(t, v.Rows.Notices, "a defer settles nothing")
}

// TestReadTurnFacts_StampsForeignWorkspaceFromTheRunError: readTurnFacts is
// where the fact enters the snapshot, from the run/turn error the caller
// brings in -- the DB half of the facts holds no workspace column to read it
// from.
//
// Revert-check: removing the errors.Is(runErr, session.ErrForeignWorkspace)
// stamping in readTurnFacts (internal/agent/turn_arbiter.go) fails the foreign
// sub-case: the fact stays false and the verdict becomes a run.
func TestReadTurnFacts_StampsForeignWorkspaceFromTheRunError(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "ws-facts-stamping", attemptFixtureOpts{noIdle: true})

	// A foreign refusal, wrapped exactly the way runOwned's guard returns it.
	wrapped := &DrainNotAttemptedError{Err: session.ErrForeignWorkspace}

	facts, err := f.coord.readTurnFacts(ctx, f.sessID, siteTick, wrapped)
	require.NoError(t, err)
	require.True(t, facts.Session.ForeignWorkspace,
		"the run error must stamp the WS-1 fact into the snapshot")
	require.Equal(t, VDefer, decide(facts).Kind)
	require.Equal(t, foreignWorkspaceReason, decide(facts).Reason)

	facts, err = f.coord.readTurnFacts(ctx, f.sessID, siteTick)
	require.NoError(t, err)
	require.False(t, facts.Session.ForeignWorkspace,
		"a caller with no run error in hand must get exactly the pre-#1142 snapshot")
}

// TestAccountDrainAttempt_ForeignLegLeavesTheDebtAlone: the production path of
// the fact, end to end. A Drain leg whose error is the WS-1 refusal is
// accounted as a foreign defer: the gate is not paced, no recheck is queued,
// and the visible debt is left for its owner instead of being settled by
// failure.
//
// Revert-check (fact): removing the ForeignWorkspace stamping in
// accountDrainAttempt (internal/agent/drain_attempt.go) makes the leg an
// ordinary refused launch: the gate is paced and the recheck set is used, so
// the assertions below go red.
// Revert-check (rule): deleting the WS-1 case in executeAccountVerdict
// (internal/agent/drain_attempt.go) drops the leg into A7's generic VDefer,
// which paces the gate too.
func TestAccountDrainAttempt_ForeignLegLeavesTheDebtAlone(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "ws-foreign-leg", attemptFixtureOpts{noIdle: true})

	// Visible debt for this session: it must survive the foreign leg.
	f.seedDebt(ctx, "ws-foreign-call", true)

	// A refused leg, exactly the shape runOwned's guard produces: the call
	// never reached the provider, and the refusal names another workspace.
	att := &drainAttempt{sessionID: f.sessID, outcome: drainNotAttempted}
	turnErr := &DrainNotAttemptedError{Err: session.ErrForeignWorkspace}

	f.coord.accountDrainAttempt(ctx, att, turnErr)

	gate := f.coord.arb.snapshot(f.sessID).Gate
	require.True(t, gate.RetryAt.IsZero(),
		"a foreign refusal must not pace the gate: it says nothing about our own launch cadence")
	require.Zero(t, gate.FreeStreak, "a foreign refusal is not a free-no-turn attempt")
	require.Zero(t, gate.PaidStreak, "a foreign refusal is not a paid unreacted attempt")
	require.False(t, f.inRecheckSet(), "a foreign workspace is not worth a tick")

	// The debt is untouched, so the owner process still sees it.
	hasJobDebt, notices, _, err := f.store.PendingInclusiveDebtRows(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, hasJobDebt || len(notices) > 0,
		"a foreign leg must not settle the debt by failure")
	require.Zero(t, f.markers(ctx), "a foreign leg writes no wake_failed marker")
}
