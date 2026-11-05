package agent

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// The batch agent scope tests assert on what does NOT happen within an 80 ms
// window (no report while descendants are live), so one failure on a slow
// runner reads as a bare "scope reported while ..." with no way to tell which
// transition the poller saw. batchAgentTestTrace is a timestamped log the
// fakes write to (turn finish/exit, descendant reader values, the scope's
// reports); it is printed, with the stacks of the scope's goroutines, when a
// helper times out or the test fails. All methods are nil-safe so a fake used
// outside batchAgentTestRun records nothing.
type batchAgentTestTrace struct {
	start time.Time

	mu    sync.Mutex
	lines []string
}

const batchAgentTraceCap = 200

func (tr *batchAgentTestTrace) add(format string, args ...any) {
	if tr == nil {
		return
	}
	line := fmt.Sprintf("+%7.3fs %s", time.Since(tr.start).Seconds(), fmt.Sprintf(format, args...))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.lines) >= batchAgentTraceCap {
		tr.lines = append(tr.lines[:0], tr.lines[len(tr.lines)-batchAgentTraceCap/2:]...)
	}
	tr.lines = append(tr.lines, line)
}

func (tr *batchAgentTestTrace) String() string {
	if tr == nil {
		return "(no trace)"
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.lines, "\n")
}

var batchAgentTraces sync.Map // *testing.T -> *batchAgentTestTrace

// batchAgentTraceFor returns the test's trace, creating it (and the
// print-on-failure cleanup) on first use.
func batchAgentTraceFor(t *testing.T) *batchAgentTestTrace {
	t.Helper()
	v, loaded := batchAgentTraces.LoadOrStore(t, &batchAgentTestTrace{start: time.Now()})
	tr := v.(*batchAgentTestTrace)
	if !loaded {
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(batchAgentFailureReport(t))
			}
			batchAgentTraces.Delete(t)
		})
	}
	return tr
}

// batchAgentFailureReport is the timeline plus the goroutines inside the batch
// agent scope.
func batchAgentFailureReport(t *testing.T) string {
	var tr *batchAgentTestTrace
	if v, ok := batchAgentTraces.Load(t); ok {
		tr = v.(*batchAgentTestTrace)
	}
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "batchAgentExecution") || strings.Contains(g, "batchAgentFamily") {
			keep = append(keep, g)
		}
	}
	stacks := strings.Join(keep, "\n\n")
	if len(stacks) > 12000 {
		stacks = stacks[:12000] + "\n...(truncated)"
	}
	return fmt.Sprintf("--- batch agent scope timeline ---\n%s\n--- scope goroutines: %d ---\n%s", tr, len(keep), stacks)
}

func TestBatchAgentTraceRecordsTransitionsAndReports(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	reader.set(true, true)
	turn.finish()
	turn.exitNow()
	batchAgentTestNoReport(t, reports, "descendants are live")
	reader.set(false, true)
	batchAgentTestReport(t, reports)
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))

	report := batchAgentFailureReport(t)
	require.Contains(t, report, "reader.set(live=true, complete=true)")
	require.Contains(t, report, "turn.finish()")
	require.Contains(t, report, "turn.exitNow()")
	require.Contains(t, report, "descendant read #")
	require.Contains(t, report, "-> live=true complete=true")
	require.Contains(t, report, "scope report: state=")
	require.Contains(t, report, "scope goroutines:")
}

func TestBatchAgentTraceKeepsTheNewestEventsWhenFull(t *testing.T) {
	t.Parallel()

	tr := &batchAgentTestTrace{start: time.Now()}
	for i := 0; i < batchAgentTraceCap*2; i++ {
		tr.add("event %d", i)
	}
	out := tr.String()
	require.Contains(t, out, fmt.Sprintf("event %d", batchAgentTraceCap*2-1))
	require.LessOrEqual(t, strings.Count(out, "\n")+1, batchAgentTraceCap)

	var none *batchAgentTestTrace
	none.add("ignored")
	require.Equal(t, "(no trace)", none.String())
}
