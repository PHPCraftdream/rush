// The arbiter's mutable state (docs/plans/2026-10-01-turn-arbiter.md
// sec.2.3, R-ARB-2): the implementation lives in
// runtime/exec/drainctl/turn_arbiter_state.go; this file keeps the agent
// package's root arbiter type and the one-line spellings of its call sites.
package agent

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
)

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
