package itest

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
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
	session.Service
	clk *fakePumpClock

	seen              sync.Map     // entry ids the pump leased
	leases            atomic.Int64 // LeaseRunQueueEntry calls that claimed a row
	renewals          atomic.Int64 // pump renewals that landed
	cleanups          atomic.Int64 // CleanupExpiredLeases calls that returned
	lastCleanupBefore atomic.Int64 // cutoff of the latest cleanup, Unix seconds
}

// newFakeClockService also registers a state describer for t: on an awaitPump
// timeout the probe counters and the rows of every leased entry are printed.
func newFakeClockService(t *testing.T, svc session.Service, clk *fakePumpClock) *fakeClockService {
	s := &fakeClockService{Service: svc, clk: clk}
	registerPumpDiag(t, s.describe)
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

// pumpDiags holds, per test, the state describers that awaitPump prints when
// its condition never becomes true.
var pumpDiags sync.Map // *testing.T -> *pumpDiagSet

type pumpDiagSet struct {
	mu  sync.Mutex
	fns []func() string
}

// registerPumpDiag adds a state describer printed on an awaitPump timeout of t.
func registerPumpDiag(t *testing.T, fn func() string) {
	v, _ := pumpDiags.LoadOrStore(t, &pumpDiagSet{})
	set := v.(*pumpDiagSet)
	set.mu.Lock()
	set.fns = append(set.fns, fn)
	set.mu.Unlock()
	t.Cleanup(func() { pumpDiags.Delete(t) })
}

// registerPumpCalls adds a coordinator's call counter to the failure report.
func registerPumpCalls(t *testing.T, name string, calls *atomic.Int64) {
	registerPumpDiag(t, func() string { return fmt.Sprintf("coordinator %s: calls=%d\n", name, calls.Load()) })
}

// pumpDiagnostics renders the registered describers plus the stacks of the
// pump's goroutines, capped so a CI log stays readable.
func pumpDiagnostics(t *testing.T) string {
	var b strings.Builder
	if v, ok := pumpDiags.Load(t); ok {
		set := v.(*pumpDiagSet)
		set.mu.Lock()
		fns := append([]func() string(nil), set.fns...)
		set.mu.Unlock()
		for _, fn := range fns {
			b.WriteString(fn())
		}
	}
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var stacks []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "RunQueuePump") {
			stacks = append(stacks, g)
		}
	}
	fmt.Fprintf(&b, "pump goroutines: %d\n", len(stacks))
	dump := strings.Join(stacks, "\n\n")
	if len(dump) > 12000 {
		dump = dump[:12000] + "\n...(truncated)"
	}
	b.WriteString(dump)
	return b.String()
}

// awaitPump waits for a pump-side event that must eventually happen. It is a
// hang guard, not a timing window: fake time does not move while it waits.
// When the condition never holds it prints the pump's observable state (fake
// clock, probe counters, queue rows, pump goroutine stacks), so a flake that
// shows up once on a CI runner carries its own evidence.
func awaitPump(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	if report, ok := awaitPumpFor(t, 15*time.Second, cond, msg); !ok {
		t.Fatal(report)
	}
}

// awaitPumpFor polls cond until timeout; on failure it returns the report
// awaitPump prints.
func awaitPumpFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Sprintf("awaitPump timed out after %s: %s\n--- pump state at timeout ---\n%s", timeout, msg, pumpDiagnostics(t)), false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", true
}

// testTimeline is a per-test event log with elapsed times. It is never printed
// on a passing run: reportPumpOnFailure emits it, with the pump state, only
// when the test has failed, so a one-off CI flake leaves its own evidence.
type testTimeline struct {
	start    time.Time
	mu       sync.Mutex
	events   []string
	last     map[string]string
	reported bool
}

func newTestTimeline() *testTimeline { return &testTimeline{start: time.Now()} }

func (tl *testTimeline) addf(format string, args ...any) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, fmt.Sprintf("+%-10s %s", time.Since(tl.start).Round(time.Millisecond), fmt.Sprintf(format, args...)))
}

// stepf runs fn and records how long it took, so a disk or scheduler stall
// shows up as one slow step rather than as an unexplained timeout.
func (tl *testTimeline) stepf(fn func(), format string, args ...any) {
	began := time.Now()
	tl.addf("%s started", fmt.Sprintf(format, args...))
	defer func() { tl.addf("%s took %s", fmt.Sprintf(format, args...), time.Since(began).Round(time.Millisecond)) }()
	fn()
}

func (tl *testTimeline) String() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var latest []string
	for key, value := range tl.last {
		latest = append(latest, key+": "+value)
	}
	sort.Strings(latest)
	return "timeline:\n" + strings.Join(tl.events, "\n") + "\nlast observations:\n" + strings.Join(latest, "\n") + "\n"
}

func (tl *testTimeline) observef(key, format string, args ...any) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.last == nil {
		tl.last = make(map[string]string)
	}
	tl.last[key] = fmt.Sprintf("+%s %s", time.Since(tl.start).Round(time.Millisecond), fmt.Sprintf(format, args...))
}

func (tl *testTimeline) report(t interface {
	Failed() bool
	Log(...any)
}, render func() string,
) {
	if !t.Failed() {
		return
	}
	tl.mu.Lock()
	if tl.reported {
		tl.mu.Unlock()
		return
	}
	tl.reported = true
	tl.mu.Unlock()
	t.Log(render())
}

func reportPumpSetupOnFailure(t *testing.T, tl *testTimeline) {
	t.Cleanup(func() {
		tl.report(t, func() string { return "--- pump setup/cleanup failure report ---\n" + tl.String() })
	})
}

// pumpFailureReport renders what reportPumpOnFailure prints.
func pumpFailureReport(t *testing.T) string {
	return "--- pump failure report (printed only because the test failed) ---\n" + pumpDiagnostics(t)
}

// reportPumpOnFailure registers the failure report. Register it after the
// pump's Stop cleanup: cleanups run last-in-first-out, so the report is taken
// while the pump is still running.
func reportPumpOnFailure(t *testing.T, tl *testTimeline) {
	registerPumpDiag(t, tl.String)
	t.Cleanup(func() {
		tl.report(t, func() string { return pumpFailureReport(t) })
	})
}

func pumpQueueSnapshot(svc session.Service, id string) string {
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

func registerPumpQueueReport(t *testing.T, tl *testTimeline, svc session.Service, id string) func(context.Context) (*session.RunQueueEntry, error) {
	registerPumpDiag(t, func() string { return pumpQueueSnapshot(svc, id) })
	return func(ctx context.Context) (*session.RunQueueEntry, error) {
		began := time.Now()
		e, err := svc.GetRunQueueEntry(ctx, id)
		state := "absent"
		if e != nil {
			state = fmt.Sprintf("status=%s attempts=%d leaseExpiresAt=%d", e.Status, e.Attempts, e.LeaseExpiresAt)
		}
		if err != nil {
			tl.observef("queue read error", "err=%v ctx=%v elapsed=%s", err, ctx.Err(), time.Since(began))
		}
		tl.observef("queue read", "%s err=%v ctx=%v elapsed=%s", state, err, ctx.Err(), time.Since(began))
		return e, err
	}
}

func notifyLiveIdentity(ch chan<- string, id string) {
	select {
	case ch <- id:
	default:
	}
}

func notifyAdmissionRefusal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type countingCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *countingCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}
