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

// The launch decision (R-ARB-2): readTurnFacts + decide is THE decision;
// drainPolicy/drainPermitted are now thin adapters translating the arbiter's
// verdict into the drainVerdict the internal callers and tests read. The old
// policy/gate split is gone -- the gate rows (8-10 of the arbiter table) are
// part of the same verdict. The reaction-chain guard's CONDITION is rule 5
// (computed in readTurnFacts); this file executes its marker.

// arbiterVerdict reads the facts for one launch site and decides. The site
// encodes `spent`: only siteFact (a committed fact's own launch) spends a
// bg-shell auto-resume slot.
func (c *coordinator) arbiterVerdict(ctx context.Context, sessionID string, site LaunchSite) (Verdict, TurnFacts, error) {
	facts, err := c.readTurnFacts(ctx, sessionID, site)
	if err != nil {
		return Verdict{}, facts, err
	}
	return decide(facts), facts, nil
}

// drainVerdictOf maps an arbiter verdict onto the drainVerdict kinds the
// callers (wakeSession's switch, the CLI loop, decideDrainTurn) still speak.
// Texts and recheck-ness mirror today's policy exactly:
//   - rerun hold and a gate pause read paced and are worth a tick (recheck);
//   - a dormant gate reads stuck;
//   - a foreign live driver is deferred but worth a tick;
//   - every other policy refusal (suspension, chain guard, released child,
//     cap, auto-resume off) is deferred without a tick.
func drainVerdictOf(v Verdict) drainVerdict {
	switch v.Kind {
	case VRun, VNone:
		return drainVerdict{kind: drainAllow}
	}
	switch v.Reason {
	case "rerun in progress":
		return drainVerdict{kind: drainPaced, recheck: true, reason: v.Reason}
	case "retry pause after an unreacted attempt":
		return drainVerdict{kind: drainPaced, retryAt: v.RecheckAt, recheck: true, reason: v.Reason}
	case "repeated unreacted attempts":
		return drainVerdict{kind: drainStuck, retryAt: v.RecheckAt, reason: v.Reason}
	}
	recheck := v.Reason == "another process drives the session"
	return drainVerdict{kind: drainDeferred, recheck: recheck, reason: v.Reason}
}

// unreadable wraps a read error as a fail-closed deferred verdict (worth a
// tick), as the old policy's per-input handler did.
func unreadablePolicyInput(what string, sessionID string, err error) drainVerdict {
	slog.Warn("drain policy: input unreadable; refusing the turn for now",
		"session_id", sessionID, "input", what, "err", err)
	v := drainVerdict{kind: drainDeferred, recheck: true, reason: what + " unreadable"}
	v.err = err
	return v
}

// siteForSpent maps the legacy `spent` flag onto the launch site: a fact's
// own launch spent its slot at admission; everything else is a re-check.
func siteForSpent(spent bool) LaunchSite {
	if spent {
		return siteFact
	}
	return siteRelease
}

// drainPolicy is the launch verdict (the arbiter's), with the
// reaction-chain guard's marker executed on a web-driven session's first
// deferred read. spent: see siteForSpent.
func (c *coordinator) drainPolicy(ctx context.Context, sessionID string, spent bool) drainVerdict {
	v, facts, err := c.arbiterVerdict(ctx, sessionID, siteForSpent(spent))
	if err != nil {
		return unreadablePolicyInput("launch decision input", sessionID, err)
	}
	if v.Kind == VNone && v.Reason == "no debt" {
		// The old policy refused a session regardless of debt (a foreign
		// driver, a suspension, a released child refused even a debt-free
		// one); the LAUNCHERS keep their own debt checks (drainDecision,
		// CLIScope, decideDrainTurn), so rule 1 never launched anything for
		// a debt-free session. Re-ask the arbiter without rule 1's
		// short-circuit to keep that contract; a VRun answer on a debt-free
		// session is still "nothing to launch" (allow).
		facts.Debt.PendingIncl = true
		v = decide(facts)
		if v.Kind == VRun {
			return drainVerdict{kind: drainAllow}
		}
	}
	dv := drainVerdictOf(v)
	if dv.kind == drainDeferred && dv.reason == reactionChainReason &&
		!facts.Session.ChainNoticed && !facts.Session.ExternallyDrivenSelf {
		c.chainGuardMarker(ctx, sessionID)
	}
	return dv
}

// drainPermitted is THE launch predicate: the one arbiter verdict, mapped.
// spent: see siteForSpent.
func (c *coordinator) drainPermitted(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return c.drainPolicy(ctx, sessionID, spent)
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
	return true, c.drainPolicy(ctx, sessionID, spent), nil
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
