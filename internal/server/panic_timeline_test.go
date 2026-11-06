package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type panicTimeline struct {
	mu       sync.Mutex
	start    time.Time
	events   []string
	reported bool
	snapshot func() string
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
}, rec *panicRecorder,
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
	snapshot := tl.snapshot
	tl.mu.Unlock()
	state := ""
	if snapshot != nil {
		state = snapshot()
	}
	t.Log("panic-window timeline:\n" + tl.String() + "\n" + state + rec.String() + "\n" + panicWindowGoroutines())
}

func (tl *panicTimeline) String() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return strings.Join(tl.events, "\n")
}

func panicHandlerSnapshot(ctx context.Context, done <-chan struct{}) string {
	returned := false
	select {
	case <-done:
		returned = true
	default:
	}
	deadline, hasDeadline := ctx.Deadline()
	return fmt.Sprintf("handler returned=%v context err=%v hasDeadline=%v deadline=%s\n", returned, ctx.Err(), hasDeadline, deadline.Format(time.RFC3339Nano))
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
	calls := 0
	done := make(chan struct{})
	close(done)
	tl.snapshot = func() string { calls++; return panicHandlerSnapshot(t.Context(), done) }
	tl.report(sink, rec)
	if len(sink.logs) != 0 || calls != 0 {
		t.Fatal("successful test emitted diagnostics")
	}
	sink.failed = true
	tl.report(sink, rec)
	tl.report(sink, rec)
	if calls != 1 || len(sink.logs) != 1 {
		t.Fatalf("report lacks single snapshot: calls=%d reports=%d", calls, len(sink.logs))
	}
	report := fmt.Sprint(sink.logs...)
	for _, want := range []string{"handler returned=true", "panic-window timeline:", "fixture setup started", "handler launched", "no panic was recovered", "goroutines in the rerun handler"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q: %s", want, report)
		}
	}
}
