package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RunQueuePump is a background goroutine that scans the durable run queue
// for pending or stale-leased entries and attempts to execute them.
//
// It lives independently of any specific request/turn, surviving process
// restarts and ensuring that once a call is enqueued (durable), it will
// eventually be executed by some process.
//
// Design principles (from task #340, P0-2 review):
//   - Independent of request/turn lifecycle (not a child goroutine of Run())
//   - One pump per process (or per dataDir - resolved to "per process" here)
//   - Never acquires the session OS lock itself — it drives execution
//     through Coordinator.Run, which goes through the normal
//     TryAcquireSessionLock path exactly like any other caller (a
//     SessionLockBusyError from ordinary lock contention is handled by
//     executeEntry's NackRunQueueEntryNoAttemptPenalty path, see below —
//     the pump does not pre-check or avoid contention, it just doesn't
//     let contention drain the durable queue).
//   - Respects ErrCallAlreadyAttempted as terminal (no retry)
//   - Graceful shutdown via context cancellation
//
// Pump interval: 3 seconds (fast enough to pick up orphaned work quickly,
// but not so fast as to cause excessive lock contention or DB polling).
const RunQueuePumpInterval = 3 * time.Second

// Orphan outbox drain interval: 15 seconds (fallback path for when main
// queue was temporarily unavailable. Longer than main tick since this is
// for rare recovery scenarios, not normal operation).
const OrphanOutboxDrainInterval = 15 * time.Second

// Lease TTL: 30 seconds. A pump that crashes while holding a lease will
// release it within this window, allowing another pump instance to pick it up.
const RunQueueLeaseTTL = 30 * time.Second

// MaxAttempts: 10 retries before giving up (transient errors only).
// ErrCallAlreadyAttempted-type errors respect terminal_failure flag and
// are removed immediately regardless of attempts count.
const RunQueueMaxAttempts = 10

// MaxConcurrentExecutions: maximum number of executeEntry goroutines that
// can run simultaneously across all sessions. Without this limit, a process
// crash followed by a large backlog would spawn one goroutine per pending
// entry in a single tick, potentially creating hundreds of concurrent
// provider streams, DB connections, and subprocesses — indistinguishable
// from a hang and prone to rate-limit/retry storms.
// 10 is a reasonable default for typical resource constraints: one turn
// can hold ~1-2 HTTP connections (LLM request + tools) plus a few SQLite
// writers, so 10 concurrent turns stay within practical limits for a
// single-process Crush instance. Each session still has its own inFlight
// guard, so multiple entries for the same session never run concurrently
// even if the pool is not full.
const RunQueueMaxConcurrentExecutions = 10

// LeaseWatchdogSafetyMargin: the safety margin before lease expiry at which
// the watchdog cancels execution. This margin accounts for:
//   - Scheduling delays between watchdog timer firing and execCtx cancellation
//   - Time for cancellation to propagate through Coordinator.Run to actual LLM/tool calls
//   - Time for provider/tool code to respect cancellation (in-flight requests may complete)
//
// Production: 5 seconds means we cancel at least 5s before the lease actually expires.
// A separate pump instance therefore cannot legitimately take ownership until at least
// 5s after this executor has stopped, providing a strong bound against double-execution.
// The comment about "bounded to one TTL residual window" (P1-2 original fix) was too
// optimistic: with a fixed 30s DB timeout per renewal, an executor could continue up to
// 40s (TTL + full timeout) after the lease expired. The watchdog closes this gap.
const LeaseWatchdogSafetyMargin = 5 * time.Second

// DrainSessionNowPollInterval is the production poll granularity
// DrainSessionNow uses while waiting for a same-pump in-flight execution it
// lost the lease race against (see DrainSessionNow's own doc). Kept short:
// the wait is bounded by an ordinary turn's remaining duration, not a full
// lease TTL, and this is the loop's only source of added latency in that
// branch.
const DrainSessionNowPollInterval = 25 * time.Millisecond

