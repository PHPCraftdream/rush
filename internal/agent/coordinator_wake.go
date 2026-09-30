// wakeSession: the single wake primitive (phase-4 step 3, docs/plans/
// 2026-09-28-async-phase4-durable-core.md sec.3.4). It no longer persists
// anything itself -- by the time any caller reaches wakeSession, the fact is
// already durably committed: a non-sync async job's terminal transition
// already set its async_jobs row's delivery='pending' (work_ledger_
// transition.go), or the caller already inserted a session_notices row
// (agent_notice_pull.go's InsertSessionNotice, called from
// coordinator_background.go/work_ledger_timeout.go/supervision.go). This
// function's only remaining job is: if wake is true, submit a Drain call
// (agent_drain.go) to the session's owning driver -- idle becomes owner like
// any call, busy queues (mailbox_queue.go's mergeQueuedCall). The Drain's own
// turn-start pull is what actually moves the notice into history; wakeSession
// itself never builds notice text or a NoticeKind anymore.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/PHPCraftdream/rush/internal/session"
)

// jobIdentity is the (owner, toolCallID) pair a completion's wake hint is
// keyed on. toolCallID is diagnostic-only now (logging) -- wake no longer
// needs an idempotency key of its own: a Drain call is regenerable (its
// turn-start pull re-derives whatever it would have reacted to), so there is
// nothing to deduplicate by job identity the way the old noticedBefore map
// did.
type jobIdentity struct {
	owner      string
	toolCallID string
}

// wakeSession implements doc sec.3.4's "who triggers a Drain": on wake=true
// it ALWAYS bumps the session's non-blocking hint (a committed transition/
// notice with wake=1 must be visible to every hint-driven waiter regardless
// of what happens next), then either lets an external driver's own hint
// suffice (a live `rush run` loop, doc sec.3.4's session-with-an-external-
// driver row) or submits a Drain call after the session policy check --
// checked BEFORE submission, so a forbidden session never gets one. A no-op
// when wake is false (doc sec.3.4's wake-policy table: Stop/job_kill/a
// failed wake-up marker never wake). A Drain failure cannot lose the fact --
// it stays debt for the next pull -- and recordDrainOutcome accounts for it:
// a visible wake-failed marker (session_notices, wake=0) is written once the
// debt is closed by failure (ASYNC-09), not on every failed attempt.
// wakeSessionAttemptSeam is a test-only hook, called once per wakeSession
// invocation (after the wake==false short-circuit). Lets a test count/bound
// how many times a self-perpetuating release->recheck chain re-enters this
// function without hanging the test on a genuinely unbounded recursion (B2/
// C2 regression coverage). nil (a no-op) in every production path.
var wakeSessionAttemptSeam func()

func (c *coordinator) wakeSession(ctx context.Context, job jobIdentity, wake bool) (err error) {
	if !wake {
		return nil
	}
	if wakeSessionAttemptSeam != nil {
		wakeSessionAttemptSeam()
	}
	if c.asyncJobs == nil {
		return errors.New("wakeSession: async job ledger unavailable")
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wakeSession panic", "session_id", job.owner, "job_id", job.toolCallID,
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("wakeSession panic: %v", r)
		}
	}()

	c.asyncJobs.bumpHint(job.owner)
	if c.asyncJobs.isExternalDriver(job.owner) {
		// Doc sec.3.4: "sessions with an external driver get NO Drain turn,
		// only a hint to the loop" -- the hint above already delivered that;
		// the loop re-evaluates its own scope/debt from the DB.
		return nil
	}

	allowed, counted, polErr := c.sessionDrainPolicy(ctx, job.owner)
	if polErr != nil {
		// A policy-check error is not authoritative either way -- the
		// Drain call's OWN turn-start debt+policy re-check (agent_turn.go)
		// is what actually decides whether a provider turn runs, so failing
		// open here costs at most one redundant queued Drain, never a
		// correctness gap.
		slog.Warn("wakeSession: session drain policy check failed; submitting anyway",
			"session_id", job.owner, "err", polErr)
		allowed, counted = true, true
	}
	if !allowed {
		return nil
	}

	call, buildErr := c.drainCallFor(ctx, job.owner)
	if buildErr != nil {
		return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
	}

	agent := c.agentFor(job.owner)

	// B2/C2 fix (doc sec.3.4 rule (b)): probe the session's OS lock with a
	// SHARED, non-blocking hold BEFORE ever attempting a Run. Winning it is
	// kernel-attested proof no exclusive holder exists right now; contention
	// means another process is genuinely mid-turn on this session, so
	// submitting anyway would only fail identically to the last attempt and
	// (via abandonOwnershipWithHandoff's release hook) invite an immediate
	// re-check with no pause. Go straight to the recheck set instead of
	// spending an attempt whose outcome is already known. A type-assertion
	// miss (a mock SessionAgent in tests) or any non-contention probe error
	// (permission, IO, ...) is not conclusive either way -- fall through to
	// the ordinary Run attempt below, which remains the authoritative check.
	if sa, ok := agent.(*sessionAgent); ok && sa.dataDir != "" {
		probe, probeErr := session.TryHoldSessionLockShared(sa.dataDir, job.owner)
		if probeErr != nil {
			var busyErr *session.SessionLockBusyError
			if errors.As(probeErr, &busyErr) {
				c.addToRecheckSet(job.owner)
				return nil
			}
		} else {
			probe.Release()
		}
	}

	admission := newTurnAdmission()
	// B7 fix: wired so runTurn's onDrainTurnStarting call (agent_turn.go,
	// fired only once the Drain actually commits to a provider turn -- never
	// for a merely-queued or no-turn Drain) marks THIS invocation's own
	// recorder, never an ancestor's (mirrors turnAdmission's existing
	// queued-flag isolation).
	call.onDrainTurnStarting = admission.markReachedProvider
	// The leg is accounted by the turn loop that runs it (drain_attempt.go),
	// never here: a queued or merged Drain has no launcher.
	_, runErr := agent.Run(withTurnAdmission(ctx, admission), call)
	if runErr != nil {
		slog.Warn("drain call failed after its underlying notice was committed",
			"session_id", job.owner, "job_id", job.toolCallID, "err", runErr)
		return runErr
	}
	// B7 fix: count only a Drain that ACTUALLY reached the provider.
	if counted && admission.didReachProvider() {
		if already, _ := ctx.Value(capAlreadyCountedCtxKey{}).(bool); !already {
			c.bumpConsecutiveResume(job.owner)
		}
	}
	return nil
}
