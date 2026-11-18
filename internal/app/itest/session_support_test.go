package itest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	"github.com/PHPCraftdream/rush/internal/session/run/sessiontest"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

// fakePumpEpoch lies in the PAST, so every real-clock reading is later than
// every fake deadline, lease expiry and backoff end: a pump decision taken on
// the real clock reads "already expired" (the watchdog fires, a renewal is
// skipped, a lease is stamped on real time, a backoff is over) and the tests
// that watch those outcomes fail. A FUTURE epoch would do the opposite -- every
// real-clock read would look early, and the watchdog and renewal paths would
// pass silently. Whole-second, so Unix-seconds columns round-trip exactly.
var fakePumpEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

type fakePumpClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers map[*fakePumpTicker]struct{}
}

func newFakePumpClock(start time.Time) *fakePumpClock {
	return &fakePumpClock{now: start, tickers: make(map[*fakePumpTicker]struct{})}
}

func (c *fakePumpClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakePumpClock) NewTicker(d time.Duration) runq.PumpTicker {
	c.mu.Lock()
	defer c.mu.Unlock()
	tk := &fakePumpTicker{clk: c, period: d, next: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.tickers[tk] = struct{}{}
	return tk
}

// Advance moves time forward and fires every ticker that came due. Like a
// real ticker with a slow receiver, missed periods coalesce into one tick.
func (c *fakePumpClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for tk := range c.tickers {
		if c.now.Before(tk.next) {
			continue
		}
		select {
		case tk.ch <- c.now:
		default:
		}
		missed := c.now.Sub(tk.next) / tk.period
		tk.next = tk.next.Add((missed + 1) * tk.period)
	}
}

// tickerState lists each live ticker's period and next due instant.
func (c *fakePumpClock) tickerState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	for tk := range c.tickers {
		parts = append(parts, fmt.Sprintf("period=%s next=%s", tk.period, tk.next.UTC().Format(time.RFC3339)))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, "; ") + "]"
}

func (c *fakePumpClock) liveTickers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tickers)
}

type fakePumpTicker struct {
	clk    *fakePumpClock
	period time.Duration
	next   time.Time
	ch     chan time.Time
}

func (tk *fakePumpTicker) C() <-chan time.Time { return tk.ch }

func (tk *fakePumpTicker) Stop() {
	tk.clk.mu.Lock()
	defer tk.clk.mu.Unlock()
	delete(tk.clk.tickers, tk)
}

// fakeClockService stamps every fresh lease on the fake clock (the real
// service uses time.Now()) and counts the pump's lease-related calls.
type fakeClockService struct {
	coresession.Service
	clk *fakePumpClock

	seen              sync.Map     // entry ids the pump leased
	leases            atomic.Int64 // LeaseRunQueueEntry calls that claimed a row
	renewals          atomic.Int64 // pump renewals that landed
	cleanups          atomic.Int64 // CleanupExpiredLeases calls that returned
	lastCleanupBefore atomic.Int64 // cutoff of the latest cleanup, Unix seconds
}

// newFakeClockService also registers a state describer for t: on an awaitPump
// timeout the probe counters and the rows of every leased entry are printed.
func newFakeClockService(t *testing.T, svc coresession.Service, clk *fakePumpClock) *fakeClockService {
	s := &fakeClockService{Service: svc, clk: clk}
	sessiontest.RegisterPumpDiag(t, s.describe)
	return s
}

