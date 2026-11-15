package cmd

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitOrDump waits for ch or the deadline. On deadline it dumps ALL goroutine
// stacks into the test log — so a hang becomes a fast red test with a full
// stack in the -v output instead of a package-level -timeout with a lost
// stack (task #1148) — and returns an error naming what was awaited.
func waitOrDump(t *testing.T, ch <-chan struct{}, d time.Duration, what string) error {
	t.Helper()
	select {
	case <-ch:
		return nil
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("goroutine dump after %s waiting for %s:\n%s", d, what, buf[:n])
		return fmt.Errorf("timed out after %s waiting for %s", d, what)
	}
}

// waitOrDump returns promptly when the channel closes.
func TestWaitOrDump_ReturnsOnClose(t *testing.T) {
	ch := make(chan struct{})
	close(ch)
	if err := waitOrDump(t, ch, time.Second, "already-closed channel"); err != nil {
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
	err := waitOrDump(t, make(chan struct{}), 20*time.Millisecond, "never-closing channel")
	if err == nil {
		t.Fatal("expected a timeout error for a channel that never closes")
	}
	if !strings.Contains(err.Error(), "never-closing channel") {
		t.Fatalf("error must name the awaited subject, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitOrDump returned only after %s — the deadline is not being honored", elapsed)
	}
}
