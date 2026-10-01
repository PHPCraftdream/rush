// The arbiter's shadow comparison (docs/plans/2026-10-01-turn-arbiter.md
// R-ARB-1 step 2, orchestrator decision sec.9.3): decide(readTurnFacts(...))
// against the RUNNING code (drainPermitted / CLIScope) on the SAME state,
// built on a real coordinator over SQLite through existing paths. A
// divergence does NOT fail the test: it is logged and reported (R-ARB-2 must
// not land while the list is unexplained), the code stays the behavior
// oracle.
package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// shadowCase runs one fixture state and reports verdict divergences.
type shadowReport struct {
	name    string
	arbiter Verdict
	// code is drainPermitted's verdict (kind + reason) mapped onto words.
	code     string
	facts    TurnFacts
	CLIState string
}

func shadowVerdictWord(v drainVerdict) string {
	switch v.kind {
	case drainAllow:
		return "run"
	case drainPaced:
		return "deferred(paced): " + v.reason
	case drainDeferred:
		return "deferred: " + v.reason
	default:
		return "deferred(stuck): " + v.reason
	}
}

func drainStateWord(s DrainState) string {
	switch s {
	case DrainNone:
		return "None"
	case DrainOwed:
		return "Owed"
	case DrainPaced:
		return "Paced"
	case DrainDeferred:
		return "Deferred"
	default:
		return "Stuck"
	}
}

func shadowArbiterWord(v Verdict) string {
	switch v.Kind {
	case VRun:
		return "run"
	case VNone:
		return "none: " + v.Reason
	case VClose:
		return "close: " + v.Reason
	default:
		if v.Reason == "retry pause after an unreacted attempt" || v.Reason == "rerun in progress" {
			return "deferred(paced): " + v.Reason
		}
		if v.Reason == "repeated unreacted attempts" {
			return "deferred(stuck): " + v.Reason
		}
		return "deferred: " + v.Reason
	}
}

// checkShadow compares one state: facts -> decide(site) vs drainPermitted
// (spent per site) and, when asked, vs CLIScope (siteCLI). Returns the
// report; the caller logs divergences, never fails.
func checkShadow(ctx context.Context, t *testing.T, f *attemptFixture, sessionID string, site LaunchSite, name string, compareCLI bool) shadowReport {
	t.Helper()
	facts, err := f.coord.readTurnFacts(ctx, sessionID, site)
	require.NoError(t, err)
	av := decide(facts)
	spent := site == siteFact
	cv := f.coord.drainPermitted(ctx, sessionID, spent)
	rep := shadowReport{name: name, arbiter: av, code: shadowVerdictWord(cv), facts: facts}
	if compareCLI {
		state, err := f.coord.CLIScope(ctx, sessionID)
		require.NoError(t, err)
		rep.CLIState = drainStateWord(state.Drain)
	}
	if sameShadow(rep) && (!compareCLI || cliMatches(rep)) {
		return rep
	}
	t.Logf("SHADOW divergence %q: arbiter=%q code=%q", name, shadowArbiterWord(av), rep.code)
	if compareCLI {
		t.Logf("SHADOW divergence %q: CLIScope=%s arbiter(CLI)=%q", name, rep.CLIState, shadowArbiterWord(decide(mustFacts(ctx, t, f, sessionID, siteCLI))))
	}
	return rep
}

func mustFacts(ctx context.Context, t *testing.T, f *attemptFixture, sessionID string, site LaunchSite) TurnFacts {
	t.Helper()
	facts, err := f.coord.readTurnFacts(ctx, sessionID, site)
	require.NoError(t, err)
	return facts
}

func sameShadow(r shadowReport) bool {
	return shadowArbiterWord(r.arbiter) == r.code
}

func cliMatches(r shadowReport) bool {
	switch r.CLIState {
	case "Owed":
		return shadowArbiterWord(r.arbiter) == "run"
	case "Paced":
		return shadowArbiterWord(r.arbiter) == "deferred(paced): "+r.arbiter.Reason
	case "Stuck":
		return r.arbiter.Reason == "repeated unreacted attempts"
	case "Deferred":
		return shadowArbiterWord(r.arbiter) == "deferred: "+r.arbiter.Reason
	default:
		return r.CLIState == "None" && r.arbiter.Kind == VNone
	}
}

// newShadowFixture is a plain root session on a real coordinator: no driver
// registry, no idle hook; cfg is nil, so autonomy reads as off (rule 12
// territory) unless a test opts in by config.
func newShadowFixture(t *testing.T, title string) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t, title, attemptFixtureOpts{noIdle: true, noDriver: true})
	return f
}

