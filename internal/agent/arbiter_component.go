// The turn arbiter (R-ARB-1, docs/plans/2026-10-01-turn-arbiter.md): TurnFacts,
// Verdict and the pure decide function. This step does NOT move callers:
// drainPermitted/decideDrainTurn/CLIScope stay the running code (R-ARB-2
// translates them); readTurnFacts is the shadow's fact collector.

package agent

import (
	"context"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/session"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
)

type (
	LaunchSite   = drainctl.LaunchSite
	SessionFacts = drainctl.SessionFacts
	DebtFacts    = drainctl.DebtFacts
	GateFacts    = drainctl.GateFacts
	AttemptFacts = drainctl.AttemptFacts
	TurnFacts    = drainctl.TurnFacts
	VerdictKind  = drainctl.VerdictKind
	Verdict      = drainctl.Verdict

	arbiterSnapshot = drainctl.ArbiterSnapshot
)

const (
	siteFact    = drainctl.SiteFact
	siteRelease = drainctl.SiteRelease
	siteTick    = drainctl.SiteTick
	siteCommit  = drainctl.SiteCommit
	siteCLI     = drainctl.SiteCLI
	siteChild   = drainctl.SiteChild
	siteAccount = drainctl.SiteAccount

	VRun   = drainctl.VRun
	VDefer = drainctl.VDefer
	VClose = drainctl.VClose
	VNone  = drainctl.VNone
)

// The arbiter's named constants: K closes a debt row by its own counted
// attempts, N bounds the idle reaction chain, D is either dormancy streak of
// the launch gate, CAP bounds bg-shell auto-resumes per human message and R
// paces the next launch after an unreacted attempt. Re-exported from their
// owning files so the values never diverge.
const (
	turnFailureSettleThreshold = drainctl.FailureSettleThreshold // K
	turnReactionChainLimit     = ReactionChainLimit              // N
	turnDormantStreak          = drainctl.DormantStreak          // D
	turnAutoResumeCap          = drainctl.AutoResumeCap          // CAP
)

const foreignWorkspaceReason = drainctl.ForeignWorkspaceReason

// turnRetryAfterFailure is R: the launch-gate pause after an unreacted
// attempt.
func turnRetryAfterFailure() time.Duration { return drainRetryAfterFailure() }

func decide(f TurnFacts) Verdict {
	return drainctl.Decide(f, drainRetryAfterFailure, drainRefusalPauseLoop)
}

func decideAccount(f TurnFacts) Verdict {
	return drainctl.DecideAccount(f, drainRetryAfterFailure, drainRefusalPauseLoop)
}

// readTurnFacts collects the ONE snapshot the arbiter decides on (design
// sec.2.1). Order is law (ASYNC-02/09 anchor): the DB half FIRST, then the
// in-process half -- a fact committed between the reads is already durable
// when the memory half is captured, so it is seen; the inverted order could
// lose it. No DB I/O happens under the arbiter's or the ledger's mutex: each
// is captured once and copied to a value. The bgArrival mutex is taken
// BEFORE the debt-rows read and spans the in-process half -- exactly the
// completion arrival's own span (insert + slot decision under the same
// gate) -- so the debt rows and the over-cap set are ONE consistent pair:
// a completing background shell is either fully visible to both or to
// neither. The read never writes the set; pruning happens only at a
// completion's arrival (pruneBGShellOverCap, under bgArrival). The rows are
// the ONE PendingInclusiveDebtRows read: the debt's existence, summary,
// kinds, over-cap matching and the chain guard's claims all come from it.
// An unreadable input is an error: the caller fails closed.
//
// runErrs is optional and carries the caller's knowledge of this session's
// run/turn leg, which the DB half below cannot see: when a leg was refused
// because the session belongs to another workspace (WS-1, #1142 step C), the
// ForeignWorkspace fact is stamped into the snapshot. Callers with no leg
// error in hand (every launch site, every existing test) pass none and get
// exactly the pre-#1142 snapshot.
// ForeignWorkspace fact is stamped into the snapshot. Callers with no leg
// error in hand (every launch site, every existing test) pass none and get
// exactly the pre-#1142 snapshot.
//
// The DB half is the batch node read (readTurnFactsBatch over the bound
// BatchCutoverFacts); the in-process half reads the rehomed coordinator
// runtime state (hint counter, external-driver mirror). The bgArrival mutex
// is taken BEFORE the debt-rows read and spans the in-process half -- the
// completion arrival's own span -- so the debt rows and the over-cap set are
// ONE consistent pair. The read never writes the set; pruning happens only
// at a completion's arrival (pruneBGShellOverCap, under bgArrival). An
// unreadable input is an error: the caller fails closed.
func (c *coordinator) readTurnFacts(ctx context.Context, sessionID string, site LaunchSite, runErrs ...error) (TurnFacts, error) {
	return newDrainComponent(c).readTurnFacts(ctx, sessionID, site, runErrs...)
}

