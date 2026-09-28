// Phase-4 step 3: a call built from an active call never inherits its kind
// or notice flags (doc sec.3.4). `sessions inject --interrupt` landing on a
// Drain turn must run the operator's message as an ordinary call.
package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newInterruptDrainFixture(t *testing.T, active func(sessionID string) SessionAgentCall) (*coordinator, *mockSessionAgent, fakeEnv, string) {
	t.Helper()
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("operator-provider", config.ProviderConfig{
		ID: "operator-provider", Type: openai.Name,
		Models: []catwalk.Model{{ID: "operator-model", Name: "Operator Model"}},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "operator-provider", Model: "operator-model"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: "operator-provider", Model: "operator-model"}

	current := newMockAgent("operator-provider", 4096, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	sess, err := env.sessions.Create(t.Context(), "interrupt-during-drain")
	require.NoError(t, err)
	current.activeCall = active(sess.ID)
	current.hasActiveCall = true
	coord := &coordinator{
		cfg: cfg, sessions: env.sessions, messages: env.messages,
		currentAgent: current, modelCache: csync.NewMap[string, cachedModelPair](),
	}

	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "operator interrupt"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: msg.FullText(), Interrupt: true,
	}))
	return coord, current, env, sess.ID
}

// TestHandleInterruptTick_DrainActiveCallNotInherited: the durable interrupt
// path copies the active call's policy shape; a Drain's kind and notice
// flags must not ride along -- neither onto the in-process replacement nor
// into the durable row the pump rebuilds from.
//
// Revert-check performed: replaced `callFromActive(activeCall)` with the
// plain `activeCall` copy in handleInterruptTick -- this test FAILED
// (replacement.IsDrain true, AutoResumed true). Restored; re-ran, passed.
func TestHandleInterruptTick_DrainActiveCallNotInherited(t *testing.T) {
	coord, current, env, sessionID := newInterruptDrainFixture(t, func(id string) SessionAgentCall {
		return newDrainCall(SessionAgentCall{SessionID: id, NoticeKind: "supervision"})
	})

	fired, err := coord.handleInterruptTick(t.Context(), sessionID)
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, current.interruptAndReplaced, 1)
	replacement := current.interruptAndReplaced[0]
	require.False(t, replacement.IsDrain, "the operator's message must not run as a Drain")
	require.False(t, replacement.AutoResumed)
	require.False(t, replacement.BackgroundJobNotice)
	require.Empty(t, replacement.NoticeKind)
	require.Equal(t, "operator interrupt", replacement.Prompt)

	pending, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	var data session.SessionAgentCallData
	require.NoError(t, json.Unmarshal([]byte(pending[0].CallData), &data))
	require.False(t, data.AutoResumed, "the durable row must not carry the Drain's notice flags")
	require.False(t, data.BackgroundJobNotice)
}

// TestHandleActiveNonDurableInterrupt_DrainActiveCallNotInherited: same rule
// on the non-durable (credentialed) interrupt path.
//
// Revert-check performed: replaced `callFromActive(active)` with `active` in
// handleActiveNonDurableInterrupt -- this test FAILED (replacement.IsDrain
// true). Restored; re-ran, passed.
func TestHandleActiveNonDurableInterrupt_DrainActiveCallNotInherited(t *testing.T) {
	creds := &CredentialSet{Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "k"}}}
	coord, current, _, sessionID := newInterruptDrainFixture(t, func(id string) SessionAgentCall {
		call := newDrainCall(SessionAgentCall{SessionID: id})
		call.Credentials = creds
		return call
	})

	fired, err := coord.handleInterruptTick(t.Context(), sessionID)
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, current.interruptAndReplaced, 1)
	replacement := current.interruptAndReplaced[0]
	require.False(t, replacement.IsDrain, "the operator's message must not run as a Drain")
	require.False(t, replacement.AutoResumed)
	require.False(t, replacement.BackgroundJobNotice)
	require.Same(t, creds, replacement.Credentials, "the active call's non-durable shape is still kept")
}