// newShadowDelegationFixture is a parent session with a delegated child; the
// child's delegation row is RUNNING, or terminal (released) when running is
// false. The child keeps its durable delegation identity either way.
func newShadowDelegationFixture(t *testing.T, running bool) (*attemptFixture, string, string) {
	t.Helper()
	f, parentID, childID, _ := childDelegationBase(t, attemptFixtureOpts{noIdle: true})
	if !running {
		_, err := f.store.Transition(context.Background(), session.TransitionParams{
			Owner: parentID, ToolCallID: "delegate-1", State: "completed", ResultSummary: "done", Wake: true,
		})
		require.NoError(t, err)
	}
	return f, parentID, childID
}

// TestShadow_OpenGatePlainDebt: debt, open gate, no refusals -- both answers
// are run.
func TestShadow_OpenGatePlainDebt(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-open")
	f.seedDebt(ctx, "shadow-open-1", true)
	rep := checkShadow(ctx, t, f, f.sessID, siteFact, "open gate, fact", true)
	require.Equal(t, "run", rep.code, "fixture sanity: the running code admits this debt")
	require.Equal(t, VRun, rep.arbiter.Kind)
	require.True(t, rep.arbiter.Counted)
}

// TestShadow_PacedGate: one unreacted paid attempt paces the gate for both.
func TestShadow_PacedGate(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-paced")
	f.seedDebt(ctx, "shadow-paced-1", true)
	f.ledger.paceDrainGate(f.sessID, f.ledger.hintSeqOf(f.sessID), time.Minute, true, pacePaidUnreacted)
	rep := checkShadow(ctx, t, f, f.sessID, siteTick, "paced gate", true)
	require.Equal(t, VDefer, rep.arbiter.Kind)
	require.Contains(t, shadowArbiterWord(rep.arbiter), "paced")
	require.Contains(t, rep.code, "paced")
	require.Equal(t, "Paced", rep.CLIState)
}

