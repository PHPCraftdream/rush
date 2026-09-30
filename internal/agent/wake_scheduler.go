// Stage 4b: the durable wake-schedule worker (docs/plans/2026-09-24-agent-
// wakes-and-async-job-control.md sec.4, wake-tools-contract.md §1/§5/§7).
// ONE goroutine per process sleeping until the earliest due schedule
// (WakeScheduleStore.NextDue), never one goroutine per schedule and never
// busy polling. On each due tick: ClaimDue -> FireOccurrence (one
// transaction: occurrence advance + the durable wake_fired notice) ->
// wake the owner through the SAME path every other durable wake=1 notice
// uses (coordinator.wakeSession; the resulting Drain's turn-start pull
// moves the notice into history — CLI via Drain, web via the auto move).
//
// Missed deadlines are NOT caught up: a schedule overdue after downtime
// fires at most one immediate occurrence (FireOccurrence's nextLoopRun
// counts from the SCHEDULED time, SCHED-3), and a machine that was off
// wakes nobody — the notice is simply delivered after the next start of
// this worker, on the next due tick.
package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// wakeSchedulerMaxWait caps the timer sleep when recomputing the nearest
// due time: schedules created or cancelled by ANOTHER process do not reach
// this worker's wake channel, so the NextDue read is refreshed at least
// this often. One SQLite read per interval is not busy polling.
const wakeSchedulerMaxWait = time.Minute

// wakeSchedulerClaimBatch bounds one ClaimDue batch per tick; the next tick
// (immediately after, via the recomputed NextDue) picks up more.
const wakeSchedulerClaimBatch = 50

// wakeScheduler is the single per-process timer over wake_schedules.
// now/after are injectable so tests are deterministic without sleeps;
// wake is the delivery callback (production: coordinator.wakeSession in a
// goroutine, so a long Drain turn never stalls the timer loop).
type wakeScheduler struct {
	store *session.WakeScheduleStore
	// leaseOwner identifies this scheduler instance in wake_schedules'
	// lease columns; unique per Start so a dead worker's leases expire
	// (SCHED-2) instead of being silently reused.
	leaseOwner string
	// wake delivers one fired occurrence to its owner session.
	wake func(ctx context.Context, sessionID string)
	now  func() time.Time
	// after returns the timer channel the run loop sleeps on for the given
	// duration; production uses time.After.
	after func(d time.Duration) <-chan time.Time

	wakeCh    chan struct{} // buffered 1: "recompute the nearest due time"
	stopCh    chan struct{}
	done      chan struct{} // closed after the run goroutine actually returns
	started   chan struct{} // closed after the startup lease recovery ran
	startOnce sync.Once
	stopOnce  sync.Once
}

// WakeScheduleController is the optional interface the App asserts the
// Coordinator to when wiring the stage-4b wake-schedule worker
// (internal/app). Deliberately NOT part of the Coordinator interface
// itself: every test fake implementing Coordinator would have to grow the
// two methods for nothing (same reasoning as
// coordinator_work_scope.go's own not-on-the-interface note).
type WakeScheduleController interface {
	// SetWakeScheduleStore hands the coordinator the durable wake-schedule
	// store and starts the stage-4b worker over it. nil (or a second call)
	// is a no-op.
	SetWakeScheduleStore(store *session.WakeScheduleStore)
	// StopWakeScheduler terminates the worker; safe to call repeatedly
	// and on a coordinator that never got a store.
	StopWakeScheduler()
}

// SetWakeScheduleStore builds this coordinator's wake scheduler over store
// and starts it. The wake delivery is coordinator.wakeSession(ctx, id,
// fact=true) in a goroutine — the same path every other durable wake=1
// session_notices row is delivered by (supervision, wake_only timeouts,
// async completions); detached so a long Drain turn never stalls the timer
// loop.
func (c *coordinator) SetWakeScheduleStore(store *session.WakeScheduleStore) {
	if c == nil || store == nil || c.wakeScheduler != nil {
		return
	}
	owner := "wakesched-" + uuid.NewString()
	s := newWakeScheduler(store, owner, func(ctx context.Context, sessionID string) {
		go func() {
			if err := c.wakeSession(context.Background(), sessionID, true); err != nil {
				slog.Debug("wake scheduler: delivery attempt did not complete", "session_id", sessionID, "err", err)
			}
		}()
	})
	c.wakeScheduler = s
	s.Start()
}

// StopWakeScheduler stops the wake-schedule worker goroutine, waiting for
// its exit. Part of App shutdown ordering: it must finish before the DB is
// released. Nil-safe.
func (c *coordinator) StopWakeScheduler() {
	if c == nil {
		return
	}
	c.wakeScheduler.Stop()
}

