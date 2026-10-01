// Table tests for the turn arbiter's pure functions (docs/plans/2026-10-01-
// turn-arbiter.md sec.3): every rule row is a case, plus the finding cases
// from sec.1's "Находки" column and the orchestrator's corrected order
// (sec.9.1). The rule ORDER is contract: the two overlap cases at the bottom
// go red if 5/6 or 6/8 are swapped.
package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func baseFacts() TurnFacts {
	return TurnFacts{
		Now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Debt: DebtFacts{
			PendingIncl: true,
			Visible:     session.DebtSnapshot{Notices: []session.DebtNoticeRef{{ID: 1}}},
		},
		Session: SessionFacts{AutonomyEnabled: true},
	}
}

func TestDecide_Rule1_NoDebtIsNone(t *testing.T) {
	f := baseFacts()
	f.Site = siteTick
	f.Debt.PendingIncl = false
	v := decide(f)
	require.Equal(t, VNone, v.Kind)
	// The same snapshot at the accounting side is NOT rule 1: it decides
	// from the attempt facts.
	f.Site = siteAccount
	require.Equal(t, VNone, decide(f).Kind, "no attempt facts: nothing to account")
}

func TestDecide_Rule2_ForeignDriver(t *testing.T) {
	f := baseFacts()
	f.Site = siteFact
	f.Session.ForeignDriverLive = true
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "another process drives the session", v.Reason)
	// A session this process drives itself is not refused (DUR-10).
	f.Session.ExternallyDrivenSelf = true
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule3_HeldIsPacedNotDeferred(t *testing.T) {
	// R3C-5: a hold is temporary -- every scope consumer must read it as
	// still open, so it is a defer with a zero RecheckAt (the tick and the
	// release retry it), never a "droppable" verdict.
	f := baseFacts()
	f.Site = siteTick
	f.Session.Held = true
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "rerun in progress", v.Reason)
	require.True(t, v.RecheckAt.IsZero())
	require.True(t, v.ReopenOnHint)
}

func TestDecide_Rule4_Suspended(t *testing.T) {
	// R2B-1(B): Stop and a pending question suspend until a human message;
	// not worth a tick (recheck=false in today's verdict).
	f := baseFacts()
	f.Site = siteRelease
	f.Session.Suspended = true
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "automatic turns suspended", v.Reason)
	require.True(t, v.RecheckAt.IsZero())
	require.False(t, v.ReopenOnHint)
}

func TestDecide_Rule5_ChainGuardAboveRunningDelegation(t *testing.T) {
	// #1113, R6B-1: the chain guard fires BEFORE the running-delegation
	// shortcut: a child is freed from the auto-resume policy, not from the
	// guard.
	f := baseFacts()
	f.Site = siteFact
	f.Session.ChainLinks = turnReactionChainLimit
	f.Session.ChainOwnsAllDebt = true
	f.Session.RunningDelegation = true
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, reactionChainReason, v.Reason)
	// Below N links, or a debt that carries a real fact, the guard is quiet.
	f.Session.ChainLinks = turnReactionChainLimit - 1
	require.Equal(t, VRun, decide(f).Kind)
	f.Session.ChainLinks = turnReactionChainLimit
	f.Session.ChainOwnsAllDebt = false
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rules7and11_ChildUnderRunningDelegation(t *testing.T) {
	// R6B-1: a child whose delegation row runs is driven by the delegation:
	// neither the released-child refusal, nor the cap (11), nor
	// auto-resume-off (12) applies to it.
	f := baseFacts()
	f.Site = siteTick
	f.Session.DurableChild = true
	f.Session.RunningDelegation = true
	f.Session.AutonomyEnabled = false
	f.Debt.BGShellOnly = true
	f.Debt.BGShellNotices = 2
	f.Debt.OverCapRows = 2
	require.Equal(t, VRun, decide(f).Kind)
	// sec.9.1 MANDATORY case: the same child INSIDE the paid pause is
	// paced, not run -- the gate applies to every session. Revert-check:
	// putting the old rule 6 (running delegation -> VRun) before rule 8
	// turns this red.
	f.Gate.PaidStreak = turnDormantStreak
	f.Gate.RetryAt = f.Now.Add(time.Minute)
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "repeated unreacted attempts", v.Reason)
	require.Equal(t, f.Gate.RetryAt, v.RecheckAt)
	require.False(t, v.ReopenOnHint)
	// Without a running delegation the released-child refusal applies (B3/C6):
	// a durable child never falls through to the root agent.
	f.Session.RunningDelegation = false
	f.Gate = GateFacts{}
	v = decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "released delegation child", v.Reason)
	// A session this process's own rush run loop drives runs on
	// currentAgent anyway.
	f.Session.ExternallyDrivenSelf = true
	f.Debt.BGShellOnly = false
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule8_PaidDormantGate(t *testing.T) {
	// R3B-4: a newer fact does NOT shorten the pause of a paid streak; only
	// a human message or a restart reopens it.
	f := baseFacts()
	f.Site = siteFact
	f.Gate.PaidStreak = turnDormantStreak
	f.Gate.RetryAt = f.Now.Add(time.Minute)
	f.Gate.HintOpens = false
	f.Gate.HintNow = f.Gate.HintSeen + 1 // a fresh fact arrives: still shut
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "repeated unreacted attempts", v.Reason)
	require.False(t, v.ReopenOnHint)
}

