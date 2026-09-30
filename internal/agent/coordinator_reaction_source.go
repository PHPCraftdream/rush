// ReactionDebtSource is the phase-4 step 4 replacement for AsyncCompletionSource
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.5): the CLI loop
// (internal/app/app_run_async.go) no longer drains an in-memory completion
// queue -- it claims itself as sessionID's external driver (so wakeSession
// hints it instead of submitting a Drain turn of its own), then re-derives
// each next turn and its own exit condition straight from the DB.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// DrainState is the launch verdict for a session's pending-inclusive reaction
// debt (drainPermitted): policy first, then the per-session launch gate.
type DrainState uint8

const (
	// DrainNone: no reaction debt.
	DrainNone DrainState = iota
	// DrainOwed: debt, and a Drain may be launched now.
	DrainOwed
	// DrainPaced: debt, retrying after an unreacted attempt or a refusal; the
	// gate opens at CLIScopeState.RetryAt (a newer event may open it sooner).
	DrainPaced
	// DrainDeferred: debt the policy will not allow a turn for (bg-shell-only
	// with AutoResumeOnJobDone off, a released delegation child, another live
	// driver, Stop's suspension, a pending question).
	DrainDeferred
	// DrainStuck: repeated unreacted attempts; no launch until a newer event or
	// a human message.
	DrainStuck
)

// CLIScopeState is the ONE answer to "what now" for a session between turns
// (CLIScope), read by the CLI loop and by a delegation child's release.
type CLIScopeState struct {
	// WorkOpen: the session has a running task row on a host not provably
	// dead -- wait for it.
	WorkOpen bool
	// Drain is the launch verdict for its reaction debt.
	Drain DrainState
	// RetryAt is when a Paced gate reopens by itself.
	RetryAt time.Time
	// Reason names why the debt is Deferred/Stuck/Paced.
	Reason string
}

// ReactionDebtSource is implemented by *coordinator; internal/app type-
// asserts for it exactly like it used to for AsyncCompletionSource.
type ReactionDebtSource interface {
	// ClaimExternalDriver marks sessionID as externally driven for the
	// lifetime of one `rush run` loop: the durable session_drivers marker
	// first (so no OTHER process starts a reaction turn for it -- see
	// drainPolicy), then the in-memory one wakeSession hints against.
	// A session already driven by another live loop is refused with
	// *session.ErrSessionDrivenElsewhere and nothing is set. The ONLY
	// degraded outcome is a data dir that cannot host an OS lock file
	// (session.ErrDriverMarkerUnavailable = the lock file cannot be created or
	// locked): a Warn, and the in-memory marker alone is set. Any other
	// failure -- a database error registering the host (SQLITE_BUSY/FULL), a
	// cancelled ctx, a failed marker write -- is returned and nothing is set:
	// the run fails before it touches the session (R3A-1).
	ClaimExternalDriver(ctx context.Context, sessionID string) error
	// ReleaseExternalDriver releases the durable marker once the loop exits
	// (always), and the in-memory one for a persistent coordinator (C4).
	ReleaseExternalDriver(ctx context.Context, sessionID string)
	// CLIScope is the CLI loop's between-turns decision (doc sec.3.5), also
	// read by a delegation child's release: it reads doc sec.3.4's debt
	// predicate PENDING-INCLUSIVE (delivery IN {pending, done} -- a
	// freshly-arrived notice is still 'pending' until a turn's own preamble
	// pull, so VISIBLE-only would wrongly report no debt) and answers with the
	// ONE launch predicate every Drain launch reads (policy, then the
	// per-session launch gate): Owed/Paced/Deferred/Stuck. Running rows are
	// read BEFORE debt: a job's terminal transition and its debt commit
	// atomically, so "row not running" then implies its debt is already
	// visible to the debt read that follows -- reversing the order could let
	// the loop exit between the two.
	CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error)
	// WaitForHint blocks until sessionID's hint counter advances, ctx is
	// done, until (when non-zero) passes, or a bounded same-process fallback
	// elapses. The caller must still re-derive its decision from CLIScope
	// afterward -- a returned hint is a reason to re-check, not itself an
	// answer.
	WaitForHint(ctx context.Context, sessionID string, until time.Time)
	// RunMaintenanceSweep runs the dead-host sweep and retention purge halves
	// of the 60s host-level pass ONCE, best-effort (B12/C14 fix, part 3), at
	// the start of a `rush run` invocation; the 60s pass ClaimExternalDriver
	// starts repeats them (with the recheck-child/recheck-set halves) while
	// the loop waits.
	RunMaintenanceSweep(ctx context.Context)
}

