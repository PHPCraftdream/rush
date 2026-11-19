package drainctl

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	"github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}

// installStallTestPolicy swaps the turn-stall policy for fn and restores the
// previous one on cleanup.
func installStallTestPolicy(t *testing.T, fn func(string, turnPhase, time.Duration, time.Duration)) {
	t.Helper()
	prev := turnStallPolicy.Load()
	setTurnStallPolicy(fn)
	t.Cleanup(func() {
		if prev == nil {
			turnStallPolicy.Store(func(string, turnPhase, time.Duration, time.Duration) {})
			return
		}
		turnStallPolicy.Store(prev)
	})
}

// newArmedZeroClock builds an armed clock exactly like ArmTurnStall does for
// CallOptions{TurnStallTimeout: 0}, without starting the process-wide
// sampler goroutine.
func newArmedZeroClock(t *testing.T, sessionID string, call *agentcall.CallOptions) (*StallClock, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	sc := &StallClock{}
	if call != nil {
		sc.abortTimeout = call.TurnStallTimeout
	}
	now := stallNow()
	sc.lastProgress.Store(now.UnixNano())
	sc.lastByte.Store(now.UnixNano())
	sc.SetPhase("internal", "preamble", "")
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	var cancels, aborts atomic.Int32
	sc.ArmAttemptCancel(context.CancelFunc(func() { cancels.Add(1) }))
	sc.ArmStallAbort(func(time.Duration) { aborts.Add(1) })
	return sc, &cancels, &aborts
}

// restoreTurnStallPolicy puts back a value read by turnStallPolicy.Load.
func restoreTurnStallPolicy(prev any) {
	if prev == nil {
		return
	}
	turnStallPolicy.Store(prev)
}

// newStallPolicyClock arms a clock under sessionID with fake-clock
// control, an attempt-cancel holder and an abort recorder; restores the
// global registry and policy on cleanup.
func newStallPolicyClock(t *testing.T, abortTimeout time.Duration) (*StallClock, *context.Context, *[]time.Duration, func(time.Time)) {
	t.Helper()
	prev := turnStallPolicy.Load()
	installTurnStallPolicy()
	t.Cleanup(func() { restoreTurnStallPolicy(prev) })
	t0 := time.Now()
	set := InstallFakeStallNow(t)
	set(t0)
	sc := &StallClock{abortTimeout: abortTimeout}
	sc.MarkProgress()
	sessionID := "stall-policy-test"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sc.ArmAttemptCancel(cancel)
	var aborts []time.Duration
	sc.ArmStallAbort(func(elapsed time.Duration) { aborts = append(aborts, elapsed) })
	return sc, &ctx, &aborts, set
}

// fireStallSampler advances the fake clock past both the warn threshold
// and the clock's abortTimeout and runs one sampler tick.
func fireStallSampler(t *testing.T, set func(time.Time), from time.Time, advance time.Duration) {
	t.Helper()
	set(from.Add(advance))
	stallSamplerTickOnce()
}

func TestTurnStallReasoningDeltaDoesNotMoveProgress(t *testing.T) {
	t0 := time.Now()
	set := InstallFakeStallNow(t)
	set(t0)
	sc := &StallClock{}
	sc.MarkProgress()
	set(t0.Add(time.Hour))
	sc.AddReasoning(42)
	if got := time.Duration(stallNow().UnixNano() - sc.lastProgress.Load()); got != time.Hour {
		t.Fatalf("sinceProgress = %s, want 1h (reasoning must not move lastProgress)", got)
	}
	StallSamplerCheck("sess-1", sc)
	if sc.lastWarnNanos.Load() == 0 {
		t.Fatal("sampler did not warn despite reasoning-only traffic")
	}
}

func TestTurnStallSamplerWarnsOncePerWindow(t *testing.T) {
	t0 := time.Now()
	set := InstallFakeStallNow(t)
	set(t0)
	sc := &StallClock{abortTimeout: time.Minute}
	sc.MarkProgress()
	sessionID := "abcdef1234567890"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })
	t.Cleanup(stallDumpWrites.Wait)

	set(t0.Add(11 * time.Minute))
	stallSamplerTickOnce()
	firstWarn := sc.lastWarnNanos.Load()
	if firstWarn == 0 {
		t.Fatal("first crossing did not warn")
	}
	set(t0.Add(11*time.Minute + 5*time.Second))
	stallSamplerTickOnce()
	if sc.lastWarnNanos.Load() != firstWarn {
		t.Fatal("warned again inside the warn window")
	}
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
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("dump mode = %o, want 600", perm)
		}
	}
}

