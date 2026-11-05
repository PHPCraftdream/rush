package server

import (
	"bytes"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The panic-window tests recover the handler's panic on purpose. Two things
// make a one-off failure on a CI runner diagnosable instead of a bare message:
// the recovered value is checked (a different panic, say a nil dereference in
// the coordinator, must not pass as the expected one), and every timeout or
// failed assertion prints the stacks of the goroutines inside the rerun handler.

// panicRecorder stores the value the handler goroutine panicked with.
type panicRecorder struct {
	mu  sync.Mutex
	val any
	hit bool
}

// capture must be deferred directly: recover only works in the deferred func.
func (r *panicRecorder) capture() {
	v := recover()
	r.mu.Lock()
	r.val, r.hit = v, v != nil
	r.mu.Unlock()
}

func (r *panicRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.hit {
		return "no panic was recovered"
	}
	return fmt.Sprintf("recovered panic: %v", r.val)
}

func (r *panicRecorder) value() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.val
}

// panicWindowGoroutines returns the stacks of goroutines that are inside the
// rerun handler or the reservation code, capped for a readable CI log.
func panicWindowGoroutines() string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "handleRerunMessage") || strings.Contains(g, "ReserveExclusive") || strings.Contains(g, "RunWithReservedOwnership") {
			keep = append(keep, g)
		}
	}
	out := fmt.Sprintf("goroutines in the rerun handler / reservation code: %d\n%s", len(keep), strings.Join(keep, "\n\n"))
	if len(out) > 12000 {
		out = out[:12000] + "\n...(truncated)"
	}
	return out
}

func TestPanicRecorderReportsValueOrAbsence(t *testing.T) {
	t.Parallel()

	rec := &panicRecorder{}
	require.Equal(t, "no panic was recovered", rec.String())

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rec.capture()
		panic("seam boom")
	}()
	<-done
	require.Equal(t, "seam boom", rec.value())
	require.Contains(t, rec.String(), "seam boom")

	quiet := &panicRecorder{}
	done = make(chan struct{})
	go func() {
		defer close(done)
		defer quiet.capture()
	}()
	<-done
	require.Nil(t, quiet.value())
	require.Equal(t, "no panic was recovered", quiet.String())
}

func TestPanicWindowGoroutinesDumpsHandlerStacks(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		// A frame named like the real handler's callee is enough for the filter.
		blockedInReserveExclusive(entered, release)
	}()
	t.Cleanup(func() { close(release) })
	<-entered

	dump := panicWindowGoroutines()
	require.Contains(t, dump, "blockedInReserveExclusive")
	require.NotContains(t, dump, "goroutines in the rerun handler / reservation code: 0")
}

//go:noinline
func blockedInReserveExclusive(entered, release chan struct{}) {
	close(entered)
	<-release
}
