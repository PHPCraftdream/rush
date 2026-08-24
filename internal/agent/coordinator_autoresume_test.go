package agent

// Auto-resume / autonomy policy tests: consecutive-resume counter and cap,
// autonomyEnabled/autoResumeEligible truth tables, persistent-mode flag,
// background-job summaries, and runAutoResumeRecovered panic safety.

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boolPtr is a tiny helper for building *bool config values in tests.
func boolPtr(b bool) *bool { return &b }

func TestConsecutiveAutoResumeCounter(t *testing.T) {
	coord := &coordinator{consecutiveAutoResumes: make(map[string]int)}

	t.Run("starts at zero", func(t *testing.T) {
		assert.Equal(t, 0, coord.consecutiveResume("sess-1"))
	})

	t.Run("bump increments and consecutiveResume reflects it", func(t *testing.T) {
		coord.bumpConsecutiveResume("sess-1")
		coord.bumpConsecutiveResume("sess-1")
		assert.Equal(t, 2, coord.consecutiveResume("sess-1"))
	})

	t.Run("reset clears to zero", func(t *testing.T) {
		coord.bumpConsecutiveResume("sess-reset")
		require.Equal(t, 1, coord.consecutiveResume("sess-reset"))
		coord.resetConsecutiveResume("sess-reset")
		assert.Equal(t, 0, coord.consecutiveResume("sess-reset"))
	})

	t.Run("sessions are independent", func(t *testing.T) {
		coord.bumpConsecutiveResume("a")
		coord.bumpConsecutiveResume("a")
		coord.bumpConsecutiveResume("b")
		assert.Equal(t, 2, coord.consecutiveResume("a"))
		assert.Equal(t, 1, coord.consecutiveResume("b"))
	})

	t.Run("reset on unknown session is a no-op", func(t *testing.T) {
		coord.resetConsecutiveResume("never-seen")
		assert.Equal(t, 0, coord.consecutiveResume("never-seen"))
	})

	t.Run("concurrent bumps are serialized by the mutex", func(t *testing.T) {
		const sessionID = "sess-concurrent"
		const n = 100
		var wg sync.WaitGroup
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				coord.bumpConsecutiveResume(sessionID)
			}()
		}
		wg.Wait()
		assert.Equal(t, n, coord.consecutiveResume(sessionID))
	})
}

func TestMaxConsecutiveAutoResumesCap(t *testing.T) {
	// Guard against accidental edits to the runaway bound.
	assert.Equal(t, 5, maxConsecutiveAutoResumes)
}

func TestAutonomyEnabled(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg}

	t.Run("nil Options.AutoResumeOnJobDone defaults disabled", func(t *testing.T) {
		cfg.Config().Options = nil
		assert.False(t, coord.autonomyEnabled())
	})

	t.Run("explicit false stays disabled", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(false)}
		assert.False(t, coord.autonomyEnabled())
	})

	t.Run("explicit true enables", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		assert.True(t, coord.autonomyEnabled())
	})
}

func TestSetPersistentMode(t *testing.T) {
	coord := &coordinator{}
	assert.False(t, coord.persistentMode.Load(), "default must be false (crush run is non-persistent)")
	coord.SetPersistentMode(true)
	assert.True(t, coord.persistentMode.Load())
	coord.SetPersistentMode(false)
	assert.False(t, coord.persistentMode.Load())
}

// TestSetPersistentModeConcurrentAccess is the regression test for L-8:
// persistentMode used to be a plain bool written by SetPersistentMode and
// read by autoResumeEligible with no synchronization. That race was
// unreachable in practice (SetPersistentMode is only ever called once at
// process start today), but every sibling guard in this struct
// (allowPeakHours, activeModelRole, maxCost) is already lock/atomic-
// protected, so a plain bool here was a silent trap for the next caller who
// adds a second call path. This test writes and reads persistentMode from
// many goroutines concurrently — under `go test -race` this fails loudly on
// the old plain-bool field and passes cleanly on the atomic.Bool.
func TestSetPersistentModeConcurrentAccess(t *testing.T) {
	coord := &coordinator{consecutiveAutoResumes: make(map[string]int)}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			coord.SetPersistentMode(i%2 == 0)
		}(i)
		go func() {
			defer wg.Done()
			_ = coord.persistentMode.Load()
		}()
	}
	wg.Wait()
}

