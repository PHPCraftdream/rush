package sessiontest

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTimeline is a per-test event log with elapsed times. It is never printed
// on a passing run: ReportPumpOnFailure emits it, with the pump state, only
// when the test has failed, so a one-off CI flake leaves its own evidence.
type TestTimeline struct {
	start    time.Time
	mu       sync.Mutex
	events   []string
	last     map[string]string
	reported bool
}

func NewTestTimeline() *TestTimeline { return &TestTimeline{start: time.Now()} }

func (tl *TestTimeline) Addf(format string, args ...any) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, fmt.Sprintf("+%-10s %s", time.Since(tl.start).Round(time.Millisecond), fmt.Sprintf(format, args...)))
}

// Stepf runs fn and records how long it took, so a disk or scheduler stall
// shows up as one slow step rather than as an unexplained timeout.
func (tl *TestTimeline) Stepf(fn func(), format string, args ...any) {
	began := time.Now()
	tl.Addf("%s started", fmt.Sprintf(format, args...))
	defer func() { tl.Addf("%s took %s", fmt.Sprintf(format, args...), time.Since(began).Round(time.Millisecond)) }()
	fn()
}

func (tl *TestTimeline) String() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var latest []string
	for key, value := range tl.last {
		latest = append(latest, key+": "+value)
	}
	sort.Strings(latest)
	return "timeline:\n" + strings.Join(tl.events, "\n") + "\nlast observations:\n" + strings.Join(latest, "\n") + "\n"
}

func (tl *TestTimeline) Observef(key, format string, args ...any) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.last == nil {
		tl.last = make(map[string]string)
	}
	tl.last[key] = fmt.Sprintf("+%s %s", time.Since(tl.start).Round(time.Millisecond), fmt.Sprintf(format, args...))
}

func (tl *TestTimeline) Report(t interface {
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

func ReportPumpSetupOnFailure(t *testing.T, tl *TestTimeline) {
	t.Cleanup(func() {
		tl.Report(t, func() string { return "--- pump setup/cleanup failure report ---\n" + tl.String() })
	})
}

// PumpFailureReport renders what ReportPumpOnFailure prints.
func PumpFailureReport(t *testing.T) string {
	return "--- pump failure report (printed only because the test failed) ---\n" + PumpDiagnostics(t)
}

// ReportPumpOnFailure registers the failure report. Register it after the
// pump's Stop cleanup: cleanups run last-in-first-out, so the report is taken
// while the pump is still running.
func ReportPumpOnFailure(t *testing.T, tl *TestTimeline) {
	RegisterPumpDiag(t, tl.String)
	t.Cleanup(func() {
		tl.Report(t, func() string { return PumpFailureReport(t) })
	})
}
