// ReactionDebtSource is the phase-4 step 4 replacement for AsyncCompletionSource
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.5): the CLI loop
// (internal/app/app_run_async.go) no longer drains an in-memory completion
// queue -- it claims itself as sessionID's external driver (so wakeSession
// hints it instead of submitting a Drain turn of its own), then re-derives
// each next turn and its own exit condition straight from the DB.
package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ReactionDebtSource is implemented by *coordinator; internal/app type-
// asserts for it exactly like it used to for AsyncCompletionSource.
type ReactionDebtSource interface {
	// ClaimExternalDriver marks sessionID as externally driven for the
	// lifetime of one `rush run` loop -- see wakeSession's external-driver
	// branch.
	ClaimExternalDriver(sessionID string)
	// ReleaseExternalDriver clears the marker once the loop exits.
	ReleaseExternalDriver(sessionID string)
	// ReactionDebtExists reads doc sec.3.4's debt predicate for sessionID.
	ReactionDebtExists(ctx context.Context, sessionID string) (bool, error)
	// ScopeOpen evaluates doc sec.3.5's scope predicate for sessionID: a
	// running task row on a live host, OR (since this is always called
	// BETWEEN turns, never mid-turn) a reaction debt this session's own
	// policy allows a turn on. For the CLI root calling this on its OWN
	// session id, policy is trivially "yes" (doc: "the loop decides").
	ScopeOpen(ctx context.Context, sessionID string) (bool, error)
	// WaitForHint blocks until sessionID's hint counter advances, ctx is
	// done, or a bounded same-process fallback elapses. The caller must
	// still re-derive its decision from ReactionDebtExists/ScopeOpen
	// afterward -- a returned hint is a reason to re-check, not itself an
	// answer.
	WaitForHint(ctx context.Context, sessionID string)
	// CaptureDrainSnapshot snapshots sessionID's current debt id set --
	// call BEFORE a Drain-context (empty-prompt) loop turn runs, pair with
	// RecordDrainTurnOutcome afterward (doc sec.6's launch-counter bound:
	// the CLI root's own turns never go through wakeSession, so this is the
	// loop's own equivalent of wakeSession's pre-Run capture).
	CaptureDrainSnapshot(ctx context.Context, sessionID string) session.DebtSnapshot
	// RecordDrainTurnOutcome applies settle-by-failure/stuck-progress
	// accounting to a Drain-context loop turn that actually ran (never call
	// this for a turn that was merely queued/never attempted -- doc
	// sec.3.4/sec.6, mirrors wakeSession's own post-Run handling exactly).
	RecordDrainTurnOutcome(ctx context.Context, sessionID string, snapshot session.DebtSnapshot, turnErr error)
}

// ClaimExternalDriver implements ReactionDebtSource.
func (c *coordinator) ClaimExternalDriver(sessionID string) {
	if c.asyncJobs != nil {
		c.asyncJobs.claimExternalDriver(sessionID)
	}
}

// ReleaseExternalDriver implements ReactionDebtSource.
func (c *coordinator) ReleaseExternalDriver(sessionID string) {
	if c.asyncJobs != nil {
		c.asyncJobs.releaseExternalDriver(sessionID)
	}
}

// ReactionDebtExists implements ReactionDebtSource.
func (c *coordinator) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil {
		return false, nil
	}
	return c.asyncJobs.reactionDebtExists(ctx, sessionID)
}

// reactionDebtSourceHintFallback bounds WaitForHint's own wait even without
// a hint or ctx cancellation -- belt-and-braces against a missed signal
// inside this one process; the CLI loop's own 60s tick (app_run_async.go)
// is the actual cross-process fallback doc sec.3.5 describes.
const reactionDebtSourceHintFallback = 5 * time.Second

// WaitForHint implements ReactionDebtSource.
func (c *coordinator) WaitForHint(ctx context.Context, sessionID string) {
	if c.asyncJobs == nil {
		return
	}
	since := c.asyncJobs.hintSeqOf(sessionID)
	waitCtx, cancel := context.WithTimeout(ctx, reactionDebtSourceHintFallback)
	defer cancel()
	c.asyncJobs.waitForHint(waitCtx, sessionID, since)
}

// CaptureDrainSnapshot implements ReactionDebtSource.
func (c *coordinator) CaptureDrainSnapshot(ctx context.Context, sessionID string) session.DebtSnapshot {
	if c.asyncJobs == nil {
		return session.DebtSnapshot{}
	}
	snap, err := c.asyncJobs.captureDebtSnapshot(ctx, sessionID)
	if err != nil {
		return session.DebtSnapshot{}
	}
	return snap
}

// RecordDrainTurnOutcome implements ReactionDebtSource.
func (c *coordinator) RecordDrainTurnOutcome(ctx context.Context, sessionID string, snapshot session.DebtSnapshot, turnErr error) {
	job := jobIdentity{owner: sessionID, toolCallID: "cli-loop"}
	c.recordDrainOutcome(ctx, job, snapshot, true, turnErr)
}

// ScopeOpen implements ReactionDebtSource -- doc sec.3.5: "S has a running
// task row on a LIVE host, OR S is mid-turn, OR S has a reaction debt and
// its policy allows a turn". The "mid-turn" branch never applies here: every
// caller evaluates this strictly BETWEEN its own turns.
func (c *coordinator) ScopeOpen(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, nil
	}
	rows, err := c.asyncJobs.store.ListRunningForOwners(ctx, []string{sessionID})
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if c.asyncJobs.store.HostNotDead(row.HostID) {
			return true, nil
		}
	}
	debt, err := c.asyncJobs.store.ReactionDebtExists(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return debt, nil
}
