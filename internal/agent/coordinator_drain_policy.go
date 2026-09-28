// The Drain session policy (doc sec.3.4's "session policy" table) and
// settle-by-failure (doc sec.3.4 "closing debt by failure"). Both act on a
// session id BEFORE (policy) or AFTER (settle) a Drain's own provider-turn
// attempt; see coordinator_wake.go's wakeSession for where each is called
// from, and agent_turn.go's runTurn for the Drain call's OWN turn-start
// re-check of the same policy (defense in depth: a policy-check error here
// fails open, because that turn-start re-check is the authoritative gate).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// drainFailureSettleThreshold is doc sec.3.4's K=3: after this many failed
// wake-up passes for the SAME captured debt id set, a temporary failure is
// treated as settled even without an unrecoverable classification.
const drainFailureSettleThreshold = 3

// sessionDrainPolicy decides, BEFORE a Drain is submitted (doc sec.3.4),
// whether sessionID's category currently permits a Drain TURN at all.
// counted reports whether this category is subject to (and, on a
// SUCCESSFUL turn, must increment) the consecutive-auto-turn cap -- true
// only for the web/default branch: a delegated child's policy is "while its
// delegation row runs", not a counter, and doc sec.3.4 is explicit that
// "failed Drains do not consume the cap" is meaningful only where a cap
// applies at all. Callers must route external-driver (CLI root) sessions to
// a hint-only path before ever reaching this -- see wakeSession.
func (c *coordinator) sessionDrainPolicy(ctx context.Context, sessionID string) (allowed, counted bool, err error) {
	if c.asyncJobs != nil {
		running, runErr := c.asyncJobs.hasRunningDelegationFor(ctx, sessionID)
		if runErr != nil {
			return false, false, runErr
		}
		if running {
			// Delegated child, currently armed: a turn is allowed while its
			// delegation row stays running (doc sec.3.4).
			return true, false, nil
		}
		// No RUNNING delegation row claims sessionID right now -- after Stop
		// or the delegation ending, doc sec.3.4 says "no further turn, by
		// construction, without a separate race against Stop": the
		// construction is Stop's own suspendAutoResume call (coordinator_
		// interrupt.go's Cancel, applied to every id in the cancelled tree)
		// and the driver teardown on scope close (releaseDriverIfScopeClosed),
		// not a second check here keyed on subAgentDrivers alone -- a
		// registered driver with no currently-running row is also the
		// ordinary shape of a bare test fixture that never modeled a real
		// delegation row at all. Falls through to the generic policy below,
		// which a Stop's suspension already caps to zero headroom.
	}
	// Web/default (doc sec.3.4): the same consecutive-cap that already
	// bounds SDK background-shell auto-resume, applied here to async-job/
	// delegation notice wakes too, reset by the last human message
	// (resetConsecutiveResume/ResetAutoResumeCounter) -- also what a Stop
	// suspension (suspendAutoResume) rides on. wakeSession increments the
	// counter itself, ONLY on a successful turn (never on a failed one).
	return c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes, true, nil
}

// recordDrainOutcome is the shared post-turn accounting for a Drain-context
// turn attempt (doc sec.3.4/sec.6), used both by wakeSession (a coordinator-
// submitted Drain) and the CLI root's own loop (internal/app, whose turns
// never go through wakeSession at all -- see ReactionDebtSource.
// RecordDrainTurnOutcome). snapshot is the debt id set captured BEFORE the
// turn ran; attempted is false when this specific call never actually ran
// (merely queued behind another owner) -- nothing to account for yet, since
// that later turn's own in-turn debt+policy re-check governs it and some
// future fresh call will still observe whatever debt survives.
func (c *coordinator) recordDrainOutcome(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, attempted bool, turnErr error) {
	if !attempted {
		return
	}
	if turnErr != nil {
		if turnAttemptRefused(turnErr) {
			// Doc sec.3.4 rule (c)/(b): an admission refusal (session-lock
			// held by another process, shutdown) keeps the debt untouched
			// and writes no marker -- it is not a turn failure, just a
			// missed window. The session goes into the 60s recheck set
			// instead of being forgotten.
			c.addToRecheckSet(job.owner)
			return
		}
		c.settleOrRetryDrainFailure(ctx, job, snapshot, turnErr)
	}
	// A successful turn is not re-checked against its own captured snapshot
	// here: this codebase's wakeSession tests widely register a bare mock
	// SessionAgent as a session's driver (subAgentDrivers.register), whose
	// canned Run never touches the reacted column at all -- a "did this
	// turn's debt actually clear" check would misclassify every one of
	// those as permanently stuck. A genuinely broken pull (doc sec.6: "does
	// not loop") is still bounded less precisely but safely by the hint/
	// recheck-set/60s-pass cadence rather than a tight per-release retry.
}