// WakeSchedulerNotify recomputes the worker's timer after an in-process
// schedule change (the stage-4c tools' hook; see wakeScheduler.Notify).
// No-op when the coordinator has no scheduler.
func (c *coordinator) WakeSchedulerNotify() {
	if c == nil {
		return
	}
	c.wakeScheduler.Notify()
}

// HasOpenOnceWakeSchedule reports whether sessionID owns at least one
// ACTIVE once schedule — the stage-5a "should `rush run` hold the process
// open" predicate (operator decision: a one-shot timer holds the run, a
// loop does not). Reads the store through ListSchedules and filters here,
// so no store/SQL change is needed.
func (c *coordinator) HasOpenOnceWakeSchedule(ctx context.Context, sessionID string) (bool, error) {
	if c == nil || c.wakeScheduler == nil {
		return false, nil
	}
	rows, err := c.wakeScheduler.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Kind == string(session.WakeKindOnce) && row.State == "active" {
			return true, nil
		}
	}
	return false, nil
}

func newWakeScheduler(store *session.WakeScheduleStore, leaseOwner string, wake func(ctx context.Context, sessionID string)) *wakeScheduler {
	return &wakeScheduler{
		store:      store,
		leaseOwner: leaseOwner,
		wake:       wake,
		now:        time.Now,
		after:      time.After,
		wakeCh:     make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
		started:    make(chan struct{}),
	}
}

// Start launches the worker goroutine. Idempotent.
func (s *wakeScheduler) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Stop terminates the worker and waits for its goroutine to return.
// Nil-safe, idempotent.
func (s *wakeScheduler) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.done
}

// Notify asks the run loop to recompute the nearest due time NOW — the
// recompute hook the stage-4c tools (wakein/wakeon/loop/wake_cancel) call
// after CreateSchedule/CancelSchedule. Reschedule is an alias of the same
// operation: the timer's only state is NextDue, so "something changed" and
// "reschedule" are one and the same signal.
func (s *wakeScheduler) Notify() { s.Reschedule() }

func (s *wakeScheduler) Reschedule() {
	if s == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *wakeScheduler) run() {
	defer close(s.done)
	// Startup recovery (SCHED-2/07): release leases a dead scheduler left
	// behind, then let the first NextDue read below deliver whatever came
	// due while we were off — one immediate occurrence, no catch-up.
	if _, err := s.store.RecoverExpiredLeases(context.Background(), s.now()); err != nil {
		slog.Error("wake scheduler: failed to recover expired leases", "err", err)
	}
	close(s.started)
	for {
		sched := s.nextWait()
		timer := s.after(sched)
		select {
		case <-s.stopCh:
			return
		case <-s.wakeCh:
			continue // recompute
		case <-timer:
			s.fireDue()
		}
	}
}

// nextWait returns how long the timer should sleep: until the earliest due
// schedule, capped at wakeSchedulerMaxWait so cross-process changes are
// seen even without a Notify.
func (s *wakeScheduler) nextWait() time.Duration {
	d := wakeSchedulerMaxWait
	next, err := s.store.NextDue(context.Background())
	if err != nil {
		// A read error must not spin: wait out the full cap and retry.
		slog.Error("wake scheduler: failed to read next due time", "err", err)
		return d
	}
	if next != nil {
		if until := next.Sub(s.now()); until < d {
			d = until
		}
	}
	if d < 0 {
		d = 0
	}
	return d
}

// fireDue claims and fires every currently due occurrence, then returns;
// the run loop recomputes NextDue right after (a loop's advanced
// next_run_at and a once schedule's completion both show up there).
func (s *wakeScheduler) fireDue() {
	now := s.now()
	claimed, err := s.store.ClaimDue(context.Background(), s.leaseOwner, now, wakeSchedulerClaimBatch, session.WakeLeaseTTL)
	if err != nil {
		// Nothing was lost: unclaimed rows stay due, and a claimed lease
		// expires (SCHED-2) so the next tick re-claims and re-fires.
		slog.Error("wake scheduler: claim due failed", "err", err)
		return
	}
	for _, row := range claimed {
		fire, err := s.store.FireOccurrence(context.Background(), row.ID, s.leaseOwner, now)
		if err != nil {
			// The lease expires and the occurrence is re-claimed later
			// (SCHED-2): a failed fire never loses or duplicates it, the
			// advance+notice is one transaction.
			slog.Error("wake scheduler: fire occurrence failed", "schedule_id", row.ID, "err", err)
			continue
		}
		if !fire.Fired {
			// Lease lost, cancelled mid-lease, or the owner session was
			// deleted (row cascaded away): nothing to wake and nothing to
			// retry — the row is terminal or gone, so the worker can never
			// loop on it (task item 5).
			continue
		}
		// The occurrence (and its durable wake_fired notice) is committed;
		// a delivery failure here leaves the notice pending for the owner's
		// next pull — it is never lost and never re-fired.
		s.wake(context.Background(), row.OwnerSessionID)
	}
}
