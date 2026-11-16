package sessiontest

import (
	"bytes"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pumpDiags holds, per test, the state describers that AwaitPump prints when
// its condition never becomes true.
var pumpDiags sync.Map // *testing.T -> *pumpDiagSet

type pumpDiagSet struct {
	mu  sync.Mutex
	fns []func() string
}

// RegisterPumpDiag adds a state describer printed on an AwaitPump timeout of t.
func RegisterPumpDiag(t *testing.T, fn func() string) {
	v, _ := pumpDiags.LoadOrStore(t, &pumpDiagSet{})
	set := v.(*pumpDiagSet)
	set.mu.Lock()
	set.fns = append(set.fns, fn)
	set.mu.Unlock()
	t.Cleanup(func() { pumpDiags.Delete(t) })
}

// RegisterPumpCalls adds a coordinator's call counter to the failure report.
func RegisterPumpCalls(t *testing.T, name string, calls *atomic.Int64) {
	RegisterPumpDiag(t, func() string { return fmt.Sprintf("coordinator %s: calls=%d\n", name, calls.Load()) })
}

// PumpDiagnostics renders the registered describers plus the stacks of the
// pump's goroutines, capped so a CI log stays readable.
func PumpDiagnostics(t *testing.T) string {
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

// AwaitPump waits for a pump-side event that must eventually happen. It is a
// hang guard, not a timing window: fake time does not move while it waits.
// When the condition never holds it prints the pump's observable state (fake
// clock, probe counters, queue rows, pump goroutine stacks), so a flake that
// shows up once on a CI runner carries its own evidence.
func AwaitPump(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	if report, ok := AwaitPumpFor(t, 15*time.Second, cond, msg); !ok {
		t.Fatal(report)
	}
}

// AwaitPumpFor polls cond until timeout; on failure it returns the report
// AwaitPump prints.
func AwaitPumpFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Sprintf("awaitPump timed out after %s: %s\n--- pump state at timeout ---\n%s", timeout, msg, PumpDiagnostics(t)), false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", true
}

func NotifyLiveIdentity(ch chan<- string, id string) {
	select {
	case ch <- id:
	default:
	}
}

func NotifyAdmissionRefusal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