// TestShadow_PaidDormantGate: D paid unreacted attempts -- stuck for both;
// a newer fact hint does not reopen either.
func TestShadow_PaidDormantGate(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-paid-dormant")
	f.seedDebt(ctx, "shadow-paid-dormant-1", true)
	for range turnDormantStreak {
		f.ledger.paceDrainGate(f.sessID, f.ledger.hintSeqOf(f.sessID), time.Minute, false, pacePaidUnreacted)
	}
	f.ledger.bumpHint(f.sessID)
	rep := checkShadow(ctx, t, f, f.sessID, siteFact, "paid-dormant gate", true)
	require.Equal(t, "deferred(stuck): repeated unreacted attempts", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	require.Equal(t, "Stuck", rep.CLIState)
}

// TestShadow_FreeDormantGate: a failing pull's free dormancy; a newer fact
// reopens both (the code's the arbiter gate rows and the arbiter's rule 10).
func TestShadow_FreeDormantGateHintOpens(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-free-dormant")
	f.seedDebt(ctx, "shadow-free-dormant-1", true)
	for range turnDormantStreak {
		f.ledger.paceDrainGate(f.sessID, f.ledger.hintSeqOf(f.sessID), time.Minute, true, paceFreeNoTurn)
	}
	// Without a newer fact: stuck on both sides.
	rep := checkShadow(ctx, t, f, f.sessID, siteTick, "free-dormant, no hint", true)
	require.Equal(t, "deferred(stuck): repeated unreacted attempts", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	// With one: open on both sides.
	f.ledger.bumpHint(f.sessID)
	rep = checkShadow(ctx, t, f, f.sessID, siteTick, "free-dormant, newer hint", true)
	require.Equal(t, "run", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
}

// TestShadow_Suspended: Stop's suspension defers both, without a tick.
func TestShadow_Suspended(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-suspended")
	f.seedDebt(ctx, "shadow-suspended-1", true)
	f.coord.suspendAutoResume(f.sessID)
	rep := checkShadow(ctx, t, f, f.sessID, siteRelease, "suspended", true)
	require.Equal(t, "deferred: automatic turns suspended", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
}

// TestShadow_HeldByRerun: a hold reads paced (no clock) on both sides.
func TestShadow_HeldByRerun(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-held")
	f.seedDebt(ctx, "shadow-held-1", true)
	release := f.coord.HoldAutomaticTurns(f.sessID)
	defer release()
	rep := checkShadow(ctx, t, f, f.sessID, siteTick, "held by rerun", true)
	require.Equal(t, "deferred(paced): rerun in progress", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
}

// TestShadow_ChainGuard: N idle links owning the whole job-debt defer both;
// an extra foreign claim breaks the guard on both.
func TestShadow_ChainGuard(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-chain")
	f.seedDebt(ctx, "shadow-chain-claim", false) // pending job debt
	claim := f.row(ctx, "shadow-chain-claim").ClaimID
	require.NotEmpty(t, claim)
	f.coord.setReactionChain(f.sessID, turnReactionChainLimit, map[string]struct{}{claim: {}}, false)
	// The running code's guard inserts a marker notice for a non-CLI session
	// on the FIRST deferred read; pre-mark it so the comparison stays
	// read-only (the marker insert is the executor's job).
	f.coord.seedArbiterState(f.sessID, func(s *arbiterState) { s.chainNoticed = true })
	rep := checkShadow(ctx, t, f, f.sessID, siteFact, "chain guard fires", true)
	require.Equal(t, "deferred: "+reactionChainReason, rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	// A debt row outside the chain's claims: a real fact -- run on both.
	f.seedDebt(ctx, "shadow-chain-real", true)
	rep = checkShadow(ctx, t, f, f.sessID, siteFact, "chain guard, foreign claim", true)
	require.Equal(t, "run", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
}

// TestShadow_BGShellOnlyAutoResumeOff: cfg nil -> autonomy off; a bg-shell
// notice debt is deferred by both, a job debt next to it runs.
func TestShadow_BGShellOnlyAutoResumeOff(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-bgoff")
	_, err := f.store.InsertSessionNoticeReturningID(ctx, f.sessID, session.NoticeKindBGShellDone, "done", true, "")
	require.NoError(t, err)
	rep := checkShadow(ctx, t, f, f.sessID, siteTick, "bg-shell only, autonomy off", true)
	require.Contains(t, rep.code, "auto-resume off")
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
}

// TestShadow_OverCapBGShellDebt: the cap spent, the whole debt over-cap --
// deferred by both for a no-slot launch, run for a fact.
func TestShadow_OverCapBGShellDebt(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-overcap")
	// The cap rules only exist with AutoResumeOnJobDone on (a real config,
	// isolated global paths).
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(f.env.workingDir, "", false)
	require.NoError(t, err)
	f.coord.cfg = cfg
	cfg.Config().Options.AutoResumeOnJobDone = boolPtr(true)
	const n = 3
	ids := make([]int64, 0, n)
	for i := range n {
		id, err := f.store.InsertSessionNoticeReturningID(ctx, f.sessID, session.NoticeKindBGShellDone, "done", true, "")
		require.NoError(t, err)
		ids = append(ids, id)
		_ = i
	}
	f.coord.setOverCap(f.sessID, turnAutoResumeCap, ids)
	rep := checkShadow(ctx, t, f, f.sessID, siteRelease, "over-cap re-check", true)
	require.Contains(t, rep.code, "cap reached")
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	// The completion's own launch never compares the cap.
	facts := mustFacts(ctx, t, f, f.sessID, siteFact)
	require.Equal(t, VRun, decide(facts).Kind)
	cv := f.coord.drainPermitted(ctx, f.sessID, true)
	require.Equal(t, drainAllow, cv.kind)
}

// TestShadow_ForeignLiveDriver: a second live host (its own store, its own
// OS lock in the same data dir) drives the session -- both refuse.
func TestShadow_ForeignLiveDriver(t *testing.T) {
	ctx := context.Background()
	f := newShadowFixture(t, "shadow-foreign")
	f.seedDebt(ctx, "shadow-foreign-1", true)
	other := session.NewAsyncJobStore(f.env.conn, f.env.workingDir, os.Getpid()+7777, "shadow-other-host")
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	require.NoError(t, other.ClaimSessionDriver(ctx, f.sessID))
	rep := checkShadow(ctx, t, f, f.sessID, siteFact, "foreign live driver", false)
	require.Contains(t, rep.code, "another process drives")
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	require.True(t, rep.facts.Session.ForeignDriverLive)
}

// TestShadow_RunningDelegationChild (step-0 gap, R-ARB-2): a child whose
// delegation row runs is driven by that delegation -- neither the
// released-child refusal nor the bg-shell policy rows apply; both sides run.
func TestShadow_RunningDelegationChild(t *testing.T) {
	ctx := context.Background()
	f, _, childID := newShadowDelegationFixture(t, true)
	f.seedDebtFor(ctx, childID, "shadow-run-child-1", true)
	rep := checkShadow(ctx, t, f, childID, siteFact, "running delegation child", true)
	require.Equal(t, "run", rep.code, "fixture sanity: the running code admits the child")
	require.Equal(t, VRun, rep.arbiter.Kind)
	require.True(t, rep.facts.Session.RunningDelegation)
	require.True(t, rep.facts.Session.DurableChild, "durable identity is set; rule 7 is skipped only by RunningDelegation")
}

// TestShadow_ReleasedDelegationChild (step-0 gap): a released child with debt
// never falls through to the root agent -- deferred on both sides.
func TestShadow_ReleasedDelegationChild(t *testing.T) {
	ctx := context.Background()
	f, _, childID := newShadowDelegationFixture(t, false)
	f.seedDebtFor(ctx, childID, "shadow-rel-child-1", true)
	rep := checkShadow(ctx, t, f, childID, siteRelease, "released delegation child", true)
	require.Equal(t, "deferred: released delegation child", rep.code)
	require.Equal(t, rep.code, shadowArbiterWord(rep.arbiter))
	require.True(t, rep.facts.Session.DurableChild)
	require.False(t, rep.facts.Session.RunningDelegation)
}