func TestTurnStallProviderRetriesAreSpaced(t *testing.T) {
	sc, _, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.SetPhase("provider", "test-model", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)
	require.Equal(t, int32(1), sc.stallRetries.Load())

	fireStallSampler(t, set, t0, 11*time.Minute+stallSamplerTick)
	require.Equal(t, int32(1), sc.stallRetries.Load(), "a retry needs a full warn window before the next one")
	require.Empty(t, *aborts)

	fireStallSampler(t, set, t0, 11*time.Minute+StreamIdleTimeoutDefault+stallSamplerTick)
	require.Equal(t, int32(2), sc.stallRetries.Load())
	require.Empty(t, *aborts)
}

func TestTurnStallPolicyFiresEveryCrossing(t *testing.T) {
	t0 := time.Now()
	set := InstallFakeStallNow(t)
	set(t0)
	sc := &StallClock{abortTimeout: time.Minute}
	sc.MarkProgress()
	sc.SetPhase("tool", "slow_tool", "call-x")
	sessionID := "sess-crossing"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	var fires atomic.Int64
	prev, _ := turnStallPolicy.Load().(func(string, turnPhase, time.Duration, time.Duration))
	t.Cleanup(func() {
		turnStallPolicy.Store(prev)
	})
	setTurnStallPolicy(func(sid string, phase turnPhase, sp, sb time.Duration) {
		if phase.kind != "tool" || phase.callID != "call-x" {
			t.Errorf("policy phase = %s/%s, want tool/call-x", phase.kind, phase.callID)
		}
		fires.Add(1)
	})

	set(t0.Add(11 * time.Minute))
	stallSamplerTickOnce()
	firstWarn := sc.lastWarnNanos.Load()
	set(t0.Add(11*time.Minute + 5*time.Second))
	stallSamplerTickOnce()
	if fires.Load() != 2 {
		t.Fatalf("policy fires = %d, want 2 inside the warn window", fires.Load())
	}
	if sc.lastWarnNanos.Load() != firstWarn {
		t.Fatal("warn+dump was not rate-limited inside the window")
	}
	set(t0.Add(45 * time.Minute))
	stallSamplerTickOnce()
	if fires.Load() != 3 {
		t.Fatalf("policy fires = %d, want 3", fires.Load())
	}
	if sc.lastWarnNanos.Load() == firstWarn {
		t.Fatal("warn did not fire after the window opened")
	}
}

func TestTurnStallPolicyProviderStallRetriesThenAborts(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.SetPhase("provider", "test-model", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)
	select {
	case <-(*ctx).Done():
	default:
		t.Fatal("policy did not cancel the stalled provider attempt on fire 1")
	}
	require.Equal(t, int32(1), sc.stallRetries.Load())
	require.Empty(t, *aborts)

	fireStallSampler(t, set, t0, 11*time.Minute+stallWarnWindow)
	require.Equal(t, int32(2), sc.stallRetries.Load())
	require.Empty(t, *aborts)

	fireStallSampler(t, set, t0, 11*time.Minute+2*stallWarnWindow)
	require.Len(t, *aborts, 1)
}

func TestTurnStallPolicyInternalPhaseAbortsImmediately(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.SetPhase("internal", "compaction", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)

	require.Len(t, *aborts, 1)
	require.Equal(t, int32(0), sc.stallRetries.Load(), "internal phases must never retry")
	select {
	case <-(*ctx).Done():
		t.Fatal("attempt cancel must not fire for internal phases")
	default:
	}
}

func TestTurnStallPolicyAbortDisabled(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 0)
	sc.SetPhase("internal", "persist", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, time.Hour)
	fireStallSampler(t, set, t0, time.Hour+stallWarnWindow)

	require.Empty(t, *aborts)
	require.Equal(t, int32(0), sc.stallRetries.Load())
	select {
	case <-(*ctx).Done():
		t.Fatal("abort must be disabled when abortTimeout is 0")
	default:
	}
	require.NotZero(t, sc.lastWarnNanos.Load())
}

