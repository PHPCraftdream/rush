package session_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestPumpFailureReportCarriesTimelineAndPumpState(t *testing.T) {
	tl := newTestTimeline()
	tl.stepf(func() { time.Sleep(3 * time.Millisecond) }, "fixture setup")
	tl.addf("pump started")
	registerPumpDiag(t, tl.String)

	report := pumpFailureReport(t)
	for _, want := range []string{"printed only because the test failed", "timeline:", "fixture setup took", "pump started", "pump goroutines:"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// The pump tests' database fixtures must use the production pragmas; with the
// driver defaults every commit fsyncs several times and a stalled CI disk turns
// fixture setup into tens of seconds.
func TestPumpFixtureDatabaseUsesProductionPragmas(t *testing.T) {
	_, _, sqlDB := setupTestSessionWithDB(t, "pragma-pin")
	var journal string
	if err := sqlDB.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var synchronous int
	if err := sqlDB.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}
