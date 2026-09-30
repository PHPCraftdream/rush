package session_test

// Deterministic time for RunQueuePump tests: a manually advanced clock plus a
// Service decorator that keeps the DB's lease stamps on that same clock.
// Fake time never moves on its own, so an assertion about "across N TTL
// windows" cannot be broken by wall-clock scheduling.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
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

func (c *fakePumpClock) NewTicker(d time.Duration) session.PumpTicker {
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
	session.Service
	clk *fakePumpClock

	leases            atomic.Int64 // LeaseRunQueueEntry calls that claimed a row
	renewals          atomic.Int64 // pump renewals that landed
	cleanups          atomic.Int64 // CleanupExpiredLeases calls that returned
	lastCleanupBefore atomic.Int64 // cutoff of the latest cleanup, Unix seconds
}

func newFakeClockService(svc session.Service, clk *fakePumpClock) *fakeClockService {
	return &fakeClockService{Service: svc, clk: clk}
}

func (s *fakeClockService) LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, ttl time.Duration) (*session.RunQueueEntry, error) {
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

// awaitPump waits for a pump-side event that must eventually happen. It is a
// hang guard, not a timing window: fake time does not move while it waits.
func awaitPump(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 15*time.Second, 2*time.Millisecond, msg)
}

// TestFakePumpClock_TickersFireOnAdvanceAndCoalesce pins the fake's own
// contract: a ticker fires only once its period has elapsed, and periods
// missed while the receiver is slow coalesce into a single tick.
func TestFakePumpClock_TickersFireOnAdvanceAndCoalesce(t *testing.T) {
	t.Parallel()
	clk := newFakePumpClock(fakePumpEpoch)
	tk := clk.NewTicker(time.Minute)

	clk.Advance(59 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("ticker fired before its period elapsed")
	default:
	}

	clk.Advance(time.Second)
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker did not fire when its period elapsed")
	}

	clk.Advance(10 * time.Minute)
	<-tk.C()
	select {
	case <-tk.C():
		t.Fatal("missed periods must coalesce into one tick")
	default:
	}

	tk.Stop()
	require.Zero(t, clk.liveTickers())
	clk.Advance(time.Hour)
	select {
	case <-tk.C():
		t.Fatal("a stopped ticker must not fire")
	default:
	}
}
