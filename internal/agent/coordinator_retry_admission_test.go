package agent

// R7-1 (round-7 audit) regression coverage. This file complements
// TestRunInternal_QueuedAttemptNotRetriedOnStaleEvidence in
// coordinator_retry_evidence_test.go, which covers the stale-evidence
// case with NO callback at all: a queued (nil, nil) attempt whose
// per-attempt capture stays empty. Here the queued copy's callback DOES
// fire -- the report's early-dispatch-before-seal ordering: a concurrent
// dispatcher drains the queued call and its turn reports the row ID into
// the sender's still-unsealed capture before the sender's resolve()
// seals it -- and the coordinator must still refuse to retry, because
// the queued admission outcome gates classification before any attempt
// evidence is consulted.

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunInternal_QueuedAdmissionEarlyCallbackNotRetried reproduces the
// R7-1 mechanism at the coordinator's own boundary: agent.Run reports a
// queued admission (marking the recorder exactly like agent_run.go's
// queueing branch) while the queued copy's OnAssistantMessageCreated has
// ALREADY fired into the sender's still-unsealed attempt capture,
// recording a terminal error-finish no-progress row (a plain 400: no
// transient signal, no progress). The admission gate must end the retry
// loop before the classifiers ever see that evidence: exactly one
// provider call, no continuation prompt persisted, the (nil, nil) queued
// contract preserved upward.
func TestRunInternal_QueuedAdmissionEarlyCallbackNotRetried(t *testing.T) {
	const providerID = "test-queued-early-callback"
	const prompt = "A's prompt that gets queued and dies a terminal death"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "queued-early-callback")
	require.NoError(t, err)

	callCount := 0
	var seenPrompts []string
	agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		seenPrompts = append(seenPrompts, call.Prompt)
		// The queued copy's dispatch, executed by another dispatcher
		// before the sender returns: it writes a genuine TERMINAL
		// failure row -- error finish, NO progress (no text, reasoning,
		// or tool calls; the plain-400 shape) -- and fires the callback
		// into the sender's still-unsealed capture.
		badRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.Finish{Reason: message.FinishReasonError, Message: "Bad Request"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, call.OnAssistantMessageCreated)
		call.OnAssistantMessageCreated(badRow.ID)
		// agent_run.go's queueing branch: mark the direct caller's
		// admission recorder, then the historical (nil, nil) return.
		if adm := turnAdmissionFrom(ctx); adm != nil {
			adm.markQueued()
		}
		return nil, nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err, "the queued contract must be preserved upward: (nil, nil), not an error")
	assert.Nil(t, res)
	assert.Equal(t, 1, callCount, "a queued admission must end the retry loop before the classifiers see the queued copy's evidence -- no second provider call")
	require.Len(t, seenPrompts, 1)
	assert.Equal(t, prompt, seenPrompts[0], "the only prompt is the original; no continuation or retry rewrite may be issued")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.Role == message.User {
			assert.False(t, IsContinuationPrompt(m.Content().Text), "no continuation prompt may be persisted for a queued admission")
		}
	}
}
