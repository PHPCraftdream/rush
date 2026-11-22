package cmdtest

import (
	"strings"
	"testing"
	"time"
)

// WaitOrDump returns promptly when the channel closes.
func TestWaitOrDump_ReturnsOnClose(t *testing.T) {
	ch := make(chan struct{})
	close(ch)
	if err := WaitOrDump(t, ch, time.Second, "already-closed channel"); err != nil {
		t.Fatalf("expected nil error on closed channel, got %v", err)
	}
}

// TestWaitOrDump_TimesOutWithDump pins the deadline: a channel that never
// closes must produce an error (with the awaited subject named) within the
// given delay, not hang. Revert-check: removing the time.After case (making
// the helper wait on ch unconditionally) turns this test into a hang, red
// under any external -timeout — that hang IS the mutant's failure signal.
// The elapsed guard below keeps the non-mutant run fast even if the helper
// regresses into a busy form that leaks past the deadline without error.
func TestWaitOrDump_TimesOutWithDump(t *testing.T) {
	start := time.Now()
	err := WaitOrDump(t, make(chan struct{}), 20*time.Millisecond, "never-closing channel")
	if err == nil {
		t.Fatal("expected a timeout error for a channel that never closes")
	}
	if !strings.Contains(err.Error(), "never-closing channel") {
		t.Fatalf("error must name the awaited subject, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitOrDump returned only after %s — the deadline is not being honored", elapsed)
	}
}