// Coordinator interface is a minimal subset for executing queued calls.
// We use this instead of importing the full agent.Coordinator to avoid
// import cycles (session → agent → session). The real app's AgentCoordinator
// implements this interface.
type Coordinator interface {
	// Run executes a call for the given session using the full call data.
	// Returns the result or an error. Implementations MUST return
	// ErrCallQueuedNotExecuted (not a nil error) if the call was merely
	// appended to an already-owned session's in-process queue rather than
	// actually executed by this call — see that error's doc.
	Run(ctx context.Context, call SessionAgentCallData) (*any, error)
}

// ErrCallQueuedNotExecuted is returned by Coordinator.Run when the target
// session was already owned by a live, in-process turn at the moment of the
// call: the call was appended to that owner's own mailbox queue to be run
// as a later turn, not executed by this call. This is NOT a failure of the
// call and NOT a signal that the current owner is a stale/foreign process —
// it can legitimately be this same pump instance's OWN prior execution of a
// different (or the same, self-raced) entry for that session; see the
// RunQueuePump.inFlight field's doc, which prevents the pump from ever
// causing this itself. What remains, once inFlight closes the self-inflicted
// paths, is a genuinely external live owner (e.g. a web/CLI request running
// concurrently in this same process outside pump control).
//
// executeEntry treats this specially (see there): it must NOT be handled
// like an ordinary success (would Ack/delete a durable row for work that
// has not actually run yet — the queued-mailbox copy is only as durable as
// the owning process staying alive long enough to drain it) and must NOT be
// retried on every tick like an ordinary failure either (mailbox.submit
// unconditionally appends on every call, so nacking-and-retrying every tick
// would append a new duplicate of the same call on every attempt, all of
// which the owner eventually runs when it drains its queue).
//
// Fixed by the fourth @oh review pass over #337-349: an earlier version of
// this doc (and the code) left the entry exactly as leased and untouched,
// relying on RunQueueLeaseTTL's natural expiry (via CleanupExpiredLeases) as
// the only recovery path — but that cleanup unconditionally counts an
// attempt on every recovery, so a session that stayed externally busy for a
// few lease windows would have its accepted, never-actually-failed work
// silently dead-lettered. The entry is instead released immediately via
// NackRunQueueEntryNoAttemptPenalty (never counts an attempt) paired with a
// local RunQueuePump.busyBackoffUntil deadline that stops THIS pump instance
// specifically from re-attempting the same session before a full lease
// window has passed — see that field's doc for the full rationale,
// including why a naive lease-renewal-based backoff was tried first and
// found not to work.
var ErrCallQueuedNotExecuted = errors.New("run_queue_pump: call was queued into an already-owned session, not executed")

// errLeaseLost is returned by executeEntrySync when the entry's lease was
// reassigned to a different owner during the renewal loop (see leaseLost's
// doc there). It carries no outcome for the row itself — the new owner
// writes the eventual Ack/Nack/TerminalFail — callers just need to know
// nothing further can be learned from this particular execution attempt.
var errLeaseLost = errors.New("run_queue_pump: lost lease ownership mid-execution")