// ClaimExternalDriver implements ReactionDebtSource: the durable claim first (a
// refusal or a real error returns before anything is set), then the in-memory
// marker. An unavailable durable marker (no OS-lock-capable data dir, and only
// that) is logged and the in-memory marker alone is kept -- the pre-durable
// behaviour; every other claim error is returned.
func (c *coordinator) ClaimExternalDriver(ctx context.Context, sessionID string) error {
	if c.asyncJobs == nil {
		return nil
	}
	if store := c.asyncJobs.store; store != nil {
		if err := store.ClaimSessionDriver(ctx, sessionID); err != nil {
			if !errors.Is(err, session.ErrDriverMarkerUnavailable) {
				return err
			}
			slog.Warn("external driver: durable marker unavailable; other processes cannot see this loop",
				"session_id", sessionID, "err", err)
		}
	}
	c.asyncJobs.claimExternalDriver(sessionID)
	// The same 60s pass the web process runs (recheck set, parked children,
	// maintenance): the CLI root itself is a hint-only no-op for it, but its
	// delegated children's retries and refusals ride it. Stopped by CancelAll.
	c.StartRecheckTicker()
	return nil
}

// ReleaseExternalDriver implements ReactionDebtSource.
//
// C4 fix (docs/reviews/2026-09-29-async-phase4-round1.md): a NON-persistent
// coordinator (`rush run`, persistentMode never set true) keeps its root
// hint-only for the entire life of the process instead of ever releasing
// it. Before this fix, the CLI loop's own defer (app_run_async.go) released
// the marker the instant runNonInteractiveWithAsyncResults returned --
// strictly BEFORE the on-finish hook and before cmd/run.go's deferred
// App.Shutdown() actually runs (or completes CancelAll) -- so a background
// job finishing in that window found isExternalDriver false and started a
// full, PAID Drain turn on context.Background() in a process that is
// already shutting down. A one-shot CLI process has no "later web-driven
// wake for the same session id" within its own lifetime to restore ordinary
// routing FOR (a different process's coordinator has its own, separate
// in-memory marker) -- so simply never releasing it here is both safe and
// sufficient; the marker dies with the process either way.
func (c *coordinator) ReleaseExternalDriver(ctx context.Context, sessionID string) {
	if c.asyncJobs == nil {
		return
	}
	// The DURABLE marker is released unconditionally: it is what other
	// processes consult, and it must not keep naming a host that stays alive
	// until this process really exits (shutdown can take a while).
	if store := c.asyncJobs.store; store != nil {
		if err := store.ReleaseSessionDriver(ctx, sessionID); err != nil {
			slog.Warn("external driver: releasing the durable marker failed; Close is the backstop",
				"session_id", sessionID, "err", err)
		}
	}
	if !c.persistentMode.Load() {
		return // C4: the in-memory marker of a one-shot CLI process dies with it
	}
	c.asyncJobs.releaseExternalDriver(sessionID)
}

// ReactionDebtExists reads doc sec.3.4's pending-inclusive debt predicate for
// sessionID.
func (c *coordinator) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil {
		return false, nil
	}
	return c.asyncJobs.reactionDebtExists(ctx, sessionID)
}

