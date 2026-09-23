package agent

// R8-3 residual (round-9 audit) regression coverage.
//
// The bug: RunWithReservedOwnership (the FailIfSessionBusy=true +
// model-override entry point, internal/app/app_run.go's runFn picks it
// when reservedHold != nil && modelOverrideRequested) builds its call via
// buildCall and hands off directly to SessionAgent.RunWithReservedOwnership
// -- entirely bypassing runInternal, which is the only place the R8-3
// CallResultRecorder used to get wired onto a call's
// OnAssistantMessageCreated. An empty recorder made internal/app's
// terminal reconciliation fall back to "any newer row not in my baseline
// is mine", the exact independent-later-caller race the recorder exists
// to close (R8-3's original scenario).
//
// Verified by revert: removing the `if callResult != nil {
// call.OnAssistantMessageCreated = callResult.record }` block in
// coordinator.RunWithReservedOwnership (coordinator_run.go) makes this
// test fail: recorder.Resolve() stays "" instead of the committed row's ID.

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunWithReservedOwnership_RecordsCallResultIdentity_R8_3(t *testing.T) {
	const providerID = "test-reserved-ownership-result-identity"

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

	sess, err := env.sessions.Create(t.Context(), "reserved-ownership-result-identity")
	require.NoError(t, err)

	var recordedID string
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		row, createErr := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "ok"}},
		})
		require.NoError(t, createErr)
		row.AddFinish(message.FinishReasonEndTurn, "", "")
		require.NoError(t, env.messages.Update(t.Context(), row))
		require.NotNil(t, call.OnAssistantMessageCreated,
			"RunWithReservedOwnership must wire the caller's call-result recorder onto the call")
		call.OnAssistantMessageCreated(row.ID)
		recordedID = row.ID
		return agentResultWithText("ok"), nil
	})
	coord.currentAgent = agent

	recorder := NewCallResultRecorder()
	ctx := WithCallResultRecorder(t.Context(), recorder)

	holdCtx, epoch, cancel, ok := coord.ReserveExclusive(ctx, sess.ID)
	require.True(t, ok, "ReserveExclusive must succeed on an idle session")

	res, err := coord.RunWithReservedOwnership(holdCtx, sess.ID, "prompt", epoch, cancel, func() {}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, recordedID, "the mock agent must have committed a row")

	assert.Equal(t, recordedID, recorder.Resolve(),
		"R8-3 residual: RunWithReservedOwnership must report its own committed message ID into the "+
			"caller's CallResultRecorder instead of leaving it empty")
}
