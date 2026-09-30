// The Drain session policy (doc sec.3.4's "session policy" table): may
// sessionID's category get a Drain TURN at all. Consulted by wakeSession
// BEFORE a Drain is submitted and again by the Drain's own turn-start commit
// (agent_drain_decision.go). Accounting of an attempt lives in
// drain_attempt.go.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// drainVerdictKind is the outcome of a launch decision.
type drainVerdictKind uint8

const (
	drainAllow drainVerdictKind = iota
	// drainDeferred: the policy refuses a turn for this debt (foreign driver,
	// Stop suspension, released child, bg-shell with auto-resume off, ...).
	drainDeferred
	// drainPaced: the launch gate is shut until retryAt.
	drainPaced
	// drainStuck: the gate is dormant; only a newer fact reopens it.
	drainStuck
)

// drainVerdict is one launch decision. recheck: deferred, but worth a tick
// (foreign driver, unreadable policy input).
type drainVerdict struct {
	kind    drainVerdictKind
	retryAt time.Time
	recheck bool
	reason  string
}

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
