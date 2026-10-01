// A Drain call through runInternal is ONE provider attempt (attempts design
// 1.6): no transient-retry/continuation loop and no 401 rebuild leg; the
// attempt is accounted by the turn loop and retried by the launch gate's pace.
// Pre-agent refusals of a Drain are typed and pace the gate.
package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// runInternalCallsFor runs runInternal for one prompt (ctx decides Drain or
// not) against a mock agent whose every attempt writes an error-finish,
// no-progress assistant row (an empty stream) and returns nil -- exactly what
// the transient-retry loop re-runs -- and returns how many times the agent ran.
func runInternalCallsFor(t *testing.T, ctx context.Context, providerID string) int {
	t.Helper()
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID: providerID, Type: "openai",
		Models: []catwalk.Model{{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096}},
	})
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel
	coord := &coordinator{
		cfg: cfg, sessions: env.sessions, messages: env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	sess, err := env.sessions.Create(t.Context(), "run-internal-"+providerID)
	require.NoError(t, err)

	calls := 0
	coord.currentAgent = newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls++
		row, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.Finish{Reason: message.FinishReasonError, Message: "Empty response"},
			},
		})
		require.NoError(t, err)
		if call.OnAssistantMessageCreated != nil {
			call.OnAssistantMessageCreated(row.ID)
		}
		return nil, nil
	})
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	_, _ = coord.runInternal(ctx, sess.ID, "", pinned)
	return calls
}

// TestRunInternal_DrainIsOneAttempt: the same empty-stream outcome is retried
// by the coordinator for an ordinary turn but never for a Drain.
//
// Revert-check: removing the `if agentCall.IsDrain { maxRetries = 0 }` guard
// makes the Drain run three times and this test goes red.
func TestRunInternal_DrainIsOneAttempt(t *testing.T) {
	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	require.Equal(t, 3, runInternalCallsFor(t, t.Context(), "test-ordinary-retry"),
		"precondition: an ordinary turn IS retried by the coordinator")
	require.Equal(t, 1, runInternalCallsFor(t, WithDrainCall(t.Context()), "test-drain-once"),
		"a Drain is one provider attempt")
}

// TestDrainRefused_TypedAndPacesOnlyDrainCalls: a pre-agent refusal of a
// Drain call says the Drain never reached the provider and paces the gate; an
// ordinary call keeps its error and leaves the gate alone.
//
// Revert-check: returning err unwrapped (or pacing for every call kind) turns
// an assertion red.
func TestDrainRefused_TypedAndPacesOnlyDrainCalls(t *testing.T) {
	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.coord = coord // the gate lives in the coordinator's arbiter
	cause := errModelProviderNotConfigured

	err := coord.drainRefused(t.Context(), "s-ordinary", cause)
	require.ErrorIs(t, err, cause)
	require.False(t, IsDrainNotAttempted(err))
	open, _, _ := gateOpen(coord, "s-ordinary", time.Now())
	require.True(t, open, "an ordinary call leaves the gate alone")

	err = coord.drainRefused(WithDrainCall(t.Context()), "s-drain", cause)
	require.ErrorIs(t, err, cause, "the refusal stays reachable")
	require.True(t, IsDrainNotAttempted(err))
	open, _, _ = gateOpen(coord, "s-drain", time.Now())
	require.False(t, open, "a refused Drain paces the gate")

	require.NoError(t, coord.drainRefused(WithDrainCall(t.Context()), "s-drain", nil))
}
