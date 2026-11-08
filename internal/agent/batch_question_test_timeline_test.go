package agent

// questionTestTimeline is a bounded in-memory event log for the held-question
// regressions. It prints ONLY from a t.Cleanup that checks t.Failed(), so a
// green run stays silent and a red one carries the full decision trail:
// fixture identities, notice delivery/wake/reacted flips, route counts,
// inject/await verdicts with expected-vs-actual, and errors. Entries hold
// fixture-generated ids and states only — never provider data or secrets.
// Bounded: the oldest entries fall off beyond questionTestTimelineCap.
// Mirror of the print-on-failure pattern of batchAgentTraceFor
// (batch_exec_agent_trace_test.go), but WITHOUT goroutine dumps — events
// only. No sleeps, no raised timeouts: the only timing knobs are the
// existing shrink vars and bounded require.Eventually windows.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type questionTestTimeline struct {
	mu    sync.Mutex
	start time.Time
	lines []string
}

const questionTestTimelineCap = 200

// newQuestionTestTimeline returns the test's timeline, creating it (and the
// failed-only cleanup) on first use.
func newQuestionTestTimeline(t *testing.T) *questionTestTimeline {
	t.Helper()
	v, loaded := questionTestTimelines.LoadOrStore(t, &questionTestTimeline{start: time.Now()})
	tl := v.(*questionTestTimeline)
	if !loaded {
		t.Cleanup(func() {
			if t.Failed() {
				t.Log("question timeline:\n" + tl.dump())
			}
			questionTestTimelines.Delete(t)
		})
	}
	return tl
}

var questionTestTimelines sync.Map // *testing.T -> *questionTestTimeline

// addf records one timestamped event; the ring buffer drops the oldest
// entries beyond questionTestTimelineCap.
func (tl *questionTestTimeline) addf(format string, args ...any) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	elapsed := time.Since(tl.start).Seconds()
	tl.lines = append(tl.lines, fmt.Sprintf("%+0.3fs %s", elapsed, fmt.Sprintf(format, args...)))
	if len(tl.lines) > questionTestTimelineCap {
		tl.lines = tl.lines[len(tl.lines)-questionTestTimelineCap:]
	}
}

func (tl *questionTestTimeline) dump() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return strings.Join(tl.lines, "\n")
}
