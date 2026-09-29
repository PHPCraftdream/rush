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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// isPseudoJobID reports whether toolCallID is one of wakeSession's own
// internal placeholder identities (jobIdentity.toolCallID's doc: "diagnostic-
// only now") rather than a real, model-facing async-job tool_call_id.
// settleAndMark uses this to keep a pseudo-id out of a marker's text (item 1
// fix, docs/reviews/2026-09-29-async-phase4-round1.md B5/C3) while still
// naming a REAL tool_call_id, which is useful context for whoever reads the
// marker later.
func isPseudoJobID(toolCallID string) bool {
	switch toolCallID {
	case "release-recheck", "cli-loop", "recheck-pass":
		return true
	}
	return strings.HasPrefix(toolCallID, "supervision-")
}

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
	// Durable external-driver marker (docs/reviews/2026-09-29-async-phase4-
	// round1-rerun-design.md, Problem 2): a live `rush run` loop in ANOTHER
	// process drives this session and reacts to its debt itself. This
	// process never starts a reaction turn for it -- covers wakeSession
	// before submit and decideDrainTurn at an already-admitted Drain's turn
	// start (the latter then only transfers notices into history). The
	// in-memory marker (own loop) skips the lookup.
	if c.asyncJobs != nil && !c.asyncJobs.isExternalDriver(sessionID) {
		foreign, drvErr := c.asyncJobs.foreignLiveDriver(ctx, sessionID)
		switch {
		case drvErr != nil && c.persistentMode.Load():
			// Web coordinator: fail closed; the 60s pass retries.
			slog.Warn("session drain policy: driver marker unreadable; refusing the turn for now",
				"session_id", sessionID, "err", drvErr)
			c.addToRecheckSet(sessionID)
			return false, false, nil
		case drvErr == nil && foreign:
			if debt, _ := c.asyncJobs.reactionDebtExists(ctx, sessionID); debt {
				c.addToRecheckSet(sessionID)
			}
			return false, false, nil
		}
		// A CLI coordinator (no recheck ticker) falls through on a read
		// error: the existing fail-open.
	}
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
		// isDurableDelegationChild additionally confirms an async_jobs
		// delegation row actually backs that ParentSessionID (item 8 fix,
		// docs/reviews/2026-09-29-async-phase4-round1.md B3/C6): a
		// `sessions fork --child X` session also sets ParentSessionID for an
		// unrelated purpose but never claims a matching delegation job, so
		// it is no longer swept into this refusal -- see that function's own
		// doc for the exact signal and its retention-bounded caveat.
		isChild, childErr := c.isDurableDelegationChild(ctx, sessionID)
		if childErr != nil {
			return false, false, childErr
		}
		if isChild {
			// A durable delegation child with no running delegation row:
			// its delegation ended (or was stopped), so it gets no further
			// Drain turn. The refusal is explicit, keyed on the parent's
			// delegation row (isDurableDelegationChild), not implied by the
			// driver registry -- never re-routed to whatever session
			// currently answers agentFor(sessionID) once its driver is torn
			// down.
			return false, false, nil
		}
		// Not a delegation child at all (a bare test fixture, or a normal
		// session that merely has no running delegation because it was
		// never one) -- falls through to the generic policy below.
	}
	// B-dev6 fix (item 7, docs/reviews/2026-09-29-async-phase4-round1.md):
	// the policy table's "Background shell: only with AutoResumeOnJobDone"
	// row was enforced ONLY at notifyBackgroundJobDone's own hint-time call
	// (coordinator_background.go) -- release-recheck (recheckDebtOnRelease)
	// and the 60s pass call wakeSession from a plain context.Background()
	// with no autoTurnCapAppliesCtxKey, so they fall into the UNCAPPED
	// generic branch below with no idea the debt they are about to react to
	// is a bg-shell notice at all. If AutoResumeOnJobDone is off and this
	// session's ENTIRE current debt is bg-shell-done notices (no async job,
	// no other notice kind mixed in), refuse here too -- exactly the policy
	// the direct hint-time call already applies, just re-checked from a
	// caller that cannot see the origin. A session with ANY other debt
	// alongside a bg-shell notice still falls through (that other debt's own
	// policy governs the turn; the bg-shell notice itself simply rides
	// along, same as it would inside an ordinary allowed turn today).
	autonomyOn := c.cfg != nil && c.autonomyEnabled()
	if c.asyncJobs != nil && !autonomyOn {
		bgOnly, bgErr := c.sessionDebtIsBGShellOnly(ctx, sessionID)
		if bgErr != nil {
			return false, false, bgErr
		}
		if bgOnly {
			return false, false, nil
		}
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
	// to discriminate (see the CHANGELOG entry on the auto-turn cap).
	//
	// "No cap" means no THROTTLE on volume (counted=false, never
	// incremented) -- it does NOT mean Stop's own "automatic turns paused
	// until the next human message" stops applying: suspendAutoResume
	// (coordinator_interrupt.go's Cancel) marks every id in a cancelled tree
	// suspended, a state of its own (autoTurnsSuspended) that gates EVERY
	// kind of automatic turn, capped or not. The bg-shell cap counter is a
	// separate thing: filling it never pauses the uncapped category.
	if c.autoResumeSuspended(sessionID) {
		return false, false, nil
	}
	if _, capped := ctx.Value(autoTurnCapAppliesCtxKey{}).(bool); !capped {
		return true, false, nil
	}
	// Web/default, bg-shell auto-resume (doc sec.3.4): the cap counter is
	// reset by the last human message (resetConsecutiveResume/
	// ResetAutoResumeCounter, the same event that lifts Stop's suspension).
	// wakeSession increments it itself, ONLY on a successful turn that
	// actually reached the provider (never a failed, queued, or no-turn one).
	return c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes, true, nil
}

