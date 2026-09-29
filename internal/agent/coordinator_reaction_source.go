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

// ReactionDebtSource is implemented by *coordinator; internal/app type-
// asserts for it exactly like it used to for AsyncCompletionSource.
type ReactionDebtSource interface {
	// ClaimExternalDriver marks sessionID as externally driven for the
	// lifetime of one `rush run` loop: the durable session_drivers marker
	// first (so no OTHER process starts a reaction turn for it -- see
	// sessionDrainPolicy), then the in-memory one wakeSession hints against.
	// A session already driven by another live loop is refused with
	// *session.ErrSessionDrivenElsewhere and nothing is set. A data dir that
	// cannot host the host lock (session.ErrDriverMarkerUnavailable) is not
	// an error: the in-memory marker alone is set.
	ClaimExternalDriver(ctx context.Context, sessionID string) error
	// ReleaseExternalDriver releases the durable marker once the loop exits
	// (always), and the in-memory one for a persistent coordinator (C4).
	ReleaseExternalDriver(ctx context.Context, sessionID string)
	// ReactionDebtExists reads doc sec.3.4's debt predicate for sessionID
	// (pending-inclusive: delivery IN {pending, done}) -- the correct
	// predicate for "is another turn owed", including a freshly-arrived
	// notice nothing has pulled into history yet. VISIBLE-only
	// (delivery='done') would wrongly report no debt for that common case,
	// since a fresh notice only becomes visible AFTER a turn's own preamble
	// pull -- see waitForNextCLITurn's own doc for the B8/C5b,c fix that
	// bounds the actually-stuck-pull case a different way (drainNoTurn ->
	// WaitForHint), without needing a different predicate here.
	ReactionDebtExists(ctx context.Context, sessionID string) (bool, error)
	// ScopeOpen evaluates doc sec.3.5's scope predicate for sessionID, always
	// BETWEEN turns: true iff the session has a running task row on a host
	// not provably dead, or a reaction debt (pending-inclusive). It checks
	// neither "mid-turn" nor the session's policy: debt a policy would refuse
	// a turn on still keeps the scope open.
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
	// producedContent reports whether the turn's own result carried a
	// non-empty final answer (item 2/C5c fix: a "successful" Drain turn
	// whose reaction write silently failed must not relaunch unbounded paid
	// turns forever -- see checkStuckDrainProgress's doc). false is always
	// safe (a no-op for this accounting), so a caller unsure of the signal
	// should pass false rather than guess true.
	RecordDrainTurnOutcome(ctx context.Context, sessionID string, snapshot session.DebtSnapshot, turnErr error, producedContent bool)
	// RunMaintenanceSweep runs the dead-host sweep and retention purge halves
	// of the 60s host-level pass ONCE, best-effort (B12/C14 fix, part 3): a
	// non-persistent coordinator (`rush run`) never starts the recurring
	// ticker (StartRecheckTicker), so without this, a CLI-only install would
	// never sweep dead hosts or purge expired rows at all -- contradicting
	// the CHANGELOG/`--jobs-older-than` help's documented promise. Does NOT
	// run the recheck-child/recheck-set halves: those are process-wide
	// concerns for the long-lived ticker, out of scope for a single-session
	// CLI invocation.
	RunMaintenanceSweep(ctx context.Context)
}

// ClaimExternalDriver implements ReactionDebtSource: the durable claim first (a
// refusal or a real error returns before anything is set), then the in-memory
// marker. An unavailable durable marker (no OS-lock-capable data dir) is
// logged and the in-memory marker alone is kept -- the pre-durable behaviour.
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

// ReactionDebtExists implements ReactionDebtSource.
func (c *coordinator) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil {
		return false, nil
	}
	return c.asyncJobs.reactionDebtExists(ctx, sessionID)
}

// reactionDebtSourceHintFallback bounds WaitForHint's wait even without a
// hint or ctx cancellation. Hints live inside one process, so this is also
// the CLI loop's only cross-process fallback: a `rush run` has no 60s ticker
// (only the web process runs RecheckPass) and re-reads the DB at least this
// often while its scope is open.
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
func (c *coordinator) RecordDrainTurnOutcome(ctx context.Context, sessionID string, snapshot session.DebtSnapshot, turnErr error, producedContent bool) {
	job := jobIdentity{owner: sessionID, toolCallID: "cli-loop"}
	// "" (no per-attempt assistant-message evidence): the CLI loop's own
	// turns go through ExecuteRun/coordinator.Run, which does not currently
	// thread an OnAssistantMessageCreated hook back out to this caller (item
	// 1, docs/reviews/2026-09-29-async-phase4-round1.md W-DRAIN). settleOr-
	// RetryDrainFailure's isProviderClassifiable fallback still refuses to
	// settle on a non-provider-shaped error (e.g. a DB error in the loop's
	// own turn-start preamble) even without per-attempt evidence.
	c.recordDrainOutcome(ctx, job, snapshot, true, turnErr, "")
	// Item 2/C5c fix: the CLI loop's OWN turns are never no-op/queued by the
	// time they reach here (ExecuteRun already ran a real turn) -- a nil
	// turnErr with real final text is exactly checkStuckDrainProgress's
	// "success but did the reaction write actually land" case.
	if turnErr == nil {
		c.checkStuckDrainProgress(ctx, job, snapshot, producedContent)
	}
}

// RunMaintenanceSweep implements ReactionDebtSource: the dead-host sweep and
// retention purge halves of the 60s pass (doc sec.3.6/3.7), factored out of
// RecheckPass so a non-persistent (CLI) coordinator can run them once per
// invocation without also running the recheck-child/recheck-set halves,
// which are process-wide concerns for the long-lived ticker.
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

// ScopeOpen implements ReactionDebtSource. Doc sec.3.5's predicate also lists
// "mid-turn" and "policy allows a turn"; neither is evaluated here: every
// caller runs strictly BETWEEN its own turns (a child's in-process turn is
// covered by childScopeDrained's IsSessionBusy check) and any reaction debt
// counts, whatever the session policy would allow.
func (c *coordinator) ScopeOpen(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, nil
	}
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
	debt, err := c.asyncJobs.store.ReactionDebtExists(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return debt, nil
}
