package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type panicTimeline struct {
	mu     sync.Mutex
	start  time.Time
	events []string
}

func (tl *panicTimeline) add(event string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.start.IsZero() {
		tl.start = time.Now()
	}
	tl.events = append(tl.events, fmt.Sprintf("+%s %s", time.Since(tl.start).Round(time.Millisecond), event))
}

func (tl *panicTimeline) report(t interface {
	Failed() bool
	Log(...any)
}, rec *panicRecorder) {
	if !t.Failed() {
		return
	}
	tl.mu.Lock()
	events := strings.Join(tl.events, "\n")
	tl.mu.Unlock()
	t.Log("panic-window timeline:\n" + events + "\n" + rec.String() + "\n" + panicWindowGoroutines())
}

type panicReportSink struct {
	failed bool
	logs   []any
}

func (s *panicReportSink) Failed() bool    { return s.failed }
func (s *panicReportSink) Log(args ...any) { s.logs = append(s.logs, args...) }

func TestPanicTimelineReportsOnlyOnFailure(t *testing.T) {
	tl := &panicTimeline{}
	tl.add("fixture setup started")
	tl.add("handler launched")
	rec := &panicRecorder{}
	sink := &panicReportSink{}
	tl.report(sink, rec)
	if len(sink.logs) != 0 {
		t.Fatal("successful test emitted diagnostics")
	}
	sink.failed = true
	tl.report(sink, rec)
	report := fmt.Sprint(sink.logs...)
	for _, want := range []string{"panic-window timeline:", "fixture setup started", "handler launched", "no panic was recovered", "goroutines in the rerun handler"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q: %s", want, report)
		}
	}
}