// isDurableDelegationChild reports whether sessionID was EVER created as a
// delegation target (session.CreateTaskSession sets ParentSessionID at
// creation, durably, unlike the in-memory subAgentDriverRegistry which
// releaseDriverIfScopeClosed tears down the instant scope closes).
//
// B3/C6 follow-up fix (docs/reviews/2026-09-29-async-phase4-round1.md, item
// 8): ParentSessionID alone is ambiguous -- `sessions fork --child` (the
// review's "sessions fork --parent" is the same knob under its CLI-flag
// name) sets the identical field on a session that was never a delegation
// target at all, for a wholly unrelated purpose (session.ForkOptions.
// ParentID). A forked child was, before this fix, refused Drain turns by
// the SAME branch a real released delegation child hits.
//
// The real signal doesn't need a schema change: every delegation claims an
// async_jobs row with Owner=ParentSessionID and ChildSessionID=the exact
// child session's id, independent of whether the job is still running -- the
// row outlives the delegation ending, until retention purges it. The Kind is
// deliberately NOT matched: runSubAgent claims JobKindAgent, agentic_fetch
// claims JobKindFetch, and any future kind that names a child session is a
// delegation target just the same. A plain fork never claims any async job
// at all, so no row naming it as ChildSessionID ever exists. This reuses
// the pre-existing ListAsyncJobsForOwner (the same query `sessions jobs`/
// `sessions why` already use -- no new SQL) instead of the coarser
// ParentSessionID-only check.
//
// The signal is durable only as long as retention keeps the row (7 days
// past done+reacted, doc sec.3.7/A5) -- after that window a long-finished
// delegation's child falls back to the generic web/default policy below,
// which the ordinary auto-turn cap still bounds; this narrows the
// false-positive (fork) case, it is not required to hold forever.
func (c *coordinator) isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error) {
	if c.sessions == nil {
		return false, nil
	}
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if sess.ParentSessionID == "" {
		return false, nil
	}
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		// No durable store to confirm identity against (bare test fixture)
		// -- fall back to the coarser pre-fix signal rather than silently
		// treating every ParentSessionID session as a non-child.
		return true, nil
	}
	jobs, listErr := c.asyncJobs.store.ListAsyncJobsForOwner(ctx, sess.ParentSessionID)
	if listErr != nil {
		return false, listErr
	}
	for _, job := range jobs {
		if job.ChildSessionID.Valid && job.ChildSessionID.String == sessionID {
			return true, nil
		}
	}
	// ParentSessionID is set but no delegation job ever named sessionID as
	// its child -- a fork with a parent set, not a delegation target.
	return false, nil
}