func (s *fakeClockService) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fake clock now=%s (unix %d), live tickers=%d%s\n", s.clk.Now().UTC().Format(time.RFC3339), s.clk.Now().Unix(), s.clk.liveTickers(), s.clk.tickerState())
	fmt.Fprintf(&b, "probe: leases=%d renewals=%d cleanups=%d lastCleanupBefore=%d\n", s.leases.Load(), s.renewals.Load(), s.cleanups.Load(), s.lastCleanupBefore.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ids []string
	s.seen.Range(func(k, _ any) bool { ids = append(ids, k.(string)); return true })
	sort.Strings(ids)
	for _, id := range ids {
		e, err := s.GetRunQueueEntry(ctx, id)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "entry %s: read error: %v\n", id, err)
		case e == nil:
			fmt.Fprintf(&b, "entry %s: gone (acked and deleted)\n", id)
		default:
			fmt.Fprintf(&b, "entry %s: status=%s attempts=%d leasedBy=%q leaseExpiresAt=%d leasedAt=%d terminal=%v lastError=%q\n",
				e.ID, e.Status, e.Attempts, e.LeasedBy, e.LeaseExpiresAt, e.LeasedAt, e.TerminalFailure, e.LastError)
		}
	}
	if pending, err := s.ListPendingRunQueueEntries(ctx); err != nil {
		fmt.Fprintf(&b, "pending list error: %v\n", err)
	} else {
		fmt.Fprintf(&b, "pending entries: %d\n", len(pending))
	}
	return b.String()
}

func (s *fakeClockService) LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, ttl time.Duration) (*coresession.RunQueueEntry, error) {
	e, err := s.Service.LeaseRunQueueEntry(ctx, sessionID, leasedBy, ttl)
	if err != nil || e == nil {
		return e, err
	}
	// Re-stamp on the fake clock via the embedded service (not s.Renew...,
	// which counts pump renewals). Safe: only tick() cleans up, and it is
	// blocked inside this very call.
	exp := s.clk.Now().Add(ttl).Unix()
	if _, err := s.Service.RenewRunQueueLease(ctx, e.ID, leasedBy, exp); err != nil {
		return nil, err
	}
	e.LeaseExpiresAt = exp
	s.seen.Store(e.ID, struct{}{})
	s.leases.Add(1)
	return e, nil
}

func (s *fakeClockService) RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error) {
	ok, err := s.Service.RenewRunQueueLease(ctx, id, leasedBy, newExpiresAt)
	if err == nil && ok {
		s.renewals.Add(1)
	}
	return ok, err
}

func (s *fakeClockService) CleanupExpiredLeases(ctx context.Context, beforeTime int64) error {
	err := s.Service.CleanupExpiredLeases(ctx, beforeTime)
	s.lastCleanupBefore.Store(beforeTime)
	s.cleanups.Add(1)
	return err
}

func pumpQueueSnapshot(svc coresession.Service, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	began := time.Now()
	e, err := svc.GetRunQueueEntry(ctx, id)
	if err != nil {
		return fmt.Sprintf("queue snapshot: read error=%v elapsed=%s\n", err, time.Since(began))
	}
	if e == nil {
		return "queue snapshot: row absent\n"
	}
	return fmt.Sprintf("queue snapshot: status=%s attempts=%d leasedBy=%q leasedAt=%d leaseExpiresAt=%d terminal=%v\n",
		e.Status, e.Attempts, e.LeasedBy, e.LeasedAt, e.LeaseExpiresAt, e.TerminalFailure)
}

func registerPumpQueueReport(t *testing.T, tl *sessiontest.TestTimeline, svc coresession.Service, id string) func(context.Context) (*coresession.RunQueueEntry, error) {
	sessiontest.RegisterPumpDiag(t, func() string { return pumpQueueSnapshot(svc, id) })
	return func(ctx context.Context) (*coresession.RunQueueEntry, error) {
		began := time.Now()
		e, err := svc.GetRunQueueEntry(ctx, id)
		state := "absent"
		if e != nil {
			state = fmt.Sprintf("status=%s attempts=%d leaseExpiresAt=%d", e.Status, e.Attempts, e.LeaseExpiresAt)
		}
		if err != nil {
			tl.Observef("queue read error", "err=%v ctx=%v elapsed=%s", err, ctx.Err(), time.Since(began))
		}
		tl.Observef("queue read", "%s err=%v ctx=%v elapsed=%s", state, err, ctx.Err(), time.Since(began))
		return e, err
	}
}

type countingCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *countingCoordinatorForDrain) Run(ctx context.Context, callData coresession.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}
