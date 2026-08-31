package agent

// Regression tests for R1-1 (P0, per-call execution context) and R1-4
// (P1, atomic fail-fast session reservation).
//
// R1-1: ExecuteRun used to pin per-invocation settings onto SHARED
// coordinator state (SetActiveModelRole/SetRunLimits/SetAllowPeakHours/
// SetTimeoutOptions) and the shared runAllowlistGate; two overlapping
// runs raced for every one of those fields. The per-call CallOptions
// carried in the run's context must win over — and leave untouched — the
// shared state, including under concurrent runs.
//
// R1-4: mailbox.submit is the single atomic check-and-set that grants
// session ownership. A call with FailIfSessionBusy that loses the race
// must neither queue nor mutate the mailbox; exactly one of two
// simultaneous starters on an idle session can win.

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callOptionsPeakCoordinator builds a coordinator whose provider sits
// inside a peak_hours window covering "now" (so checkPeakHours refuses
// unless the run bypasses it), with a call-capturing mock agent wired in.
type callCaptureAgent struct {
	mu    sync.Mutex
	calls []SessionAgentCall
}

func (c *callCaptureAgent) record(call SessionAgentCall) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
}

func (c *callCaptureAgent) reset() {
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
}

func (c *callCaptureAgent) snapshot() []SessionAgentCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SessionAgentCall, len(c.calls))
	copy(out, c.calls)
	return out
}

func newCallOptionsTestCoordinator(t *testing.T) (*coordinator, *callCaptureAgent) {
	t.Helper()

	const providerID = "test-peak"
	providerCfg := config.ProviderConfig{
		ID:        providerID,
		Type:      "openai",
		PeakHours: peakWindowAroundNow(),
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = selected
	cfg.Config().Models[config.SelectedModelTypeFast] = selected

	capture := &callCaptureAgent{}
	magent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		capture.record(call)
		return agentResultWithText("ok"), nil
	})

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	coord.currentAgent = magent
	return coord, capture
}

// TestRunInternal_PerCallOptions_honorsOptionsAndLeavesSharedState pins
// the precedence rules: a context-carrying run reads its policy from
// CallOptions and never consumes the shared Set*-state; a context-less
// run keeps the legacy read-and-reset behavior.
func TestRunInternal_PerCallOptions_honorsOptionsAndLeavesSharedState(t *testing.T) {
	coord, capture := newCallOptionsTestCoordinator(t)

	sessA, err := coord.sessions.Create(t.Context(), "per-call-a")
	require.NoError(t, err)
	sessB, err := coord.sessions.Create(t.Context(), "per-call-b")
	require.NoError(t, err)
	pinnedA, err := coord.resolveSessionModels(t.Context(), sessA.ID)
	require.NoError(t, err)
	pinnedB, err := coord.resolveSessionModels(t.Context(), sessB.ID)
	require.NoError(t, err)

	t.Run("per-call bypass reaches the agent with its own caps", func(t *testing.T) {
		capture.reset()
		ctx := WithCallOptions(t.Context(), &CallOptions{
			AllowPeakHours:    true,
			MaxCost:           1.5,
			MaxTokens:         111,
			FailIfSessionBusy: true,
		})
		res, err := coord.runInternal(ctx, sessA.ID, "A", pinnedA)
		require.NoError(t, err, "per-call AllowPeakHours must bypass the in-window refusal")
		require.NotNil(t, res)

		snap := capture.snapshot()
		require.Len(t, snap, 1)
		call := snap[0]
		assert.Equal(t, 1.5, call.MaxCost, "caps must come from CallOptions, not shared state")
		assert.Equal(t, int64(111), call.MaxTokens)
		assert.True(t, call.FailIfSessionBusy, "fail-fast flag must travel onto the call")
		require.NotNil(t, call.CallOptions)
		assert.True(t, call.CallOptions.AllowPeakHours)
	})

	t.Run("per-call no-bypass refuses while shared flag stays untouched", func(t *testing.T) {
		capture.reset()
		// Arm the shared state: a per-call run must neither read nor
		// consume it (the pre-fix code reset allowPeakHours on every read).
		coord.SetAllowPeakHours(true)
		coord.SetRunLimits(9, 9)

		ctx := WithCallOptions(t.Context(), &CallOptions{AllowPeakHours: false})
		_, err := coord.runInternal(ctx, sessB.ID, "B", pinnedB)
		require.Error(t, err, "CallOptions{AllowPeakHours:false} must refuse inside the window")
		assert.ErrorIs(t, err, errProviderPeakHours)
		assert.Empty(t, capture.snapshot(), "agent must not be reached when the per-call gate refuses")

		coord.runLimitsMu.Lock()
		sharedPeak, sharedCost, sharedTokens := coord.allowPeakHours, coord.maxCost, coord.maxTokens
		coord.runLimitsMu.Unlock()
		assert.True(t, sharedPeak, "shared one-shot flag must NOT be consumed by a per-call run")
		assert.Equal(t, 9.0, sharedCost, "shared caps must NOT be read into a per-call run")
		assert.Equal(t, int64(9), sharedTokens)
	})

	t.Run("context-less run keeps the legacy shared read-and-reset path", func(t *testing.T) {
		capture.reset()
		coord.SetRunLimits(4.5, 44)
		coord.SetAllowPeakHours(true)

		res, err := coord.runInternal(t.Context(), sessB.ID, "legacy", pinnedB)
		require.NoError(t, err, "legacy SetAllowPeakHours bypass must keep working")
		require.NotNil(t, res)

		snap := capture.snapshot()
		require.Len(t, snap, 1)
		assert.Equal(t, 4.5, snap[0].MaxCost)
		assert.Equal(t, int64(44), snap[0].MaxTokens)
		assert.Nil(t, snap[0].CallOptions, "legacy runs carry no per-call options")

		// One-shot semantics of the legacy path are preserved: consumed.
		coord.runLimitsMu.Lock()
		sharedPeak, sharedCost := coord.allowPeakHours, coord.maxCost
		coord.runLimitsMu.Unlock()
		assert.False(t, sharedPeak, "legacy bypass is one-shot")
		assert.Zero(t, sharedCost, "legacy caps reset after use")
	})
}

