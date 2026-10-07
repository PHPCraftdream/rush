package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Each test names the single production behavior it must catch (revert-check
// documentation):
//
//   - TestTurnStallZeroTimeoutWarnsButNeverAborts guards the
//     `abortTimeout <= 0` off switch in turnStallAbortPolicy: with
//     TurnStallTimeout == 0 the sampler still warns + dumps at the 10-min
//     no-progress mark, but no phase (provider / tool / internal) is ever
//     cancelled, retried, detached or aborted (no cancel, no causeStall).
//   - TestTurnStallSamplerRaceWithHooks guards the lock-free design of
//     stallSamplerTickOnce and the stallClock hooks: concurrent
//     markProgress / markByte / toolEnter / toolExit / setPhase / arm /
//     disarm on shared and separate clocks must be race-clean.
//   - TestTurnStallReasoningDeltaHookLeavesProgressPinned guards
//     agent_turn_stream.go's onReasoningDelta: it bumps lastByte
//     (via bumpActivity) and the reasoningChars counter, but must never
//     move lastProgress.

// installStallTestPolicy swaps the turn-stall policy for fn and restores the
// previous one on cleanup.
func installStallTestPolicy(t *testing.T, fn func(string, turnPhase, time.Duration, time.Duration)) {
	t.Helper()
	prev := turnStallPolicy.Load()
	SetTurnStallPolicy(fn)
	t.Cleanup(func() {
		if prev == nil {
			turnStallPolicy.Store(func(string, turnPhase, time.Duration, time.Duration) {})
			return
		}
		turnStallPolicy.Store(prev)
	})
}

// newArmedZeroClock builds an armed clock exactly like armTurnStall does for
// CallOptions{TurnStallTimeout: 0}, without starting the process-wide
// sampler goroutine: CallOptions feeds abortTimeout (0 = abort disabled),
// the registry entry is cleaned up, and the abort seam recorders are armed.
func newArmedZeroClock(t *testing.T, sessionID string, call *CallOptions) (*stallClock, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	sc := &stallClock{}
	if call != nil {
		sc.abortTimeout = call.TurnStallTimeout
	}
	now := stallNow()
	sc.lastProgress.Store(now.UnixNano())
	sc.lastByte.Store(now.UnixNano())
	sc.setPhase("internal", "preamble", "")
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	var cancels, aborts atomic.Int32
	sc.armAttemptCancel(context.CancelFunc(func() { cancels.Add(1) }))
	sc.armStallAbort(func(time.Duration) { aborts.Add(1) })
	return sc, &cancels, &aborts
}

// TestTurnStallZeroTimeoutWarnsButNeverAborts: with TurnStallTimeout == 0
// the 10-min no-progress crossing must still warn + dump, and the abort
// policy must do NOTHING in any phase — no attempt cancel, no stall retry,
// no causeStall abort, no tool detach.
func TestTurnStallZeroTimeoutWarnsButNeverAborts(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	log.SetLogDirForTest(t, t.TempDir())
	t.Cleanup(stallDumpWrites.Wait)
	installStallTestPolicy(t, turnStallAbortPolicy)

	sc, cancels, aborts := newArmedZeroClock(t, "sess-zero", &CallOptions{TurnStallTimeout: 0})

	// Cross the 10-min warn threshold with no progress at all.
	set(t0.Add(11 * time.Minute))
	for _, phase := range []struct {
		kind, name, callID string
	}{
		{"provider", "", ""},
		{"tool", "bash", "call-1"},
		{"internal", "compaction", ""},
	} {
		sc.setPhase(phase.kind, phase.name, phase.callID)
		stallSamplerTickOnce()
		require.Zero(t, cancels.Load(), "phase %s: attempt cancel fired despite TurnStallTimeout==0", phase.kind)
		require.Zero(t, sc.stallRetries.Load(), "phase %s: stall retry recorded despite TurnStallTimeout==0", phase.kind)
		require.Zero(t, aborts.Load(), "phase %s: turn abort (causeStall) fired despite TurnStallTimeout==0", phase.kind)
		require.NotZero(t, sc.lastWarnNanos.Load(), "phase %s: sampler did not warn+dump at the no-progress mark", phase.kind)
	}
}

// TestTurnStallSamplerRaceWithHooks: the sampler tick and the turn's hooks
// run on different goroutines in production (sampler goroutine vs stream
// callbacks); every hook and the tick itself must be race-clean on shared
// and separate clocks.
func TestTurnStallSamplerRaceWithHooks(t *testing.T) {
	set := installFakeStallNow(t)
	set(time.Now())
	log.SetLogDirForTest(t, t.TempDir())
	// Registered after TempDir, so it runs first: async dump writes must land
	// before the dir is removed.
	t.Cleanup(stallDumpWrites.Wait)
	installStallTestPolicy(t, func(string, turnPhase, time.Duration, time.Duration) {})

	shared := &stallClock{}
	shared.abortTimeout = time.Minute
	separate := &stallClock{}
	separate.abortTimeout = time.Minute
	armedStallClocks.Store("race-shared", shared)
	armedStallClocks.Store("race-separate", separate)
	t.Cleanup(func() {
		armedStallClocks.Delete("race-shared")
		armedStallClocks.Delete("race-separate")
	})

	// Cross the warn threshold so the tick exercises the full warn+dump path.
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
				clock.markProgress()
				clock.markByte()
				clock.toolEnter("bash", "call-race")
				clock.toolExit()
				clock.setPhase("internal", "persist", "")
				clock.reasoningChars.Add(1)
			}
		}(i)
	}
	// Arm/disarm churn on the shared session id, like overlapping turns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			armTurnStall("race-churn", &CallOptions{TurnStallTimeout: time.Minute})
			disarmTurnStall("race-churn")
		}
	}()
	wg.Wait()
}

// TestTurnStallReasoningDeltaHookLeavesProgressPinned: onReasoningDelta must
// move lastByte (the liveness mirror) and reasoningChars, but never
// lastProgress — extended thinking alone must not reset the stall clock.
func TestTurnStallReasoningDeltaHookLeavesProgressPinned(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)

	sc, _, _ := newArmedZeroClock(t, "sess-reason", &CallOptions{TurnStallTimeout: time.Minute})
	sc.markProgress()
	progressBefore := sc.lastProgress.Load()
	byteBefore := sc.lastByte.Load()

	// Begin the step's assistant row exactly like prepareStep does, against
	// a real message service (same helper as the retry fixture).
	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
	svc := message.NewService(db.New(conn))
	msg, err := svc.Create(context.Background(), "sess-reason", message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)
	ts := &turnStream{
		a:            &sessionAgent{messages: svc},
		ctx:          t.Context(),
		genCtx:       t.Context(),
		call:         SessionAgentCall{SessionID: "sess-reason"},
		stall:        sc,
		bumpActivity: sc.markByte,
		notifyUI:     func() error { return nil },
	}
	ts.beginAssistantStep(&msg)

	set(t0.Add(5 * time.Minute))
	require.NoError(t, ts.onReasoningDelta("r1", "thinking hard"))
	require.NoError(t, ts.onReasoningDelta("r1", " still thinking"))

	require.Equal(t, progressBefore, sc.lastProgress.Load(), "reasoning delta moved lastProgress")
	require.Greater(t, sc.lastByte.Load(), byteBefore, "reasoning delta did not move lastByte")
	require.Equal(t, int64(len("thinking hard")+len(" still thinking")), sc.reasoningChars.Load())
}
