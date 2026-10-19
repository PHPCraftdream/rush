package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUpdateModelsDrainsItsLocalGateBeforePublishing(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	recorder := &transactionalModelPublisher{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = recorder

	held := make(chan struct{})
	entered := make(chan struct{})
	var release sync.Once
	var local *readyGate
	// One-shot seam: only the FIRST UpdateModels installs held work; later
	// calls (none in this test) must not re-arm it.
	var seamArmed atomic.Bool
	updateModelsReadyGateSeam = func(_ context.Context, gate *readyGate) {
		if !seamArmed.CompareAndSwap(false, true) {
			return
		}
		local = gate
		gate.Go(func() error { <-held; return nil })
		close(entered)
	}
	// LIFO: registered after testEnv's own cleanups, so this runs BEFORE the
	// DB release -- the UpdateModels goroutine is always joined and can never
	// outlive the fixture.
	result := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		updateModelsReadyGateSeam = nil
		release.Do(func() { close(held) })
		if !joined {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
			}
		}
	})

	go func() { result <- coord.UpdateModels(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateModels did not register its local build work")
	}
	// entered proves the task was parked behind the seam; with the waiter
	// still blocked there is deterministically no publishing yet.
	require.NotNil(t, local)
	var modelCalls, toolCalls int
	recorder.mu.Lock()
	modelCalls, toolCalls = recorder.modelCalls, recorder.toolCalls
	recorder.mu.Unlock()
	require.Zero(t, modelCalls)
	require.Zero(t, toolCalls)

	release.Do(func() { close(held) })
	select {
	case err := <-result:
		require.NoError(t, err)
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateModels did not finish after its local task was released")
	}
	recorder.mu.Lock()
	modelCalls, toolCalls = recorder.modelCalls, recorder.toolCalls
	recorder.mu.Unlock()
	require.Equal(t, 1, modelCalls)
	require.Equal(t, 1, toolCalls)
}

func TestUpdateModelsCancellationLeavesInstalledAgentAndAllowsRecovery(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	recorder := &transactionalModelPublisher{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = recorder
	old := config.SelectedModel{Provider: "smart-provider", Model: "smart-model"}
	recorder.models = []Model{{ModelCfg: old}, {ModelCfg: old}}
	ctx, cancel := context.WithCancel(t.Context())
	held := make(chan struct{})
	entered := make(chan struct{})
	var release sync.Once
	// One-shot seam: the recovery UpdateModels at the end re-enters it and
	// must NOT install (or re-close) anything.
	var seamArmed atomic.Bool
	updateModelsReadyGateSeam = func(_ context.Context, gate *readyGate) {
		if !seamArmed.CompareAndSwap(false, true) {
			return
		}
		gate.Go(func() error {
			// readyGate.Go tasks receive no context; observe the test's ctx
			// directly so cancel() ends this task deterministically.
			select {
			case <-held:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(entered)
	}
	// LIFO: runs BEFORE testEnv's DB release, so the first UpdateModels
	// goroutine is always joined and can never outlive the fixture.
	result := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		updateModelsReadyGateSeam = nil
		cancel()
		release.Do(func() { close(held) })
		if !joined {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
			}
		}
	})

	go func() { result <- coord.UpdateModels(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateModels did not register local build work")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("canceled UpdateModels did not return after its local task observed the cancellation")
	}
	var modelCalls, toolCalls int
	var kept0, kept1 config.SelectedModel
	recorder.mu.Lock()
	modelCalls, toolCalls = recorder.modelCalls, recorder.toolCalls
	kept0, kept1 = recorder.models[0].ModelCfg, recorder.models[1].ModelCfg
	recorder.mu.Unlock()
	require.Zero(t, modelCalls)
	require.Zero(t, toolCalls)
	require.Equal(t, old, kept0)
	require.Equal(t, old, kept1)
	require.NoError(t, coord.readyWg.Wait(), "failed local refresh must not poison initialization admission")
	coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "smart-provider", Model: "model-latest"}
	require.NoError(t, coord.UpdateModels(t.Context()))
	var latest config.SelectedModel
	recorder.mu.Lock()
	modelCalls, toolCalls = recorder.modelCalls, recorder.toolCalls
	latest = recorder.models[0].ModelCfg
	recorder.mu.Unlock()
	require.Equal(t, "model-latest", latest.Model)
	require.Equal(t, 1, modelCalls)
	require.Equal(t, 1, toolCalls)
}

type transactionalModelPublisher struct {
	*mockSessionAgent
	mu         sync.Mutex
	models     []Model
	modelCalls int
	toolCalls  int
}

func (r *transactionalModelPublisher) SetModels(smart, fast Model) {
	r.mu.Lock()
	r.models = []Model{smart, fast}
	r.modelCalls++
	r.mu.Unlock()
}

func (r *transactionalModelPublisher) SetSystemPromptPrefix(string) {}

func (r *transactionalModelPublisher) SetTools([]fantasy.AgentTool) {
	r.mu.Lock()
	r.toolCalls++
	r.mu.Unlock()
}
