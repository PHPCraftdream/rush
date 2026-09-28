package agent

// Auto-resume / autonomy policy tests: consecutive-resume counter and cap,
// autonomyEnabled/autoResumeEligible truth tables, persistent-mode flag,
// background-job summaries, and runAutoResumeRecovered panic safety.

import (
	"context"
	"sync"
	"sync/atomic"
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
	assert.False(t, coord.persistentMode.Load(), "default must be false (rush run is non-persistent)")
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

	t.Run("autonomy ON + persistentMode false (rush run) is not eligible", func(t *testing.T) {
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

// TestWakeSession_RunPanicIsRecovered proves that a panic raised anywhere
// inside the SessionAgent.Run call wakeSession makes (standing in for the
// Phase 4 auto-resume turn, which re-enters the full synchronous
// tool-dispatch chain) is recovered rather than crashing the process --
// phase 2 moved this protection from the now-removed runAutoResumeRecovered
// into wakeSession's own defer recover() (docs/plans/2026-09-27-async-
// phase2-spec.md §1.7). Without it, a panic here (e.g. from a tool call made
// during the auto-resumed turn) would kill the whole rush process with no
// log output, at an arbitrary time after the triggering job finished.
func TestWakeSession_RunPanicIsRecovered(t *testing.T) {
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			panic("boom: simulated panic inside a woken Run")
		},
	}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	// Registering a driver (rather than routing through c.currentAgent)
	// keeps drainCallFor on its driver.callFor branch, which needs no
	// cfg/sessions wiring -- this test is about wakeSession's own recover(),
	// not about model resolution.
	coord.subAgentDrivers.register("sess-1", subAgentDriver{agent: agent})
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord

	var err error
	require.NotPanics(t, func() {
		err = coord.wakeSession(t.Context(), jobIdentity{owner: "sess-1", toolCallID: "call-1"}, true)
	})
	require.Error(t, err, "a recovered panic must still be reported as a real error, not silently swallowed")
}

// TestWakeSession_RunErrorIsVisibleNotDebug pins ASYNC-09: a wake whose Run
// attempt fails after the underlying fact was already committed must
// produce a visible marker, not just a Debug log line nobody sees. Step 3
// changed WHERE that marker lands: wakeSession no longer persists anything
// itself, so the marker is a durable session_notices row (kind
// NoticeKindWakeFailed, wake=0), not a second InjectMessage call.
//
// Revert-check performed: removed the persistWakeFailedMarker call from
// wakeSession's error branch -- this test FAILED (ListSessionNotices
// returned zero rows) -- restored the call, re-ran, passed (one row, kind
// "wake_failed", text containing "call-2").
func TestWakeSession_RunErrorIsVisibleNotDebug(t *testing.T) {
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, assert.AnError
		},
	}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("sess-2", subAgentDriver{agent: agent})
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord

	err := coord.wakeSession(t.Context(), jobIdentity{owner: "sess-2", toolCallID: "call-2"}, true)
	require.Error(t, err)

	notices, listErr := coord.asyncJobs.store.ListSessionNotices(t.Context(), "sess-2")
	require.NoError(t, listErr)
	require.Len(t, notices, 1, "expected exactly one durable wake-failed marker")
	require.Equal(t, "wake_failed", notices[0].Kind)
	require.Contains(t, notices[0].Text, "call-2")
}

// TestWakeSession_AlwaysAttemptsRunEvenWhenSessionLooksBusy pins the fix for
// a regression found while running internal/app's black-box suite
// (TestRunNonInteractiveChildReceivesAsyncBashResultFinishedMidTurn): an
// earlier draft of wakeSession skipped step 2 (Run) whenever
// agent.IsSessionBusy(owner) reported true, relying solely on step 1's
// InjectMessage to merge the notice into the CURRENT generation via
// injectIfBusy. That merge is a best-effort splice that only lands if the
// current generation calls PrepareStep again after the splice -- a
// generation whose last PrepareStep already ran before the notice was
// persisted never drains it, so the child never got a fresh turn and its
// parent was told "finished" over the child's pre-result text. wakeSession
// must ALWAYS attempt Run when wake=true; Run's own tryReserveSession
// queues it (guaranteed to run as the mailbox's own next turn) instead of
// silently doing nothing.
// Revert-check performed: reinstated `if agent.IsSessionBusy(job.owner) {
// return nil }` before the Run call -- this test FAILED (calls.Load() was 0
// instead of 1). Removed it again; re-ran, passed.
func TestWakeSession_AlwaysAttemptsRunEvenWhenSessionLooksBusy(t *testing.T) {
	var calls atomic.Int32
	agent := &busyStubAgent{}
	agent.setBusy(true)
	agent.runFunc = func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls.Add(1)
		if adm := turnAdmissionFrom(ctx); adm != nil {
			adm.markQueued()
		}
		return nil, nil
	}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: agent})
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord

	err := coord.wakeSession(t.Context(), jobIdentity{owner: "child-1", toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load(), "wakeSession must still call Run even when the session looks busy")
}