// sessionDebtIsBGShellOnly reports whether sessionID's ENTIRE current
// reaction debt (doc sec.3.4's predicate: wake=1/reacted=0/delivery<>'void',
// PENDING-INCLUSIVE -- same scope as ReactionDebtExists, deliberately NOT
// captureDebtSnapshot's delivery='done'-only scope, see below) consists of
// bg-shell-done notices (session.NoticeKindBGShellDone) and nothing else --
// no outstanding async-job debt, no other notice kind. Used by
// sessionDrainPolicy's B-dev6 fix (item 7) to re-apply the "Background
// shell: only with AutoResumeOnJobDone" policy-table row at release-recheck/
// 60s-pass time, which (unlike the direct hint-time call) has no ctx-carried
// signal of the debt's origin. false when there is no debt at all (nothing
// to gate) or on any job-id debt or mixed/non-bg-shell notice kind.
//
// W-DRAIN task A regression (found via the full internal/agent suite after
// narrowing captureDebtSnapshot to delivery='done' for C18/B-dev1): this
// used to read captureDebtSnapshot directly, which is CORRECT for settle-
// by-failure (only ever act on rows the failed/stuck turn actually saw) but
// WRONG here -- a release-recheck's own bg-shell notice is typically still
// delivery='pending' (not yet pulled by any turn) at the moment this policy
// check runs, so the done-only snapshot reported it as no debt at all,
// silently skipping the refusal gate and letting AutoResumeOnJobDone=off
// sessions get a Drain turn anyway. Reads the two tables directly instead,
// at the SAME pending-inclusive scope ReactionDebtExists uses.
func (c *coordinator) sessionDebtIsBGShellOnly(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs.store == nil {
		return false, nil
	}
	hasJobDebt, noticeKinds, err := c.asyncJobs.store.PendingInclusiveDebtSummary(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if hasJobDebt || len(noticeKinds) == 0 {
		// Any outstanding async-job debt at all disqualifies -- this gate
		// only ever applies to a session whose ENTIRE debt is bg-shell
		// notices. No debt at all (nothing to gate) also disqualifies.
		return false, nil
	}
	for _, k := range noticeKinds {
		if k != session.NoticeKindBGShellDone {
			return false, nil
		}
	}
	return true, nil
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
// attemptAssistantMsgID is the OnAssistantMessageCreated-reported id this
// SPECIFIC Drain attempt wrote (wakeSession captures it; the CLI loop does
// not currently wire per-attempt evidence through ExecuteRun, so it always
// passes "" -- see settleOrRetryDrainFailure's fallback for that case).
func (c *coordinator) recordDrainOutcome(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, attempted bool, turnErr error, attemptAssistantMsgID string) {
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
		c.settleOrRetryDrainFailure(ctx, job, snapshot, turnErr, attemptAssistantMsgID)
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
//
// W-DRAIN item 1 (B5/C3 deep fix): classification uses the Drain's OWN
// attempt evidence when available (attemptAssistantMsgID, wakeSession's
// capture), mirroring shouldRetryTurn's ownership gate, rather than runErr
// alone. runErr alone cannot distinguish "this Drain's own provider call
// failed" from "some unrelated failure surfaced through the same return
// path" -- e.g. a queued user turn dispatched behind this Drain in the same
// mailbox release window, or a DB error in the turn-start preamble before
// any provider request was ever made.
func (c *coordinator) settleOrRetryDrainFailure(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, runErr error, attemptAssistantMsgID string) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil || snapshot.Empty() {
		return
	}
	// B5/C3 fix: a cancellation/deadline error, or AwaitingAnswerError, is
	// NOT evidence that the Drain's own provider attempt failed -- there is
	// no attempt outcome here to classify at all. classifyProviderError's
	// "context cancellation is terminal here" is documented as safe ONLY
	// because its ordinary caller (shouldRetryTurn) first gates on owning
	// the attempt's own assistant row with a real error finish
	// (turnMadeProgress/FinishReasonError) -- this function has no such
	// gate, so it must never reach classifyProviderError for these causes.
	// Without this, a user Stop during a Drain, CancelAll/shutdown with a
	// Drain in flight, Ctrl-C/--timeout, a watchdog stall (surfaces as
	// context.Canceled), or the agent legitimately asking a question all
	// closed the debt as a permanent failure and wrote a visible
	// wake-failed marker -- breaking "graceful exit = crash" and losing a
	// question the model was waiting on an answer to. Route to the recheck
	// set instead: the debt stays open, retried by a fresh hint or the 60s
	// pass, never settled on non-attempt evidence.
	var awaiting *AwaitingAnswerError
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.As(runErr, &awaiting) {
		c.addToRecheckSet(job.owner)
		return
	}
	// Real per-attempt evidence (wakeSession's capture): classify from the
	// row THIS attempt itself wrote, exactly like shouldRetryTurn/
	// shouldContinueTurn do for an ordinary turn -- but ONLY when that row
	// actually exists. A total request failure (e.g. every retry inside a
	// single fantasy.RetryError hitting the same 503) commonly reports an
	// OnAssistantMessageCreated id for a step that never persisted content
	// at all (no row ever committed, or it was rolled back) -- attempt-
	// EvidenceID != "" does not by itself mean the row is findable. Treat
	// "reported an id but can't find it" the SAME as "no evidence wired"
	// (the isProviderClassifiable fallback below), not as a reason to
	// refuse classification outright -- that was this function's actual
	// bug (found via TestSettleByFailure_KThreeViaRealReleaseHookAndRecheckPass,
	// a real end-to-end run against a real failing HTTP provider): wake_
	// attempts never incremented, K=3 was never reachable, and a genuinely
	// permanent provider failure never settled.
	if attemptAssistantMsgID != "" {
		if msg, ok := c.ownAttemptAssistantMessage(ctx, job.owner, attemptAssistantMsgID); ok {
			fp := msg.FinishPart()
			if fp == nil || fp.Reason != message.FinishReasonError {
				// This attempt's own row did not end in an error finish --
				// it reacted (or otherwise made a clean finish). runErr does
				// not describe THIS attempt's own failure; nothing to settle.
				return
			}
			if turnMadeProgress(msg) {
				// Partial content already written before the failure: a
				// fresh Drain re-derives and re-answers from the DB at its
				// own next pull (Drains are regenerable, doc sec.3.4's
				// wakeSession doc) -- not settled by failure here, unlike an
				// ordinary turn there is no risk of losing or duplicating a
				// user prompt.
				return
			}
			// An error-finish, no-progress row: fall through to classify
			// runErr below, same as the no-evidence path would for a
			// provider-shaped error.
		} else if !isProviderClassifiable(runErr) {
			c.addToRecheckSet(job.owner)
			return
		}
	} else if !isProviderClassifiable(runErr) {
		// No per-attempt evidence wired for this caller (the CLI loop's own
		// turns -- ReactionDebtSource.RecordDrainTurnOutcome, item 1) and
		// runErr is not even provider-shaped: nothing here proves the
		// PROVIDER actually failed (a DB error in the preamble looks
		// exactly like this). Route to the recheck set instead of falling
		// into classifyProviderError's terminal default.
		c.addToRecheckSet(job.owner)
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

// errDrainProgressNotRecorded is checkStuckDrainProgress's settle-by-failure
// cause: the turn itself reported success, but the debt it was supposed to
// react to is still open.
var errDrainProgressNotRecorded = errors.New("the assistant responded, but its reaction to this event was not recorded")

// checkStuckDrainProgress implements W-DRAIN item 2 (C5c, docs/reviews/
// 2026-09-29-async-phase4-round1.md): a Drain attempt that reached the
// provider, produced real content, and returned NO error from wakeSession's
// (or the CLI loop's) point of view can still leave its captured snapshot's
// debt rows open -- doc sec.3.4's reaction write commits in the SAME DB
// transaction as the step's own content persist, and fantasy's OnStepFinish
// hook has no way to surface a failure of that write back to the caller as
// a turn error (it "swallows" it, per the finding). Left unchecked, the
// ordinary hint mechanism relaunches a fresh, PAID provider turn every time
// afterward, forever, since success never had ANY bound the way failure
// does (drainFailureSettleThreshold). This reuses that SAME wake_attempts/
// K=3 counter for the success case: hasContent must be a CONFIRMED signal
// (wakeSession's own per-attempt finish-reason evidence, or the CLI loop's
// non-empty FinalText) -- hasContent=false (no evidence at all, e.g. every
// bare-mock SessionAgent test in this package, which never touches the DB)
// is always a no-op, so this never fires on a mock's simulated "success"
// with no real reaction machinery behind it.
func (c *coordinator) checkStuckDrainProgress(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, hasContent bool) {
	if !hasContent || c.asyncJobs == nil || c.asyncJobs.store == nil || snapshot.Empty() {
		return
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c.incrementThenSettleIfThreshold(settleCtx, job, snapshot, errDrainProgressNotRecorded)
}

// settleAndMark closes debt on exactly snapshot's rows and writes the
// visible marker (doc sec.3.4/ASYNC-09) -- A10/item 1 fix: settle and marker
// insert happen in ONE transaction via SettleReactedFailedWithMarker
// (internal/session, agent `store`'s fix), which inserts the marker IFF at
// least one row was actually settled. This closes two gaps the old two-step
// SettleReactedFailed+InsertSessionNotice sequence had: (a) a marker-insert
// failure after the settle committed silently lost the "why" (ASYNC-09);
// (b) settling a snapshot that turned out to already be fully resolved (a
// queued user turn behind this Drain reacted to it first) wrote a spurious
// marker unconditionally -- the atomic method's settled-count gate means
// this simply becomes a no-op instead.
//
// Marker text names job.toolCallID only when it is a real, model-facing
// tool_call_id (an ordinary async-job/delegation wake) -- the release-
// recheck/CLI-loop/60s-pass/supervision paths key it on internal pseudo-ids
// ("release-recheck", "cli-loop", "recheck-pass", "supervision-<uuid>";
// jobIdentity is diagnostic-only, doc sec.3.4's wakeSession doc), which must
// never leak into a notice text the model or operator reads -- see
// isPseudoJobID.
func (c *coordinator) settleAndMark(ctx context.Context, job jobIdentity, snapshot session.DebtSnapshot, cause error) {
	var text string
	if isPseudoJobID(job.toolCallID) {
		text = fmt.Sprintf(
			"Не удалось продолжить работу после ошибки провайдера: %s. Событие сохранено; продолжение — при следующем ходе.",
			cause,
		)
	} else {
		// A real, model-facing tool_call_id (an ordinary async-job/
		// delegation wake) is useful context for the operator/model reading
		// the marker later -- only wakeSession's OWN internal pseudo-ids
		// (release-recheck, cli-loop, recheck-pass, supervision-<uuid>) must
		// never leak into notice text (item 1 fix).
		text = fmt.Sprintf(
			"Не удалось продолжить работу после события %s: %s. Событие сохранено; продолжение — при следующем ходе.",
			job.toolCallID, cause,
		)
	}
	settled, err := c.asyncJobs.store.SettleReactedFailedWithMarker(ctx, job.owner, snapshot, text)
	if err != nil {
		slog.Error("settle-by-failure: settle reacted failed with marker failed", "session_id", job.owner, "err", err)
		return
	}
	if settled == 0 {
		slog.Debug("settle-by-failure: captured snapshot already fully resolved, nothing settled",
			"session_id", job.owner)
	}
}