func TestTurnStallPolicyProgressPreventsAbort(t *testing.T) {
	sc, _, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	t0 := time.Now()

	set(t0.Add(11 * time.Minute))
	sc.MarkProgress()
	stallSamplerTickOnce()

	require.Empty(t, *aborts)
	require.Equal(t, int32(0), sc.stallRetries.Load())
	require.Zero(t, sc.lastWarnNanos.Load())
}

// TestTurnStallSamplerRaceWithHooks: the sampler tick and the turn's hooks
// run on different goroutines in production; every hook and the tick itself
// must be race-clean on shared and separate clocks.
func TestTurnStallSamplerRaceWithHooks(t *testing.T) {
	set := InstallFakeStallNow(t)
	set(time.Now())
	log.SetLogDirForTest(t, t.TempDir())
	t.Cleanup(stallDumpWrites.Wait)
	installStallTestPolicy(t, func(string, turnPhase, time.Duration, time.Duration) {})

	shared := &StallClock{}
	shared.abortTimeout = time.Minute
	separate := &StallClock{}
	separate.abortTimeout = time.Minute
	armedStallClocks.Store("race-shared", shared)
	armedStallClocks.Store("race-separate", separate)
	t.Cleanup(func() {
		armedStallClocks.Delete("race-shared")
		armedStallClocks.Delete("race-separate")
	})

	set(time.Now().Add(11 * time.Minute))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				stallSamplerTickOnce()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			clock := shared
			if n%2 == 1 {
				clock = separate
			}
			for j := 0; j < 200; j++ {
				clock.MarkProgress()
				clock.MarkByte()
				clock.ToolEnter("bash", "call-race")
				clock.ToolExit()
				clock.SetPhase("internal", "persist", "")
				clock.AddReasoning(1)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			ArmTurnStall("race-churn", &agentcall.CallOptions{TurnStallTimeout: time.Minute}, nil)
			DisarmTurnStall("race-churn")
		}
	}()
	wg.Wait()
}

func TestRunStallRetriedAttributesOnlyPolicyCancels(t *testing.T) {
	sc := &StallClock{}
	attempts := 0
	err := RunStallRetried(sc, func() error {
		attempts++
		if attempts <= 2 {
			sc.stallRetries.Add(1)
			return context.Canceled
		}
		return nil
	}, func(retry int) {})
	require.NoError(t, err)
	require.Equal(t, 3, attempts)

	attempts = 0
	err = RunStallRetried(sc, func() error {
		attempts++
		return context.Canceled
	}, func(retry int) {})
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, 1, attempts)
}

func TestTurnStallZeroTimeoutWarnsButNeverAborts(t *testing.T) {
	t0 := time.Now()
	set := InstallFakeStallNow(t)
	set(t0)
	log.SetLogDirForTest(t, t.TempDir())
	t.Cleanup(stallDumpWrites.Wait)
	installStallTestPolicy(t, turnStallAbortPolicy)

	sc, cancels, aborts := newArmedZeroClock(t, "sess-zero", &agentcall.CallOptions{TurnStallTimeout: 0})

	set(t0.Add(11 * time.Minute))
	for _, phase := range []struct {
		kind, name, callID string
	}{
		{"provider", "", ""},
		{"tool", "bash", "call-1"},
		{"internal", "compaction", ""},
	} {
		sc.SetPhase(phase.kind, phase.name, phase.callID)
		stallSamplerTickOnce()
		require.Zero(t, cancels.Load(), "phase %s: attempt cancel fired despite TurnStallTimeout==0", phase.kind)
		require.Zero(t, sc.stallRetries.Load(), "phase %s: stall retry recorded despite TurnStallTimeout==0", phase.kind)
		require.Zero(t, aborts.Load(), "phase %s: turn abort (causeStall) fired despite TurnStallTimeout==0", phase.kind)
		require.NotZero(t, sc.lastWarnNanos.Load(), "phase %s: sampler did not warn+dump at the no-progress mark", phase.kind)
	}
}
