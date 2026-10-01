// Test-only fixtures for the arbiter state (R-ARB-2): the ONE place tests
// may seed or nudge the arbiter's state by hand, instead of the scattered
// coordinator maps the pre-refactor tests poked (docs/plans/2026-10-01-turn-
// arbiter.md sec.5). Assertions stay on OBSERVABLE outcomes (verdicts,
// launches, DB rows); these helpers only arrange inputs.
package agent

import (
	"context"
	"time"
)

// seedArbiterState runs fn over sessionID's arbiter state under the lock:
// the single test constructor for hand-set launch facts.
func (c *coordinator) seedArbiterState(sessionID string, fn func(*arbiterState)) {
	c.arb.mu.Lock()
	defer c.arb.mu.Unlock()
	fn(c.arb.stateLocked(sessionID))
}

// readArbiterState copies sessionID's arbiter state for assertions.
func (c *coordinator) readArbiterState(sessionID string) arbiterState {
	c.arb.mu.Lock()
	defer c.arb.mu.Unlock()
	s := c.arb.bySession[sessionID]
	if s == nil {
		return arbiterState{}
	}
	cp := *s
	return cp
}

// bumpConsecutiveResume is the test fixture for "one more auto-resume
// happened": it spends a slot the way a real admission does.
func (c *coordinator) bumpConsecutiveResume(sessionID string) {
	c.arb.claimAutoResumeSlot(sessionID, 0, true)
}

// paceDrainGate is the test fixture spelling of the gate pacing (the
// production writer paths are accountDrainAttempt/noteDrainRefused; tests
// pace by hand to reach a given gate state).
func (l *workLedger) paceDrainGate(owner string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainPace) bool {
	if l.coord == nil {
		return false
	}
	return l.coord.arb.pace(owner, hintAt, wait, hintOpens, kind)
}

// resetDrainGate is the test fixture spelling of the gate reset.
func (l *workLedger) resetDrainGate(owner string) {
	if l.coord != nil {
		l.coord.arb.resetGate(owner)
	}
}

// setReactionChain seeds the reaction-chain guard's state (the old tests'
// direct map writes).
func (c *coordinator) setReactionChain(sessionID string, links int, claims map[string]struct{}, noticed bool) {
	c.seedArbiterState(sessionID, func(s *arbiterState) {
		s.chainLinks = links
		s.chainClaims = claims
		s.chainNoticed = noticed
	})
}

// setOverCap seeds the over-cap set and the cap counter together (they only
// mean something in that combination: rows recorded on arrival with every
// slot spent).
func (c *coordinator) setOverCap(sessionID string, autoResumes int, rowIDs []int64) {
	c.seedArbiterState(sessionID, func(s *arbiterState) {
		s.autoResumes = autoResumes
		s.overCap = make(map[int64]struct{}, len(rowIDs))
		for _, id := range rowIDs {
			s.overCap[id] = struct{}{}
		}
	})
}

// gateOpen is the observable gate read the old drainGateOpen gave tests:
// open = never paced || a newer hint reopened it || the pause passed with
// both streaks below dormancy; dormant reports a shut gate that only a newer
// fact (free) or a human message (paid) reopens.
func gateOpen(c *coordinator, owner string, now time.Time) (open, dormant bool, retryAt time.Time) {
	g := c.arb.snapshot(owner).Gate
	if !g.Paced {
		return true, false, time.Time{}
	}
	hintNow := uint64(0)
	if c.asyncJobs != nil {
		hintNow = c.asyncJobs.hintSeqOf(owner)
	}
	paidDormant := g.PaidStreak >= turnDormantStreak
	if g.HintOpens && !paidDormant && hintNow != g.HintSeen {
		return true, false, g.RetryAt
	}
	if !paidDormant && g.FreeStreak < turnDormantStreak {
		return !now.Before(g.RetryAt), false, g.RetryAt
	}
	return false, true, g.RetryAt
}

// capDeferred is the observable answer of the old bgShellCapDeferred: rule
// 11 of the arbiter table, read through a re-check launch decision (a
// release: spends no slot).
func capDeferred(ctx context.Context, c *coordinator, sessionID string) (bool, error) {
	facts, err := c.readTurnFacts(ctx, sessionID, siteRelease)
	if err != nil {
		return false, err
	}
	v := decide(facts)
	return v.Kind == VDefer && v.Reason == "background-shell auto-resume cap reached", nil
}