func (w *drainComponent) readTurnFacts(ctx context.Context, sessionID string, site LaunchSite, runErrs ...error) (TurnFacts, error) {
	f := TurnFacts{Now: w.host.arbiter().now(), Site: site, Snapshot: session.BatchDebtSnapshot{}}
	for _, err := range runErrs {
		if errors.Is(err, runq.ErrForeignWorkspace) {
			f.Session.ForeignWorkspace = true
		}
	}
	return w.host.readTurnFactsBatch(ctx, w.host.batchCutoverFacts(), sessionID, f)
}

var errAwaitSuperseded = errors.New("await_tasks was stopped or superseded; recheck current work before waiting again")

// The arbiter's mutable state (docs/plans/2026-10-01-turn-arbiter.md
// sec.2.3, R-ARB-2): the implementation lives in
// runtime/exec/drainctl/turn_arbiter_state.go; this file keeps the agent
// package's root arbiter type and the one-line spellings of its call sites.
type arbiter struct {
	drainctl.Arbiter
}

func (a *arbiter) now() time.Time { return a.Now() }

func (a *arbiter) state(sid string) arbiterStateHandle {
	return arbiterStateHandle{a: a, sid: sid}
}

type arbiterStateHandle struct {
	a   *arbiter
	sid string
}

func (h arbiterStateHandle) idle(now time.Time) bool { return h.a.StateIdle(h.sid, now) }

func (a *arbiter) snapshot(sid string) arbiterSnapshot { return a.Snapshot(sid) }

func (a *arbiter) hold(sid string) (release func()) { return a.Hold(sid) }

func (a *arbiter) held(sid string) bool { return a.Held(sid) }

func (a *arbiter) suspend(sid string) { a.Suspend(sid) }

func (a *arbiter) suspended(sid string) bool { return a.Suspended(sid) }

func (a *arbiter) setSleepAll(sid string) { a.SetSleepAll(sid) }

func (a *arbiter) setMaxWaitWake(sid, wakeID string) { a.SetMaxWaitWake(sid, wakeID) }

func (a *arbiter) clearSleepAll(sid string) string { return a.ClearSleepAll(sid) }

func (a *arbiter) autoResumesOf(sid string) int { return a.AutoResumesOf(sid) }

func (a *arbiter) claimAutoResumeSlot(sid string, rowID int64, eligible bool) bool {
	return a.ClaimAutoResumeSlot(sid, rowID, eligible)
}

func (a *arbiter) pruneOverCap(sid string, notices []sessionnotice.PendingNoticeDebt) {
	a.PruneOverCap(sid, notices)
}

func (a *arbiter) pace(sid string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainctl.Pace) (becameDormant bool) {
	return a.Pace(sid, hintAt, wait, hintOpens, kind)
}

func (a *arbiter) resetGate(sid string) { a.ResetGate(sid) }

func (a *arbiter) resetForHumanMessage(sid string) string { return a.ResetForHumanMessage(sid) }

func (a *arbiter) chainLink(sid string, att *drainctl.DrainAttempt, snap session.BatchDebtSnapshot) {
	a.ChainLink(sid, att.ChainProgress, att.ChainIdleClaims, att.ChainBatchRoots, att.BatchDebtCallIDs, att.BatchSnapshot)
}

func (a *arbiter) chainLastLaunch(sid, command string) bool {
	return a.ChainLastLaunch(sid, command)
}

func (a *arbiter) resetChain(sid string) { a.ResetChain(sid) }

func (a *arbiter) chainLinksOf(sid string) int { return a.ChainLinksOf(sid) }

func (a *arbiter) chainHasClaim(sid, claim string) bool { return a.ChainHasClaim(sid, claim) }

func (a *arbiter) markChainNoticed(sid string) bool { return a.MarkChainNoticed(sid) }

func (a *arbiter) sessionsWithState() []string { return a.SessionsWithState() }

func (a *arbiter) dropIfIdle(sid string, now time.Time) { a.DropIfIdle(sid, now) }

func (a *arbiter) overCapCount(sid string) int { return a.OverCapCount(sid) }

func (a *arbiter) gateHoldsHint(sid string) bool { return a.GateHoldsHint(sid) }

func (a *arbiter) dropSession(sid string) { a.DropSession(sid) }

func (a *arbiter) beginAwait(sid string, all bool) (*drainctl.AwaitTasksEra, string) {
	return a.BeginAwait(sid, all)
}

func (a *arbiter) installAwait(sid string, era *drainctl.AwaitTasksEra, id string) bool {
	return a.InstallAwait(sid, era, id)
}

func (a *arbiter) abortAwait(sid string, era *drainctl.AwaitTasksEra) {
	a.AbortAwait(sid, era)
}
