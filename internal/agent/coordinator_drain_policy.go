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
	// Stop suspension, released child, bg-shell with auto-resume off or over
	// its cap for a re-check, ...).
	drainDeferred
	// drainPaced: the launch gate is shut until retryAt, or a rerun holds the
	// session (a temporary hold: retryAt is zero and the tick retries it).
	drainPaced
	// drainStuck: the gate is dormant -- a failing pull is reopened by a newer
	// fact, paid attempts whose close kept failing only by a human message.
	drainStuck
)

// drainVerdict is one launch decision. recheck: deferred, but worth a tick
// (foreign driver, unreadable policy input).
type drainVerdict struct {
	kind    drainVerdictKind
	retryAt time.Time
	recheck bool
	reason  string
	// err is set when the refusal is an unreadable policy input (fail closed).
	err error
}

// drainPolicy decides whether sessionID's category permits a Drain TURN at
// all (doc sec.3.4's "session policy" table): allow, or deferred (with
// recheck when the refusal is worth a tick). It is the policy half of
// drainPermitted; an unreadable input FAILS CLOSED for every session (a
// wrong "allow" here would run a released child's Drain on the root agent).
// A session driven by this process's own `rush run` loop skips the
// delegation-child refusal: its turns always run on currentAgent anyway.
// spent: the asker never compares the bg-shell cap (see the bg-shell row
// below): a fact's own launch (wakeSession, fact=true) already spent its
// auto-resume slot. A release, tick or CLI-scope re-check, and the Drain's
// turn-start commit (decideDrainTurn: a queued Drain decides on debt its
// launch never saw), pass false and compare the cap without spending a slot.
func (c *coordinator) drainPolicy(ctx context.Context, sessionID string, spent bool) drainVerdict {
	l := c.asyncJobs
	if l == nil {
		return drainVerdict{kind: drainAllow}
	}
	deferred := func(reason string, recheck bool) drainVerdict {
		return drainVerdict{kind: drainDeferred, reason: reason, recheck: recheck}
	}
	unreadable := func(what string, err error) drainVerdict {
		slog.Warn("drain policy: input unreadable; refusing the turn for now",
			"session_id", sessionID, "input", what, "err", err)
		v := deferred(what+" unreadable", true)
		v.err = err
		return v
	}
	own := l.isExternalDriver(sessionID)
	if !own {
		// A live `rush run` loop in ANOTHER process reacts to this session's
		// debt itself; this process only transfers notices.
		foreign, err := l.foreignLiveDriver(ctx, sessionID)
		if err != nil {
			return unreadable("driver marker", err)
		}
		if foreign {
			return deferred("another process drives the session", true)
		}
	}
	// A rerun holds the session while it cancels, truncates and hands off. A
	// hold is TEMPORARY -- the debt is neither abandoned nor drained, and the
	// turn the rerun hands off to is the session's next work -- so it is paced
	// with no clock (the re-check tick and the release retry it), never
	// "deferred": every scope consumer reads paced as still open, while a
	// deferred verdict would let a delegated child's parent release the
	// delegation with stale text during the hold.
	if c.automaticTurnsHeld(sessionID) {
		return drainVerdict{kind: drainPaced, recheck: true, reason: "rerun in progress"}
	}
	// Stop and a pending question suspend automatic turns until a human
	// message (or a fresh delegation on a child) lifts it.
	if c.autoResumeSuspended(sessionID) {
		return deferred("automatic turns suspended", false)
	}
	running, err := l.hasRunningDelegationFor(ctx, sessionID)
	if err != nil {
		return unreadable("delegation state", err)
	}
	if running {
		return drainVerdict{kind: drainAllow} // a delegated child while its row runs
	}
	if !own {
		// A released or expired child must never fall through to the root
		// agent (B3/C6): the refusal is keyed on the durable delegation row.
		isChild, err := c.isDurableDelegationChild(ctx, sessionID)
		if err != nil {
			return unreadable("delegation identity", err)
		}
		if isChild {
			return deferred("released delegation child", false)
		}
	}
	// "Background shell: only with AutoResumeOnJobDone": a session whose
	// ENTIRE debt is bg-shell completions gets no turn with it off -- and, with
	// it on, none for a re-check launch (release, tick) or a Drain's turn-start
	// commit once every slot per human message is spent AND no row of the debt
	// holds a slot (bgShellCapDeferred: every row is a completion that arrived
	// over the cap). A completion's own launch (spent: claimAutoResume already
	// took its slot) never compares, and a row whose slot was spent but whose
	// launch was paced, refused, held or deferred is retried like any debt, so
	// the cap bounds the chain of automatic turns without dropping a reaction.
	autonomy := c.cfg != nil && c.autonomyEnabled()
	if autonomy && !spent {
		capped, err := c.bgShellCapDeferred(ctx, sessionID)
		if err != nil {
			return unreadable("bg-shell cap state", err)
		}
		if capped {
			return deferred("background-shell auto-resume cap reached", false)
		}
	}
	if !autonomy {
		bgOnly, err := c.sessionDebtIsBGShellOnly(ctx, sessionID)
		if err != nil {
			return unreadable("debt kinds", err)
		}
		if bgOnly {
			return deferred("background-shell completion with auto-resume off", false)
		}
	}
	return drainVerdict{kind: drainAllow}
}

// drainPermitted is THE launch predicate: the session policy, then the
// per-session launch gate (written only by the attempt accounting, the
// refusal note and the human-message reset). spent: see drainPolicy.
func (c *coordinator) drainPermitted(ctx context.Context, sessionID string, spent bool) drainVerdict {
	v := c.drainPolicy(ctx, sessionID, spent)
	if v.kind != drainAllow || c.asyncJobs == nil {
		return v
	}
	open, dormant, retryAt := c.asyncJobs.drainGateOpen(sessionID, time.Now())
	switch {
	case open:
		return v
	case dormant:
		return drainVerdict{kind: drainStuck, retryAt: retryAt, reason: "repeated unreacted attempts"}
	default:
		return drainVerdict{kind: drainPaced, retryAt: retryAt, recheck: true, reason: "retry pause after an unreacted attempt"}
	}
}

// drainDecision reads the pending-inclusive debt and, when there is some,
// the launch verdict. No debt means nothing to launch for. spent: see
// drainPolicy.
func (c *coordinator) drainDecision(ctx context.Context, sessionID string, spent bool) (debt bool, v drainVerdict, err error) {
	if c.asyncJobs == nil {
		return false, drainVerdict{}, nil
	}
	debt, err = c.asyncJobs.reactionDebtExists(ctx, sessionID)
	if err != nil || !debt {
		return false, drainVerdict{}, err
	}
	return true, c.drainPermitted(ctx, sessionID, spent), nil
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
// Retention keeps the row while the child still has a running row or
// unreacted debt (R2A-10), so the signal outlives the child's work; a
// finished delegation's child past that window is an ordinary session again.
// An unreadable answer is not "not a child": drainPolicy fails closed.
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
// drainPolicy's B-dev6 fix (item 7) to re-apply the "Background
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
func (c *coordinator) sessionDebtIsBGShellOnly(ctx context.Context, sessionID string) (bgOnly bool, err error) {
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
