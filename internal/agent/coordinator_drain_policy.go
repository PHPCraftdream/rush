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
		// B3/C6 fix: no RUNNING delegation row claims sessionID right now.
		// Before this fix, falling straight through to the generic
		// web/default policy below let a released or expired child ride the
		// SAME up-to-maxConsecutiveAutoResumes headroom as a real web
		// session -- and by the time such a turn actually ran, agentFor's
		// driver lookup (subAgentDrivers, already torn down by
		// releaseDriverIfScopeClosed once scope closed) would fall back to
		// c.currentAgent, the ROOT coder agent: full tool set, no
		// RunAllowlist, no child system prompt (security-relevant). Key the
		// refusal on DURABLE identity instead of the in-memory driver
		// registry, which does not survive scope closing or a process
		// restart: a session ever created as a delegation target carries
		// ParentSessionID (session.CreateTaskSession) for its entire life.
		// Known limitation (documented, not fixed here -- no new SQL/store
		// query available in this wave): `sessions fork --parent X` ALSO
		// sets ParentSessionID for a purpose unrelated to delegation, so a
		// forked-with-parent session is (rarely, and only if it later owns
		// its own async work) also refused further auto-turns by this
		// check. See docs/reviews/2026-09-29-async-phase4-round1.md B3/C6.
		isChild, childErr := c.isDurableDelegationChild(ctx, sessionID)
		if childErr != nil {
			return false, false, childErr
		}
		if isChild {
			// A durable delegation child with no running delegation row:
			// its delegation ended (or was stopped) and it gets no further
			// Drain turn, by construction (doc sec.3.4's policy table) --
			// never re-routed to whatever session currently answers
			// agentFor(sessionID) once its driver is torn down.
			return false, false, nil
		}
		// Not a delegation child at all (a bare test fixture, or a normal
		// session that merely has no running delegation because it was
		// never one) -- falls through to the generic policy below.
	}
	// B7 design decision (docs/reviews/2026-09-29-async-phase4-round1.md,
	// operator HARD RULES item 1): the consecutive-auto-turn cap applies
	// ONLY to the SDK background-shell auto-resume path
	// (notifyBackgroundJobDone's AutoResumeOnJobDone branch), matching
	// pre-phase-4 behavior -- NOT to ordinary async-job/delegation/
	// supervision/wake_only notice wakes, which were UNCAPPED before phase
	// 4 and are UNCAPPED again here. Applying the cap to those too (phase
	// 4's regression) meant a session with enough background bash/delegation
	// completions could be silently starved of further auto-wakes even
	// though nothing about that traffic is runaway-turn-shaped the way
	// repeated bg-shell auto-resumes are. autoTurnCapAppliesCtxKey is set on
	// ctx ONLY by notifyBackgroundJobDone (coordinator_background.go) --
	// its presence is this function's only reliable signal that THIS
	// specific wake is a capped bg-shell auto-resume, since a Drain call's
	// own AutoResumed/BackgroundJobNotice FIELDS are unconditionally true
	// for every Drain regardless of origin (newDrainCall) and cannot be used
	// to discriminate; CHANGELOG-visible consequence documented in W-DOCS.
	//
	// "No cap" means no THROTTLE on volume (counted=false, never
	// incremented) -- it does NOT mean Stop's own "automatic turns paused
	// until the next human message" stops applying: suspendAutoResume
	// (coordinator_interrupt.go's Cancel) forces this SAME counter to the
	// cap on every id in a cancelled tree, deliberately reusing this
	// machinery instead of a second flag that could drift out of sync with
	// it (see suspendAutoResume's own doc) -- so the READ below must still
	// run for the uncapped category too, or Stop's suspension would silently
	// stop gating async-job/delegation wakes while continuing to gate
	// bg-shell ones.
	if _, capped := ctx.Value(autoTurnCapAppliesCtxKey{}).(bool); !capped {
		return c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes, false, nil
	}
	// Web/default, bg-shell auto-resume (doc sec.3.4): reset by the last
	// human message (resetConsecutiveResume/ResetAutoResumeCounter) --
	// also what a Stop suspension (suspendAutoResume) rides on. wakeSession
	// increments the counter itself, ONLY on a successful turn that actually
	// reached the provider (never a failed, queued, or no-turn one).
	return c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes, true, nil
}

// isDurableDelegationChild reports whether sessionID was EVER created as a
// delegation target (session.CreateTaskSession sets ParentSessionID at
// creation, durably, unlike the in-memory subAgentDriverRegistry which
// releaseDriverIfScopeClosed tears down the instant scope closes). See
// sessionDrainPolicy's own doc for the known `sessions fork --parent`
// ambiguity this shares the same signal with.
func (c *coordinator) isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error) {
	if c.sessions == nil {
		return false, nil
	}
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sess.ParentSessionID != "", nil
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
