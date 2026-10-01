// Reaction-debt hint plumbing (phase-4 step 4, docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.3.4): the hint counter every committed fact
// bumps, the external-driver marker a live `rush run` loop claims so
// wakeSession never submits it a Drain turn, and the per-session Drain
// launch gate (attempts design 1.2).
package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// bumpHint increments owner's hint counter and signals its changed channel
// (doc sec.3.4: "after a commit of a transition or notice with wake=1, a
// non-blocking hint is sent to the session"). Called only for a FACT
// (wakeSession fact=true), never from a release or tick re-check -- the
// hint's only job is "something may have
// changed, re-check", never a promise that a turn will follow.
func (l *workLedger) bumpHint(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	s := l.sessionLocked(owner)
	s.hintSeq++
	l.mu.Unlock()
	signalWorkSession(s)
}

// hintSeqOf reads owner's current hint counter.
func (l *workLedger) hintSeqOf(owner string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.bySession[owner]; s != nil {
		return s.hintSeq
	}
	return 0
}

// waitForHint blocks until owner's hint counter advances past since, ctx is
// done, or the ledger closes. Returns true only on a genuine hint (not on
// ctx/close). Used by the CLI loop's wait step (doc sec.3.5); the caller
// bounds the wait (WaitForHint's 5s fallback) and re-reads the DB, since a
// hint only exists inside this process.
func (l *workLedger) waitForHint(ctx context.Context, owner string, since uint64) bool {
	for {
		l.mu.Lock()
		s := l.sessionLocked(owner)
		if s.hintSeq != since {
			l.mu.Unlock()
			return true
		}
		changed := s.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-l.closedCh:
			return false
		case <-changed:
			// Loop back and re-read hintSeq: signalWorkSession's buffered-1
			// channel can fire for an unrelated reason (job start/finish),
			// so only an actual counter change is a real hint.
		}
	}
}

// claimExternalDriver marks owner as driven by an external loop (the CLI
// root of a live `rush run` process): wakeSession must hint it, never
// submit a Drain turn (doc sec.3.4).
func (l *workLedger) claimExternalDriver(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	l.sessionLocked(owner).externalDriver = true
	l.mu.Unlock()
}

// releaseExternalDriver clears owner's external-driver marker (the loop
// exited): a later web-driven wake for the same session id must go back to
// ordinary Drain-turn routing.
func (l *workLedger) releaseExternalDriver(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	if s := l.bySession[owner]; s != nil {
		s.externalDriver = false
	}
	l.mu.Unlock()
}

// isExternalDriver reports whether owner is currently claimed by an
// external driver loop.
func (l *workLedger) isExternalDriver(owner string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[owner]
	return s != nil && s.externalDriver
}

// reactionDebtExists wraps the store's debt predicate (doc sec.3.4/DUR-4).
// A nil store (isolated ledger tests) reports no debt.
func (l *workLedger) reactionDebtExists(ctx context.Context, owner string) (bool, error) {
	if l.store == nil {
		return false, nil
	}
	return l.store.ReactionDebtExists(ctx, owner)
}

// hasRunningDelegationFor wraps the store's child-delegation-running check
// (doc sec.3.4's session-policy table).
func (l *workLedger) hasRunningDelegationFor(ctx context.Context, childSessionID string) (bool, error) {
	if l.store == nil {
		return false, nil
	}
	return l.store.HasRunningDelegationFor(ctx, childSessionID)
}

// captureDebtSnapshot wraps the store's settle-by-failure id-set capture.
func (l *workLedger) captureDebtSnapshot(ctx context.Context, owner string) (session.DebtSnapshot, error) {
	if l.store == nil {
		return session.DebtSnapshot{}, nil
	}
	return l.store.CaptureDebtSnapshot(ctx, owner)
}

// arbPace paces the session's launch gate in the arbiter state (R-ARB-2: the
// gate no longer lives on the ledger). A nil-coordinator ledger (isolated
// tests) paces nothing.
func (l *workLedger) arbPace(owner string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainPace) bool {
	if l.coord == nil {
		return false
	}
	return l.coord.arb.pace(owner, hintAt, wait, hintOpens, kind)
}

// resetGate opens the session's launch gate in the arbiter state.
func (l *workLedger) resetGate(owner string) {
	if l.coord != nil {
		l.coord.arb.resetGate(owner)
	}
}

// drainPace says what a pacing counts toward the dormancy streaks (the
// arbiter's gate keeps the same accounting the old ledger gate had).
type drainPace uint8

const (
	// paceUncounted: a refusal or a failed ordinary turn -- pause only.
	paceUncounted drainPace = iota
	// paceFreeNoTurn: a no-turn Drain left pending debt behind (pull failing).
	paceFreeNoTurn
	// pacePaidUnreacted: a paid attempt left the debt unreacted.
	pacePaidUnreacted
)