func TestDecide_Rule9_PacedGateHintOpensEarly(t *testing.T) {
	f := baseFacts()
	f.Site = siteFact
	f.Gate.Paced = true
	f.Gate.RetryAt = f.Now.Add(time.Minute)
	f.Gate.HintOpens = true
	f.Gate.HintSeen = 5
	f.Gate.HintNow = 5
	// No newer fact: paced at RetryAt.
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "retry pause after an unreacted attempt", v.Reason)
	require.Equal(t, f.Gate.RetryAt, v.RecheckAt)
	require.True(t, v.ReopenOnHint)
	// A newer fact (R2B-1: only a fact bumps the hint) opens the gate early.
	f.Gate.HintNow = 6
	require.Equal(t, VRun, decide(f).Kind)
	// After the pause passed the gate is open whatever the hint says.
	f.Gate.HintNow = 5
	f.Gate.RetryAt = f.Now.Add(-time.Second)
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule9_PaidFailureDoesNotReopenOnHint(t *testing.T) {
	// R3B-4: a paid streak inside (but below) dormancy: a hint does not
	// restart the retry clock. hintOpens was recorded false by the pacing.
	f := baseFacts()
	f.Site = siteFact
	f.Gate.Paced = true
	f.Gate.PaidStreak = 1
	f.Gate.RetryAt = f.Now.Add(time.Minute)
	f.Gate.HintOpens = false
	f.Gate.HintNow = f.Gate.HintSeen + 1
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.True(t, v.RecheckAt.Equal(f.Gate.RetryAt))
	require.False(t, v.ReopenOnHint)
}