// RunQueuePumpConfig configures a RunQueuePump instance.
type RunQueuePumpConfig struct {
	// Sessions is the session service for enqueue/lease/ack operations.
	Sessions Service

	// Coordinator is the agent coordinator that will execute leased calls.
	// Set to nil for pump instances that only scan/cleanup (no execution).
	Coordinator Coordinator

	// PumpInstanceID is a unique identifier for this pump instance (used for leased_by).
	// Defaults to a generated UUID if empty.
	PumpInstanceID string

	// TestTick is a test seam for overriding the pump interval.
	// nil = use production RunQueuePumpInterval.
	TestTick func() time.Duration

	// TestLeaseTTL is a test seam for overriding RunQueueLeaseTTL. 0 = use
	// the production constant. RunQueueLeaseTTL (30s) is not itself
	// test-overridable, which made the lease-renewal-during-a-long-turn
	// scenario (see executeEntry's renewal loop) impossible to exercise
	// deterministically and quickly — a real test would need to block a
	// fake Coordinator.Run call for 30+ real seconds to observe the race
	// this seam exists to close. Found needed by the fourth @oh review
	// pass over #337-349, which noted the existing tests could not
	// exercise post-expiry behavior at all.
	TestLeaseTTL time.Duration

	// TestMaxConcurrentExecutions is a test seam for overriding
	// RunQueueMaxConcurrentExecutions. 0 = use the production constant.
	// Allows regression tests to force an artificially small pool size
	// to verify the bounded-concurrency guarantee holds even under
	// extreme backlog pressure.
	TestMaxConcurrentExecutions int

	// TestDBWriteTimeout is a test seam for overriding the 30s budget
	// newDBCtx() gives each individual outcome/renewal DB write in
	// executeEntry. 0 = use the production 30s. Added to deterministically
	// and quickly reproduce a real regression found in the closing review
	// of the release-readiness round: a single dbCtx created once at
	// executeEntry's entry, before Coordinator.Run, was reused for the
	// post-Run outcome write too — so any turn longer than 30s hit an
	// already-expired context on its Ack/Nack/TerminalFail call. Without
	// this seam, exercising that bug needs a real Coordinator.Run call that
	// blocks for 30+ real seconds.
	TestDBWriteTimeout time.Duration

	// TestLeaseWatchdogSafetyMargin is a test seam for overriding
	// LeaseWatchdogSafetyMargin. 0 = use the production 5s. Allows tests
	// to verify the watchdog fires at the right time even with very short
	// TTLs (where a fixed 5s margin would be longer than the TTL itself).
	TestLeaseWatchdogSafetyMargin time.Duration

	// TestDrainTick is a test seam for overriding the orphan outbox drain interval.
	// nil = use production OrphanOutboxDrainInterval.
	TestDrainTick func() time.Duration

	// TestDrainSessionPollInterval is a test seam for overriding
	// DrainSessionNowPollInterval, the poll granularity DrainSessionNow uses
	// while waiting for a same-pump in-flight execution it lost the lease
	// race against. 0 = use the production constant. Regression tests need
	// this small (sub-millisecond) to observe the race deterministically
	// without a real wall-clock wait.
	TestDrainSessionPollInterval time.Duration
}

// leaseTTL returns the effective lease TTL for this pump instance —
// cfg.TestLeaseTTL if set, otherwise the production RunQueueLeaseTTL.
func (p *RunQueuePump) leaseTTL() time.Duration {
	if p.cfg.TestLeaseTTL > 0 {
		return p.cfg.TestLeaseTTL
	}
	return RunQueueLeaseTTL
}

// maxConcurrentExecutions returns the effective maximum concurrent
// executions for this pump instance — cfg.TestMaxConcurrentExecutions
// if non-zero, otherwise the production RunQueueMaxConcurrentExecutions.
func (p *RunQueuePump) maxConcurrentExecutions() int {
	if p.cfg.TestMaxConcurrentExecutions > 0 {
		return p.cfg.TestMaxConcurrentExecutions
	}
	return RunQueueMaxConcurrentExecutions
}

// dbWriteTimeout returns the effective per-write DB context budget —
// cfg.TestDBWriteTimeout if set, otherwise the production 30 seconds.
func (p *RunQueuePump) dbWriteTimeout() time.Duration {
	if p.cfg.TestDBWriteTimeout > 0 {
		return p.cfg.TestDBWriteTimeout
	}
	return 30 * time.Second
}

// leaseWatchdogSafetyMargin returns the effective safety margin for the
// lease watchdog — cfg.TestLeaseWatchdogSafetyMargin if set, otherwise
// the production LeaseWatchdogSafetyMargin (5s).
//
// The effective margin is clamped to at most TTL/2 to prevent the watchdog
// from firing immediately in test configs where the margin would be larger
// than the TTL itself.
func (p *RunQueuePump) leaseWatchdogSafetyMargin() time.Duration {
	margin := LeaseWatchdogSafetyMargin
	if p.cfg.TestLeaseWatchdogSafetyMargin > 0 {
		margin = p.cfg.TestLeaseWatchdogSafetyMargin
	}
	// Clamp margin to at most TTL/2 to prevent immediate firing
	ttl := p.leaseTTL()
	if margin > ttl/2 {
		margin = ttl / 2
	}
	return margin
}