// TestRunInternal_PerCallOptions_ConcurrentIsolated hammers the actual
// race shape: two simultaneous runInternal calls on ONE coordinator with
// opposite per-call policies. Each call must be served exclusively by its
// own options — under -race this also proves the shared Set*-state is no
// longer part of the per-call read path.
func TestRunInternal_PerCallOptions_ConcurrentIsolated(t *testing.T) {
	coord, capture := newCallOptionsTestCoordinator(t)

	sessA, err := coord.sessions.Create(t.Context(), "concurrent-a")
	require.NoError(t, err)
	sessB, err := coord.sessions.Create(t.Context(), "concurrent-b")
	require.NoError(t, err)
	pinnedA, err := coord.resolveSessionModels(t.Context(), sessA.ID)
	require.NoError(t, err)
	pinnedB, err := coord.resolveSessionModels(t.Context(), sessB.ID)
	require.NoError(t, err)

	const iterations = 25
	for i := 0; i < iterations; i++ {
		ctxA := WithCallOptions(t.Context(), &CallOptions{
			AllowPeakHours: true,
			MaxCost:        1.5,
			MaxTokens:      111,
		})
		ctxB := WithCallOptions(t.Context(), &CallOptions{
			AllowPeakHours: false,
			MaxCost:        2.5,
			MaxTokens:      222,
		})

		barrier := make(chan struct{})
		type outcome struct {
			res *fantasy.AgentResult
			err error
		}
		outA := make(chan outcome, 1)
		outB := make(chan outcome, 1)
		go func() {
			<-barrier
			res, err := coord.runInternal(ctxA, sessA.ID, "A", pinnedA)
			outA <- outcome{res, err}
		}()
		go func() {
			<-barrier
			res, err := coord.runInternal(ctxB, sessB.ID, "B", pinnedB)
			outB <- outcome{res, err}
		}()
		close(barrier)

		resA := <-outA
		resB := <-outB
		require.NoError(t, resA.err, "iteration %d: bypassing call must run", i)
		require.NotNil(t, resA.res)
		require.ErrorIs(t, resB.err, errProviderPeakHours, "iteration %d: non-bypassing call must refuse", i)

		for _, call := range capture.snapshot() {
			switch call.Prompt {
			case "A":
				assert.Equal(t, 1.5, call.MaxCost, "iteration %d: A's caps", i)
				assert.Equal(t, int64(111), call.MaxTokens, "iteration %d: A's tokens", i)
			case "B":
				t.Fatalf("iteration %d: refused call must never reach the agent", i)
			default:
				t.Fatalf("unexpected call prompt %q", call.Prompt)
			}
		}
		capture.mu.Lock()
		capture.calls = nil
		capture.mu.Unlock()
	}
}

