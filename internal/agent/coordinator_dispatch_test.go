package agent

// Session-dispatch test for the interrupt ticker: handleInterruptTick
// consuming pending_injects rows and routing them through
// InterruptAndReplace without duplicating user messages.

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleInterruptTick exercises the interrupt-inject tick handler in
// isolation (no live provider): it seeds an interrupt=true pending_injects row
// referencing an already-persisted user message, then asserts the handler
// consumes the row, queues a call that points at the EXISTING message (no
// duplicate create), and cancels the running turn. A second tick with no
// interrupt row must be a no-op.
func TestHandleInterruptTick(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	})
	// P0-2's atomic-handoff rewrite made handleInterruptTick call
	// resolveSessionModels before buildCall (closing the same
	// session-scoped-model bug P1-4 fixed elsewhere), so both smart and
	// fast must be resolvable from config — the old nil-pinned buildCall
	// fallback (c.currentAgent.Model()) that let this test skip config
	// entirely no longer exists.
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}

	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: agent,
		modelCache:   csync.NewMap[string, cachedModelPair](),
	}

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "interrupt-tick")
	require.NoError(t, err)

	// The CLI (`crush sessions inject --interrupt`) creates the user message
	// AND the interrupt row; simulate both here.
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "stop and do X"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: "stop and do X", Interrupt: true,
	}))

	t.Run("fires on interrupt row: queues existing message + cancels", func(t *testing.T) {
		fired, err := coord.handleInterruptTick(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, fired)

		// P0-1 fix (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
		// handleInterruptTick now marks the call with FromDurableQueue=true and
		// InterruptAndReplace skips mb.replacement for such calls to avoid
		// double-execution. The durable queue is now the sole owner; only the
		// current generation is cancelled. The test still verifies InterruptAndReplace
		// was called (to cancel the in-flight turn) and that the call has
		// FromDurableQueue=true set.
		require.Len(t, agent.interruptAndReplaced, 1)
		q := agent.interruptAndReplaced[0]
		assert.Equal(t, msg.ID, q.ExistingMessageID, "must reference existing message, not create a new one")
		assert.Equal(t, "stop and do X", q.Prompt)
		assert.True(t, q.FromDurableQueue, "must be marked as from durable queue to skip mb.replacement")
		assert.Empty(t, agent.queuedCalls, "must not fall back to QueueMessage when a live turn was interrupted")
		assert.Empty(t, agent.cancelled, "InterruptAndReplace cancels the generation itself — no separate Cancel call")

		// No new user message row was created — history still holds exactly
		// the one the CLI created.
		msgs, err := env.messages.List(ctx, sess.ID)
		require.NoError(t, err)
		userCount := 0
		for _, m := range msgs {
			if m.Role == message.User {
				userCount++
			}
		}
		assert.Equal(t, 1, userCount, "no duplicate user message in history")
	})

	t.Run("no interrupt row is a no-op", func(t *testing.T) {
		fired, err := coord.handleInterruptTick(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, fired)
		// No ADDITIONAL interrupt activity beyond the one the previous
		// subtest already recorded.
		assert.Len(t, agent.interruptAndReplaced, 1)
		assert.Empty(t, agent.queuedCalls)
		assert.Empty(t, agent.cancelled)
	})
}