// settleOrRetryDrainFailure implements doc sec.3.4's "closing debt by
// failure". Reached ONLY for a REAL provider-turn attempt that failed (an
// admission refusal is handled separately by wakeSession's own
// turnAttemptRefused branch, which never reaches here). snapshot is the
// debt id set captured BEFORE the turn ran (session.AsyncJobStore.
// CaptureDebtSnapshot) -- the SAME set every counter/settle write below is
// scoped to, never whatever is pending "now".
func (c *coordinator) settleOrRetryDrainFailure(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, runErr error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil || snapshot.Empty() {
		return
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if classifyProviderError(runErr) == classTerminal {
		// Unrecoverable (quota/401/402/other terminal classification):
		// close the debt on exactly the captured set immediately, K
		// doesn't matter.
		c.settleAndMark(settleCtx, job, snapshot, runErr)
		return
	}
	// Temporary failure (doc sec.3.4): increments the counter on exactly
	// the captured rows; the next attempt comes from a hint or the 60s
	// pass, never immediately. Settles once K is reached.
	c.incrementThenSettleIfThreshold(settleCtx, job, snapshot, runErr)
}

// incrementThenSettleIfThreshold bumps wake_attempts on snapshot's rows and,
// once any of them reaches drainFailureSettleThreshold, settles the debt by
// failure exactly as an unrecoverable classification would -- shared by
// settleOrRetryDrainFailure's temporary-failure branch and
// checkStuckDrainProgress (doc sec.6: "a Drain ... with a permanent pull
// error does not loop (the launch counter is bounded)" -- the same counter
// bounds both causes, since the observable symptom, debt never clearing, is
// identical).
func (c *coordinator) incrementThenSettleIfThreshold(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, cause error) {
	if err := c.asyncJobs.store.IncrementWakeAttempts(ctx, job.owner, snapshot); err != nil {
		slog.Error("settle-by-failure: increment wake attempts failed", "session_id", job.owner, "err", err)
		return
	}
	attempts, err := c.asyncJobs.store.MaxWakeAttempts(ctx, job.owner, snapshot)
	if err != nil {
		slog.Error("settle-by-failure: read wake attempts failed", "session_id", job.owner, "err", err)
		return
	}
	if attempts == 0 {
		// Every captured row already settled independently (a real turn
		// reacted to it, or an earlier pass already closed it) -- nothing
		// left for this pass to act on.
		return
	}
	if attempts < drainFailureSettleThreshold {
		return
	}
	c.settleAndMark(ctx, job, snapshot, cause)
}

// settleAndMark closes debt on exactly snapshot's rows and writes the
// visible marker (doc sec.3.4/ASYNC-09).
func (c *coordinator) settleAndMark(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, cause error) {
	if err := c.asyncJobs.store.SettleReactedFailed(ctx, job.owner, snapshot); err != nil {
		slog.Error("settle-by-failure: settle reacted-failed failed", "session_id", job.owner, "err", err)
		return
	}
	c.persistWakeFailedMarker(ctx, job, cause)
}

// persistWakeFailedMarker persists a durable session_notices row (kind
// NoticeKindWakeFailed, wake=0 per doc sec.3.4's wake-policy table) recording
// that a Drain call's provider turn failed permanently and its debt was
// closed by failure. The next successful pull surfaces it as an ordinary
// pending notice -- visible exactly once, without forcing another immediate
// turn (wake=0).
func (c *coordinator) persistWakeFailedMarker(ctx context.Context, job jobIdentity, cause error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return
	}
	markerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	text := fmt.Sprintf(
		"Не удалось продолжить работу после события %s: %s. Событие сохранено; продолжение — при следующем ходе.",
		job.toolCallID, cause,
	)
	if err := c.asyncJobs.store.InsertSessionNotice(markerCtx, job.owner, session.NoticeKindWakeFailed, text, false, ""); err != nil {
		slog.Error("wakeSession: failed to persist wake-failed marker",
			"session_id", job.owner, "job_id", job.toolCallID, "err", err)
	}
}