// drainSessionPollInterval returns the effective poll interval
// DrainSessionNow uses while waiting on a same-pump in-flight execution —
// cfg.TestDrainSessionPollInterval if set, otherwise the production
// DrainSessionNowPollInterval.
func (p *RunQueuePump) drainSessionPollInterval() time.Duration {
	if p.cfg.TestDrainSessionPollInterval > 0 {
		return p.cfg.TestDrainSessionPollInterval
	}
	return DrainSessionNowPollInterval
}

// RunQueuePump is a background pump for the durable run queue.
type RunQueuePump struct {
	cfg     RunQueuePumpConfig
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	startMu sync.Mutex

	// workerWg tracks all executeEntry goroutines, ensuring Stop() waits
	// for them to finish before returning. Critical for P0-3: without this,
	// Stop() can return while workers are still writing Ack/Nack/TerminalFail
	// to the database, causing rows to remain leased/pending after restart.
	// Pattern documented for reuse in P0-4 (agent.Summarize) and P1-1.
	workerWg sync.WaitGroup

	// admitMu/stopping form the admission gate that closes a real "Add
	// concurrently with Wait" race an earlier version of this fix
	// introduced: processEntry checked p.ctx.Err() and then called
	// workerWg.Add(1) as two separate, unsynchronized steps. Between the
	// check and the Add, Stop() could cancel p.ctx and start its
	// workerWg.Wait() goroutine while the counter was still 0 — Wait()
	// could then return (nothing to wait for) and Stop() could report
	// "stopped gracefully" a moment before the Add(1) actually landed,
	// which either panics ("Add called concurrently with Wait", per the
	// sync.WaitGroup contract: "calls with a positive delta that start
	// when the counter is zero must happen before a Wait") or, if it
	// doesn't panic, lets a brand-new worker start after Stop() has
	// already told the caller (App.Shutdown) it is safe to close the DB —
	// reintroducing the exact class of bug P0-3 exists to close, just one
	// level down.
	//
	// The fix: stopping is only ever flipped false->true, and every read
	// (processEntry) and the one write (Stop) happen under admitMu. So
	// either processEntry's critical section runs entirely before Stop's
	// (stopping was still false: Add(1) is guaranteed to complete, and
	// complete-before, Stop's subsequent cancel()+Wait()-spawn), or it
	// runs entirely after (stopping is already true: processEntry bails
	// out and never calls Add at all). There is no interleaving left where
	// Add and Wait can race.
	admitMu  sync.Mutex
	stopping bool

	// inFlight tracks session IDs with an executeEntry goroutine currently
	// running, guarded by inFlightMu. Found by the third @oh review pass
	// over #337-349 (in-range for #340's original design): Coordinator.Run
	// returns (nil, nil) when the target session is already owned
	// in-process — the call was merely appended to the current owner's
	// mailbox queue (mailbox.submit), not actually executed. executeEntry
	// treated err == nil as unconditional success and Acked (deleted) the
	// durable row regardless, so a second concurrent dispatch for the same
	// session could delete a row whose only remaining copy of the work now
	// lives purely in an in-memory mailbox queue — lost for good if that
	// process crashes before draining it, or silently re-run as a
	// duplicate turn once it does drain.
	//
	// This is reachable two ways:
	//   1. Same-tick, no lease-expiry involved: tick() leases and dispatches
	//      entries one at a time in a single pass, so two distinct
	//      durably-queued entries for the same session — e.g. two calls
	//      queued while a process was down — could be leased and
	//      `go executeEntry`-dispatched back to back within the same tick,
	//      before either had run long enough to matter.
	//   2. Sequential, via lease expiry: RunQueueLeaseTTL (30s) is far
	//      shorter than a real LLM turn can take. Without the lease-renewal
	//      loop in executeEntry (added in the fourth @oh review pass, see
	//      that function's own doc), CleanupExpiredLeases could return an
	//      entry to pending while the goroutine executing it was still
	//      genuinely running, and the next tick would then lease and
	//      dispatch a SECOND, sequential (not concurrent) goroutine for the
	//      same session.
	//
	// inFlight closes path 1 at the source: processEntry refuses to lease
	// a pending entry whose session already has an executeEntry goroutine
	// running FROM THIS PUMP INSTANCE, so this pump can never concurrently
	// dispatch two entries for the same session. It does NOT, by itself,
	// close path 2 — an inFlight session that loses its lease to
	// CleanupExpiredLeases still shows as busy in this map (the goroutine
	// is still running), so a same-tick duplicate is still prevented, but
	// nothing stopped the durable row from flipping to pending underneath
	// it and being picked up by a LATER tick once execution (wrongly)
	// looked done to a stale reader. Path 2 is closed by executeEntry's
	// lease-renewal loop keeping the row genuinely 'leased' for the whole
	// duration of a long turn, not by inFlight. See executeEntry's own
	// Run()-result handling for the narrower residual case neither
	// mechanism covers: a genuinely external, non-pump owner (e.g. a live
	// user-facing process) holding the session when the pump attempts it.
	inFlight   map[string]struct{}
	inFlightMu sync.Mutex

	// busyBackoffUntil tracks, per session ID, a local deadline before
	// which this pump instance will not attempt to lease a pending entry
	// for that session again — guarded by busyBackoffMu. Used exclusively
	// for the ErrCallQueuedNotExecuted outcome (see executeEntry): an
	// EARLIER fix tried achieving backoff purely via RenewRunQueueLease,
	// but that call happens almost instantly after the original lease was
	// taken (Coordinator.Run returns near-immediately for this outcome),
	// so it barely extends lease_expires_at beyond what leasing already
	// set — CleanupExpiredLeases still reaped the row (and incremented
	// attempts) after essentially one ordinary TTL window, no different
	// from doing nothing. Confirmed by a failing test before this design
	// was adopted: attempts still reached RunQueueMaxAttempts in the
	// expected ~10 TTL windows, not the "far more than 10" the fix was
	// supposed to guarantee.
	//
	// This map is the actual fix: on ErrCallQueuedNotExecuted, the entry
	// is immediately released via NackRunQueueEntryNoAttemptPenalty
	// (status flips straight to 'pending', attempts untouched — cleanup
	// never even sees a 'leased' row to charge an attempt against), and
	// this pump additionally records a LOCAL backoff deadline so its own
	// processEntry does not immediately re-lease and re-dispatch the same
	// entry into the same busy owner's mailbox on the very next tick
	// (mailbox.submit has no dedup — a tight retry loop would append a
	// new duplicate call on every attempt). A DIFFERENT pump instance (a
	// separate process) is free to attempt the row immediately — its
	// in-process mailbox ownership is independent of this one's, so there
	// is no reason to block it just because this process happens to be
	// busy.
	busyBackoffUntil map[string]time.Time
	busyBackoffMu    sync.Mutex

	// execSem is a bounded semaphore that limits total concurrent
	// executeEntry goroutines across all sessions. Unlike inFlight
	// (which is per-session), this prevents unbounded fan-out after
	// a process crash with a large backlog — without it, a single
	// tick would spawn one goroutine per pending entry, potentially
	// creating hundreds of concurrent provider streams, DB connections,
	// and subprocesses. P1-4 fix (docs/reviews/2026-08-11).
	execSem chan struct{}

	// Test seam for waiting for a tick in tests
	tickCh chan struct{}
}

// NewRunQueuePump creates a new RunQueuePump instance.
func NewRunQueuePump(cfg RunQueuePumpConfig) *RunQueuePump {
	if cfg.PumpInstanceID == "" {
		cfg.PumpInstanceID = fmt.Sprintf("pump-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &RunQueuePump{
		cfg:              cfg,
		ctx:              ctx,
		cancel:           cancel,
		inFlight:         make(map[string]struct{}),
		busyBackoffUntil: make(map[string]time.Time),
		tickCh:           make(chan struct{}, 1),
	}
	// execSem must be initialized after p is constructed, so maxConcurrentExecutions() works
	p.execSem = make(chan struct{}, p.maxConcurrentExecutions())
	return p
}