// TestWorkerSubAgentActiveForCall_ContextRoleWins pins the per-call model
// role: a CallOptions-carrying context decides, the shared
// activeModelRole field is only the fallback for context-less callers.
func TestWorkerSubAgentActiveForCall_ContextRoleWins(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, true) // worker configured
	cfg := coord.cfg.Config()

	ctxSmart := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})
	ctxFast := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeFast})

	// Shared field says "fast"; the per-call context says "smart" and must win.
	coord.SetActiveModelRole(config.SelectedModelTypeFast)
	assert.True(t, coord.workerSubAgentActiveForCall(ctxSmart, cfg),
		"per-call smart role must prefer the Worker slot even while the shared field says fast")
	assert.False(t, coord.workerSubAgentActiveForCall(ctxFast, cfg))

	// Flipping the shared field must not leak into a context-carrying call.
	coord.SetActiveModelRole(config.SelectedModelTypeReviewer)
	assert.True(t, coord.workerSubAgentActiveForCall(ctxSmart, cfg),
		"shared activeModelRole writes must not affect a per-call run")

	// Context-less callers keep the shared-field fallback semantics.
	coord.SetActiveModelRole(config.SelectedModelTypeSmart)
	assert.True(t, coord.workerSubAgentActiveForCall(context.Background(), cfg))
	coord.SetActiveModelRole(config.SelectedModelTypeWorker)
	assert.False(t, coord.workerSubAgentActiveForCall(context.Background(), cfg))
}

// TestMailboxSubmit_FailFastNeverQueues pins the R1-4 atomicity contract
// at the state-machine level: a FailIfSessionBusy call either becomes the
// owner in submit's single critical section, or returns without touching
// the mailbox — it must never land in the submitted queue.
func TestMailboxSubmit_FailFastNeverQueues(t *testing.T) {
	t.Run("idle mailbox: fail-fast call becomes owner", func(t *testing.T) {
		mb := &mailbox{}
		became, epoch := mb.submit(SessionAgentCall{FailIfSessionBusy: true}, func() {})
		assert.True(t, became)
		assert.NotZero(t, epoch)
		assert.True(t, mb.state == mbOwned, "state must be mbOwned after winning")
	})

	t.Run("owned mailbox: fail-fast call is rejected without queueing", func(t *testing.T) {
		mb := &mailbox{}
		_, epoch := mb.submit(SessionAgentCall{}, func() {}) // first caller owns
		became, epoch2 := mb.submit(SessionAgentCall{FailIfSessionBusy: true, Prompt: "loser"}, func() {})
		assert.False(t, became)
		assert.Zero(t, epoch2, "a losing fail-fast call never holds ownership")
		assert.Empty(t, mb.submitted, "a losing fail-fast call must NOT be queued")
		assert.Equal(t, epoch, mb.epoch, "epoch must not move for a rejected call")
	})

	t.Run("owned mailbox: legacy call still queues silently", func(t *testing.T) {
		mb := &mailbox{}
		mb.submit(SessionAgentCall{}, func() {})
		became, _ := mb.submit(SessionAgentCall{Prompt: "queued"}, func() {})
		assert.False(t, became)
		require.Len(t, mb.submitted, 1, "non-fail-fast callers keep the historical queue behavior")
		assert.Equal(t, "queued", mb.submitted[0].Prompt)
	})

	t.Run("releasing mailbox: fail-fast call is rejected as busy", func(t *testing.T) {
		mb := &mailbox{}
		_, epoch := mb.submit(SessionAgentCall{}, func() {})
		require.True(t, mb.beginRelease(epoch), "enter mbReleasing")
		became, _ := mb.submit(SessionAgentCall{FailIfSessionBusy: true}, func() {})
		assert.False(t, became, "mbReleasing reads as busy")
		assert.Empty(t, mb.submitted)
	})
}

// TestRunWithCallOptions_ContextRoundTrip is a trivial guard on the
// context plumbing itself: the exact pointer comes back, absent contexts
// yield nil.
func TestRunWithCallOptions_ContextRoundTrip(t *testing.T) {
	assert.Nil(t, callOptionsFrom(context.Background()))

	opts := &CallOptions{MaxCost: 3.25, TimeoutHardCap: 5 * time.Minute}
	ctx := WithCallOptions(context.Background(), opts)
	got := callOptionsFrom(ctx)
	require.Same(t, opts, got, "the exact CallOptions pointer must round-trip")
	assert.Equal(t, 3.25, got.MaxCost)
}
