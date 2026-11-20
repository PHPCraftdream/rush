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
package wakectl

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
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

// Scheduler is the single per-process timer over wake_schedules.
// now/after are injectable so tests are deterministic without sleeps;
// wake is the delivery callback (production: coordinator.wakeSession in a
// goroutine, so a long Drain turn never stalls the timer loop).
type Scheduler struct {
	store *sessionwake.WakeScheduleStore
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

	wakeCh         chan struct{} // buffered 1: "recompute the nearest due time"
	stopCh         chan struct{}
	done           chan struct{} // closed after the run goroutine actually returns
	started        chan struct{} // closed after the startup lease recovery ran
	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc
	dispatchWG     sync.WaitGroup // wake deliveries dispatched by the run goroutine
	startOnce      sync.Once
	stopOnce       sync.Once
}

func NewScheduler(store *sessionwake.WakeScheduleStore, sink SessionWakeDispatcher, now func() time.Time, after func(time.Duration) <-chan time.Time) *Scheduler {
	return buildScheduler(store, "wakesched-"+uuid.NewString(), func(ctx context.Context, id string) {
		if err := sink.WakeSession(ctx, id, true); err != nil {
			slog.Debug("wake scheduler: delivery attempt did not complete", "session_id", id, "err", err)
		}
	}, now, after)
}

func buildScheduler(store *sessionwake.WakeScheduleStore, leaseOwner string, wake func(context.Context, string), now func() time.Time, after func(time.Duration) <-chan time.Time) *Scheduler {
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	return &Scheduler{
		store:          store,
		leaseOwner:     leaseOwner,
		wake:           wake,
		now:            now,
		after:          after,
		wakeCh:         make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		done:           make(chan struct{}),
		started:        make(chan struct{}),
		dispatchCtx:    dispatchCtx,
		dispatchCancel: dispatchCancel,
	}
}

// Start launches the worker goroutine. Idempotent.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Stop terminates the worker, cancels its delivery context, and waits for
// its goroutine and every wake delivery it dispatched to return. Nil-safe,
// idempotent.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.dispatchCancel()
	})
	<-s.done
	s.dispatchWG.Wait()
}

// Notify asks the run loop to recompute the nearest due time NOW — the
// recompute hook the stage-4c tools (wakein/wakeon/loop/wake_cancel) call
// after CreateSchedule/CancelSchedule. reschedule is an alias of the same
// operation: the timer's only state is NextDue, so "something changed" and
// "reschedule" are one and the same signal.
func (s *Scheduler) Notify() { s.reschedule() }

func (s *Scheduler) reschedule() {
	if s == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) run() {
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
func (s *Scheduler) nextWait() time.Duration {
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
func (s *Scheduler) fireDue() {
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
		// next pull — it is never lost and never re-fired. Track the detached
		// delivery so Stop cannot release the DB underneath wakeSession.
		s.dispatchWG.Add(1)
		go func(owner string) {
			defer s.dispatchWG.Done()
			s.wake(s.dispatchCtx, owner)
		}(row.OwnerSessionID)
	}
}

func (s *Scheduler) Now() time.Time { return s.now() }

func (s *Scheduler) CancelSchedule(ctx context.Context, owner, id string, now time.Time) error {
	return s.store.CancelSchedule(ctx, owner, id, now)
}

func SetWakeScheduleStore(slot **Scheduler, store *sessionwake.WakeScheduleStore, sink SessionWakeDispatcher) {
	if slot == nil || store == nil || *slot != nil {
		return
	}
	scheduler := NewScheduler(store, sink, time.Now, time.After)
	*slot = scheduler
	scheduler.Start()
}