func TestBackgroundJobSummary(t *testing.T) {
	t.Parallel()

	t.Run("with stdout", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00A", "echo hi && make build", "hello world", "", 0, 42*time.Second)
		assert.Contains(t, got, "00A")
		assert.Contains(t, got, "`echo hi && make build`")
		assert.Contains(t, got, "exit 0")
		assert.Contains(t, got, "42s")
		assert.Contains(t, got, "hello world")
	})

	t.Run("exit code and stderr surfaced", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00B", "make test", "", "boom: tests failed", 2, 90*time.Second)
		assert.Contains(t, got, "exit 2")
		assert.Contains(t, got, "1m30s")
		assert.Contains(t, got, "boom: tests failed")
	})

	t.Run("no output falls back to placeholder", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00C", "true", "  \n ", "", 0, 3*time.Second)
		assert.Contains(t, got, "(no output)")
	})

	t.Run("both stdout and stderr are joined", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00D", "go test ./...", "ok pkg 0.1s", "warn: deprecated", 0, 5*time.Second)
		assert.Contains(t, got, "ok pkg 0.1s")
		assert.Contains(t, got, "warn: deprecated")
	})
}

func TestAutoResumeEligible(t *testing.T) {
	// Truth table for the Phase 4 autonomy policy surface. The eligibility
	// decision is the whole gate; the branch in notifyBackgroundJobDone just
	// routes eligible->Run vs not->InjectMessage.
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, consecutiveAutoResumes: make(map[string]int)}
	const sid = "sess-eligible"

	t.Run("autonomy OFF (nil Options) is never eligible regardless of persistentMode", func(t *testing.T) {
		cfg.Config().Options = nil
		coord.persistentMode.Store(true)
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy OFF (explicit false) is never eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(false)}
		coord.persistentMode.Store(true)
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy ON + persistentMode false (crush run) is not eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode.Store(false)
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy ON + persistentMode true + counter below cap is eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode.Store(true)
		coord.resetConsecutiveResume(sid)
		assert.True(t, coord.autoResumeEligible(sid))
	})

	t.Run("at the cap (== maxConsecutiveAutoResumes) flips to not eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode.Store(true)
		coord.resetConsecutiveResume(sid)
		// Bump to exactly the cap; one below the cap is still eligible.
		for i := 0; i < maxConsecutiveAutoResumes-1; i++ {
			coord.bumpConsecutiveResume(sid)
		}
		assert.True(t, coord.autoResumeEligible(sid), "one below the cap must still be eligible")
		// The boundary bump that reaches the cap flips eligibility off.
		coord.bumpConsecutiveResume(sid)
		assert.False(t, coord.autoResumeEligible(sid), "at the cap autonomy must stop")
	})
}

func TestResetAutoResumeCounter(t *testing.T) {
	// The exported wrapper is what the server package calls on the human send
	// path; it must clear the consecutive bound so a human message re-arms
	// autonomy.
	coord := &coordinator{consecutiveAutoResumes: make(map[string]int)}
	const sid = "sess-reset-exported"

	coord.bumpConsecutiveResume(sid)
	coord.bumpConsecutiveResume(sid)
	require.Equal(t, 2, coord.consecutiveResume(sid))

	coord.ResetAutoResumeCounter(sid)
	assert.Equal(t, 0, coord.consecutiveResume(sid))
}

// TestRunAutoResumeRecovered_Panic proves that a panic raised anywhere
// inside runFn (standing in for the Phase 4 auto-resume closure over c.Run
// in notifyBackgroundJobDone, which re-enters the full synchronous
// tool-dispatch chain) is recovered rather than crashing the process. This
// goroutine is spawned independently of BackgroundShell.OnDone's own
// recover(), so it needs its own — without it, a panic here (e.g. from a
// tool call made during the auto-resumed turn) would kill the whole crush
// process with no log output, at an arbitrary time after the triggering
// background job finished.
func TestRunAutoResumeRecovered_Panic(t *testing.T) {
	done := make(chan struct{})
	panicking := func(ctx context.Context) (*fantasy.AgentResult, error) {
		defer close(done)
		panic("boom: simulated panic inside Phase 4 auto-resume Run")
	}

	// Run on its own goroutine, same as production, so an unrecovered panic
	// would take down the test binary rather than just this function.
	go runAutoResumeRecovered(t.Context(), "sess-1", "shell-1", panicking)

	select {
	case <-done:
		// Expected: runFn ran (and panicked) without crashing the test
		// process — reaching this line at all is the core assertion.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for panicking runFn to run — goroutine likely died silently")
	}
}

// TestRunAutoResumeRecovered_NormalErrorUnaffected verifies the existing,
// expected error-handling path (a normal Go error returned by runFn, e.g.
// because the session was already closed) is completely untouched by the
// new recover() — it must not be misclassified as a panic or swallowed
// differently than before.
func TestRunAutoResumeRecovered_NormalErrorUnaffected(t *testing.T) {
	called := make(chan struct{})
	erroring := func(ctx context.Context) (*fantasy.AgentResult, error) {
		defer close(called)
		return nil, assert.AnError
	}

	// Must return promptly (no panic, no goroutine involved needed here
	// since erroring doesn't panic) and must not itself panic.
	require.NotPanics(t, func() {
		runAutoResumeRecovered(t.Context(), "sess-2", "shell-2", erroring)
	})

	select {
	case <-called:
	default:
		t.Fatal("runFn was not invoked")
	}
}