// reactionDebtSourceHintFallback bounds WaitForHint's wait even without a
// hint or ctx cancellation. Hints live inside one process, so this is also
// the CLI loop's cross-process fallback: it re-reads the DB at least this
// often while its scope is open.
const reactionDebtSourceHintFallback = 5 * time.Second

// WaitForHint implements ReactionDebtSource.
func (c *coordinator) WaitForHint(ctx context.Context, sessionID string, until time.Time) {
	if c.asyncJobs == nil {
		return
	}
	since := c.asyncJobs.hintSeqOf(sessionID)
	wait := reactionDebtSourceHintFallback
	if !until.IsZero() {
		wait = min(wait, max(time.Until(until), 0))
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c.asyncJobs.waitForHint(waitCtx, sessionID, since)
}

// RunMaintenanceSweep implements ReactionDebtSource: the dead-host sweep and
// retention purge halves of the 60s pass (doc sec.3.6/3.7), factored out of
// RecheckPass so a `rush run` can run them once at its start.
func (c *coordinator) RunMaintenanceSweep(ctx context.Context) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return
	}
	// Doc sec.3.6/3.7: the host-level sweep over every dead host with a
	// running row -- the only cross-process fallback for a host that never
	// gets a turn/scope-evaluation of its own to trigger own-scope recovery.
	if _, err := c.asyncJobs.store.SweepDeadHosts(ctx, c.messages); err != nil {
		slog.Debug("coordinator: maintenance sweep: dead-host sweep failed", "err", err)
	}
	// Retention (doc sec.3.7): old delivered/voided rows and empty dead
	// hosts' lock files, same pass, best-effort.
	if err := c.asyncJobs.store.PurgeExpired(ctx, session.AsyncDataRetentionAge); err != nil {
		slog.Debug("coordinator: maintenance sweep: retention purge failed", "err", err)
	}
}

// CLIScope implements ReactionDebtSource (see the interface doc for the
// read order and the meaning of each field).
func (c *coordinator) CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return CLIScopeState{}, nil
	}
	workOpen, err := c.runningWorkOpen(ctx, sessionID)
	if err != nil {
		return CLIScopeState{}, err
	}
	state := CLIScopeState{WorkOpen: workOpen}
	debt, err := c.asyncJobs.store.ReactionDebtExists(ctx, sessionID)
	if err != nil {
		return CLIScopeState{}, err
	}
	if !debt {
		return state, nil
	}
	v := c.drainPermitted(ctx, sessionID)
	if v.err != nil {
		// An unreadable policy input is a read error the loop retries with a
		// pause, never a silent exit.
		return CLIScopeState{}, v.err
	}
	state.RetryAt, state.Reason = v.retryAt, v.reason
	switch v.kind {
	case drainAllow:
		state.Drain = DrainOwed
	case drainPaced:
		state.Drain = DrainPaced
	case drainDeferred:
		state.Drain = DrainDeferred
	default:
		state.Drain = DrainStuck
	}
	return state, nil
}

// runningWorkOpen is the running-row half of doc sec.3.5's scope predicate:
// true iff sessionID owns a running task row on a host not provably dead.
func (c *coordinator) runningWorkOpen(ctx context.Context, sessionID string) (bool, error) {
	// Phase-4 step 5 (doc sec.3.5/3.7): a scope check is also a "scope
	// evaluation" point -- recover this owner's own dead-host rows to
	// 'interrupted' before answering, so a row whose host just died is
	// treated as recoverable-and-gone rather than kept "open" forever (or,
	// once recovered, correctly reported closed if nothing else is
	// outstanding). Best-effort: RecoverOwnerScope never fails this call.
	c.asyncJobs.store.RecoverOwnerScope(ctx, sessionID, c.messages)
	rows, err := c.asyncJobs.store.ListRunningForOwners(ctx, []string{sessionID})
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if c.asyncJobs.store.HostNotDead(row.HostID) {
			return true, nil
		}
	}
	return false, nil
}
