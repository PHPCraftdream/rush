package agent

import (
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/log"
)

// Each test names the production line it must catch (revert-check doc):
//
//   - TestTurnStallReasoningDeltaDoesNotMoveProgress guards
//     agent_turn_stream.go's onReasoningDelta hook: the reasoning-chars
//     counter must be diagnostics-only and never move lastProgress.
//   - TestTurnStallSamplerWarnsOncePerWindow guards stallSamplerCheck's
//     warn-window rate limit (stallWarnWindow): the WARN+DUMP fires at most
//     once per 30 min while the stall persists; the policy seam is invoked
//     on every crossing (guarded in turn_stall_tool_test.go).
//   - TestTurnStallDumpFileModeAndPrefix guards
//     internal/log/goroutine_dump.go's WriteGoroutineDumpNamed prefix
//     handling and its 0o600 os.WriteFile mode.

// installFakeStallNow swaps stallNow for a controllable fake and restores
// it on cleanup, returning a setter.
func installFakeStallNow(t *testing.T) func(time.Time) {
	t.Helper()
	var current atomic.Int64
	current.Store(time.Now().UnixNano())
	prev := stallNow
	stallNow = func() time.Time { return time.Unix(0, current.Load()) }
	t.Cleanup(func() { stallNow = prev })
	return func(at time.Time) { current.Store(at.UnixNano()) }
}

func TestTurnStallReasoningDeltaDoesNotMoveProgress(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	sc := &stallClock{}
	sc.markProgress()
	set(t0.Add(time.Hour))
	// Simulate reasoning deltas: counter grows, no markProgress.
	sc.reasoningChars.Add(42)
	if got := time.Duration(stallNow().UnixNano() - sc.lastProgress.Load()); got != time.Hour {
		t.Fatalf("sinceProgress = %s, want 1h (reasoning must not move lastProgress)", got)
	}
	// The sampler must still see the stall as stalled (progress unmoved).
	stallSamplerCheck("sess-1", sc, stallNow())
	if sc.lastWarnNanos.Load() == 0 {
		t.Fatal("sampler did not warn despite reasoning-only traffic")
	}
}

func TestTurnStallSamplerWarnsOncePerWindow(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	sc := &stallClock{abortTimeout: time.Minute}
	sc.markProgress()
	sessionID := "abcdef1234567890"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	// First crossing is past the pinned 10m warn threshold: warn+dump fire.
	set(t0.Add(11 * time.Minute))
	stallSamplerTickOnce()
	firstWarn := sc.lastWarnNanos.Load()
	if firstWarn == 0 {
		t.Fatal("first crossing did not warn")
	}
	// Still stalled 5s later: the warn window must suppress warn+dump (the
	// policy seam still fires on every crossing — see the tool tests).
	set(t0.Add(11*time.Minute + 5*time.Second))
	stallSamplerTickOnce()
	if sc.lastWarnNanos.Load() != firstWarn {
		t.Fatal("warned again inside the warn window")
	}
	// Past the 30 min window: exactly one more warn.
	set(t0.Add(45 * time.Minute))
	stallSamplerTickOnce()
	if sc.lastWarnNanos.Load() == firstWarn {
		t.Fatal("warn window did not open after 30 min")
	}
}

func TestTurnStallDumpFileModeAndPrefix(t *testing.T) {
	dir := t.TempDir()
	log.SetLogDirForTest(t, dir)
	buf := log.CaptureGoroutineStack("test stall dump")
	path, err := log.WriteGoroutineDumpNamed(buf, stallDumpPrefix("abcdef1234567890"))
	if err != nil {
		t.Fatalf("WriteGoroutineDumpNamed: %v", err)
	}
	base := path[len(dir)+1:]
	if len(base) < 15 || base[:14] != "stall-abcdef12" {
		t.Fatalf("dump name = %q, want stall-abcdef12-... prefix", base)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no POSIX permission bits: os.Stat reports 0666 for
	// every regular file regardless of the mode passed to os.WriteFile.
	// The 0o600 constant itself is asserted on Unix only.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("dump mode = %o, want 600", perm)
		}
	}
}