func TestDecide_Rule10_FreeDormantGate(t *testing.T) {
	// R3B-4: a failing pull's free dormancy is reopened by a newer fact.
	f := baseFacts()
	f.Site = siteTick
	f.Gate.FreeStreak = turnDormantStreak
	f.Gate.RetryAt = f.Now.Add(time.Minute)
	f.Gate.HintOpens = true
	f.Gate.HintSeen = 5
	f.Gate.HintNow = 5
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, "repeated unreacted attempts", v.Reason)
	require.True(t, v.ReopenOnHint, "a free-dormant gate stays reopenable by a fact")
	f.Gate.HintNow = 6
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule11_BGShellCapDeferred(t *testing.T) {
	// R3B-6/(aa): a launch that spends no slot defers a debt every row of
	// which is an over-cap completion; a fact never compares (its slot was
	// spent at admission); a row holding a slot breaks the defer.
	f := baseFacts()
	f.Site = siteRelease
	f.Debt.BGShellOnly = true
	f.Debt.BGShellNotices = 3
	f.Debt.OverCapRows = 3
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Contains(t, v.Reason, "cap reached")
	// A fact launch never compares the cap.
	f.Site = siteFact
	require.Equal(t, VRun, decide(f).Kind)
	// One row holds a slot: retried like any debt.
	f.Site = siteRelease
	f.Debt.OverCapRows = 2
	require.Equal(t, VRun, decide(f).Kind)
	// The cap row is not bg-shell-only (a real fact sits next to it): run.
	f.Debt.OverCapRows = 3
	f.Debt.BGShellOnly = false
	require.Equal(t, VRun, decide(f).Kind)
	// A delegation child under a running delegation is outside the cap.
	f.Session.RunningDelegation = true
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule12_BGShellOnlyAutoResumeOff(t *testing.T) {
	f := baseFacts()
	f.Site = siteTick
	f.Session.AutonomyEnabled = false
	f.Debt.BGShellOnly = true
	v := decide(f)
	require.Equal(t, VDefer, v.Kind)
	require.Contains(t, v.Reason, "auto-resume off")
	// Mixed debt (a real fact next to the bg-shell completion) runs even
	// with autonomy off.
	f.Debt.BGShellOnly = false
	require.Equal(t, VRun, decide(f).Kind)
	// A child under a running delegation skips row 12 (sec.9.1).
	f.Debt.BGShellOnly = true
	f.Session.RunningDelegation = true
	require.Equal(t, VRun, decide(f).Kind)
}

func TestDecide_Rule13_CountedOnlyAtAFact(t *testing.T) {
	f := baseFacts()
	f.Site = siteFact
	require.Equal(t, VRun, decide(f).Kind)
	require.True(t, decide(f).Counted)
	for _, site := range []LaunchSite{siteRelease, siteTick, siteCommit, siteCLI, siteChild} {
		f.Site = site
		v := decide(f)
		require.Equal(t, VRun, v.Kind, "site %d", site)
		require.False(t, v.Counted, "site %d must not spend a slot", site)
	}
}

func TestDecideAccount_RowsA1toA7(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(a AttemptFacts) TurnFacts {
		f := baseFacts()
		f.Site = siteAccount
		f.Attempt = a
		f.Attempt.OpenRows = 1
		return f
	}
	// A1 refused: paced, not counted; a CLI-driven session paces at the
	// loop's cadence.
	v := decide(at(AttemptFacts{NotAttempted: true, Refused: true}))
	require.Equal(t, VDefer, v.Kind)
	require.True(t, v.ReopenOnHint)
	require.Equal(t, now.Add(turnRetryAfterFailure()), v.RecheckAt)
	f := at(AttemptFacts{NotAttempted: true, Refused: true})
	f.Session.ExternallyDrivenSelf = true
	v = decide(f)
	require.Equal(t, now.Add(drainRefusalPauseLoop()), v.RecheckAt)
	// A1 cancelled preamble: nothing.
	require.Equal(t, VNone, decide(at(AttemptFacts{NotAttempted: true})).Kind)
	// A2: a no-turn Drain that left pending debt (the pull keeps failing).
	v = decide(at(AttemptFacts{NoTurn: true, PendingLeft: true}))
	require.Equal(t, VDefer, v.Kind)
	require.True(t, v.ReopenOnHint)
	require.Equal(t, now.Add(turnRetryAfterFailure()), v.RecheckAt)
	// A3: a no-turn Drain with no debt left: the gate resets.
	require.Equal(t, VNone, decide(at(AttemptFacts{NoTurn: true})).Kind)
	// A4: an operator stop is no evidence.
	require.Equal(t, VNone, decide(at(AttemptFacts{Exempt: true})).Kind)
	// A5: every visible row reacted.
	require.Equal(t, VNone, decide(at(AttemptFacts{MaxWakeAttempts: 0})).Kind)
	// A6: a row reached K (or a terminal failure closes the rows).
	a := at(AttemptFacts{MaxWakeAttempts: turnFailureSettleThreshold, AtK: session.DebtSnapshot{Notices: []session.DebtNoticeRef{{ID: 7}}}})
	v = decide(a)
	require.Equal(t, VClose, v.Kind)
	require.Equal(t, a.Attempt.AtK, v.Rows)
	// A7: a counted unreacted attempt paces at R; a fact does not open early.
	v = decide(at(AttemptFacts{MaxWakeAttempts: 2}))
	require.Equal(t, VDefer, v.Kind)
	require.Equal(t, now.Add(turnRetryAfterFailure()), v.RecheckAt)
	require.False(t, v.ReopenOnHint)
}

func TestDecide_AccountSideNeverIssuesCloseOnLaunchSites(t *testing.T) {
	// sec.2.2: VClose is structurally unreachable from a launch site -- the
	// site is part of the facts.
	for _, site := range []LaunchSite{siteFact, siteRelease, siteTick, siteCommit, siteCLI, siteChild} {
		f := baseFacts()
		f.Site = site
		f.Attempt = AttemptFacts{
			MaxWakeAttempts: turnFailureSettleThreshold,
			AtK:             session.DebtSnapshot{Notices: []session.DebtNoticeRef{{ID: 1}}},
		}
		require.NotEqual(t, VClose, decide(f).Kind, "site %d", site)
	}
}

func TestTurnArbiter_ConstantsReuseTheOwningValues(t *testing.T) {
	// The named constants must never drift from their owners.
	require.Equal(t, 3, turnFailureSettleThreshold)
	require.Equal(t, 3, turnReactionChainLimit)
	require.Equal(t, 3, turnDormantStreak)
	require.Equal(t, 5, turnAutoResumeCap)
}
