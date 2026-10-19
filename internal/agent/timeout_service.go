// The single per-process deadline timer (timeoutService), moved verbatim
// from work_ledger_timeout.go (R12-H1): supervision.go's check-in deadlines
// share this service, and supervision survives the H4 work_ledger deletion.
package agent

import (
	"container/heap"
	"sync"
	"time"
)

// timeoutEntry is one heap item: a deadline plus the action to run when it
// elapses. Generalizes the heap beyond asyncJob (phase 2) so a later
// consumer of the same single per-process timer -- supervision.go's
// root-session check-in -- shares it instead of starting a second worker
// goroutine (design doc §7's "one service, not a second timer"); arm keeps
// wrapping this for the asyncJob case so every existing call site is
// unchanged.
type timeoutEntry struct {
	deadline time.Time
	fire     func()
}

// jobDeadlineHeap is a container/heap over armed deadlines, ordered
// soonest-first.
type jobDeadlineHeap []timeoutEntry

func (h jobDeadlineHeap) Len() int           { return len(h) }
func (h jobDeadlineHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h jobDeadlineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *jobDeadlineHeap) Push(x any)        { *h = append(*h, x.(timeoutEntry)) }
func (h *jobDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = timeoutEntry{}
	*h = old[:n-1]
	return item
}

// timeoutService is the single per-process timer for every asyncJob with a
// non-zero deadline: ONE goroutine sleeping until the nearest deadline,
// never one goroutine per job. Lazy deletion: an entry popped from the heap
// is re-checked against the job's CURRENT state under l.mu before acting --
// a job that already reached a terminal state via finish/cancelSession/
// recheckChild (the ordinary CAS race, ASYNC-03) makes the popped entry a
// silent no-op, exactly like every other transitionToTerminal race.
type timeoutService struct {
	ledger    *workLedger
	mu        sync.Mutex
	heap      jobDeadlineHeap
	wake      chan struct{} // buffered 1: "recompute the nearest deadline"
	stop      chan struct{}
	closeOnce sync.Once // workLedger.close() is not itself guaranteed single-call (e.g. CancelAll during shutdown); close() below must survive a repeat
}

func newTimeoutService(l *workLedger) *timeoutService {
	s := &timeoutService{ledger: l, wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go s.run()
	return s
}

// arm adds job to the heap. l.mu need not be held by the caller (arm takes
// its own s.mu, a DIFFERENT lock than l.mu, to avoid making the ledger's hot
// path -- every Start/finish/cancelSession -- contend on the same mutex the
// timer goroutine polls). s.mu and l.mu are never held nested in either
// order (arm/fire release s.mu before acquiring l.mu, see fireDue/
// workLedger.handleTimeout), so this cannot deadlock.
func (s *timeoutService) arm(job *asyncJob) {
	if s == nil || job == nil {
		return
	}
	s.armFunc(job.deadline, func() { s.ledger.handleTimeout(job) })
}

// armFunc adds an arbitrary (deadline, fire) pair to the single per-process
// heap -- arm above is the asyncJob-specific wrapper every existing call
// site uses; supervision.go's root-session check-in is the second, more
// general caller this generalization exists for (see timeoutEntry's doc).
// Same locking contract as arm: s.mu is a DIFFERENT lock than l.mu, taken
// and released here without ever calling into the ledger, so this cannot
// nest under or deadlock against anything holding l.mu.
func (s *timeoutService) armFunc(deadline time.Time, fire func()) {
	if s == nil || fire == nil {
		return
	}
	s.mu.Lock()
	heap.Push(&s.heap, timeoutEntry{deadline: deadline, fire: fire})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *timeoutService) run() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		d := time.Hour
		if s.heap.Len() > 0 {
			d = time.Until(s.heap[0].deadline)
			if d < 0 {
				d = 0
			}
		}
		s.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		select {
		case <-s.stop:
			return
		case <-s.wake:
			continue
		case <-timer.C:
			s.fireDue()
		}
	}
}

func (s *timeoutService) fireDue() {
	now := time.Now()
	for {
		s.mu.Lock()
		if s.heap.Len() == 0 || s.heap[0].deadline.After(now) {
			s.mu.Unlock()
			return
		}
		entry := heap.Pop(&s.heap).(timeoutEntry)
		s.mu.Unlock()
		entry.fire()
	}
}

// close stops the timer goroutine. Nil-safe so an isolated ledger test that
// never wires a timeoutService can still call workLedger.close() unconditionally.
func (s *timeoutService) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() { close(s.stop) })
}
