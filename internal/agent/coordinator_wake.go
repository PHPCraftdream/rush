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
// failed wake-up marker never wake). Errors are visible (ASYNC-09): a Drain
// failure after the fact was already committed cannot lose the fact -- it
// stays pending/pending-debt for the NEXT pull -- so this persists a durable
// wake-failed marker (session_notices, wake=0) instead of just logging.
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
	snapshot, snapErr := c.asyncJobs.captureDebtSnapshot(ctx, job.owner)
	if snapErr != nil {
		slog.Warn("wakeSession: capture debt snapshot failed; settle-by-failure will see an empty set",
			"session_id", job.owner, "err", snapErr)
	}
	_, runErr := agent.Run(withTurnAdmission(ctx, admission), call)
	if runErr != nil {
		// The task is already terminal and its fact already durably
		// committed -- this failure is about the DELIVERY turn, not the
		// job. A queued admission is reported via (nil, nil) by Run's own
		// contract, never as an error, so any non-nil error here is a real
		// failure to even queue/start (e.g. shutdown, session lock busy).
		slog.Warn("drain call failed after its underlying notice was committed",
			"session_id", job.owner, "job_id", job.toolCallID, "err", runErr)
	}
	// admission.wasQueued(): this specific call merely joined another
	// owner's queue rather than running just now -- that later turn's own
	// in-turn debt+policy re-check governs it; nothing to account for here
	// yet (see recordDrainOutcome's doc).
	c.recordDrainOutcome(ctx, job, snapshot, !admission.wasQueued(), runErr)
	if runErr != nil {
		return runErr
	}
	// B7 fix: count only a Drain that ACTUALLY reached the provider -- never
	// one that was merely admitted/queued (admission.wasQueued(), already
	// excluded from recordDrainOutcome's accounting above but NOT previously
	// excluded here) or took the no-turn branch (no debt, or policy forbade
	// a turn -- decideDrainTurn returned false, so onDrainTurnStarting never
	// fired). Before this fix EVERY successful wakeSession call incremented
	// the counter regardless, so a queued or no-turn Drain could exhaust the
	// cap purely by being re-submitted, never running a single real turn.
	if counted && admission.didReachProvider() {
		if already, _ := ctx.Value(capAlreadyCountedCtxKey{}).(bool); !already {
			// Doc sec.3.4: "failed Drains do not consume the web auto-turn
			// cap" -- only reached on runErr == nil, i.e. a genuinely
			// accepted (started or queued) turn. Skipped when the caller
			// (notifyBackgroundJobDone) already bumped this same wake
			// itself -- see capAlreadyCountedCtxKey's doc.
			c.bumpConsecutiveResume(job.owner)
		}
	}
	return nil
}
