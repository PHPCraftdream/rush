package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCallOptionsSpecWebTurnOptInSerialization(t *testing.T) {
	for _, optedIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "opted_in"}[optedIn], func(t *testing.T) {
			original := &CallOptions{WebTurnOptIn: optedIn}
			spec := toSessionCallOptionsSpec(original)
			encoded, err := json.Marshal(spec)
			require.NoError(t, err)
			var durable session.CallOptionsSpec
			require.NoError(t, json.Unmarshal(encoded, &durable))
			restored, err := fromSessionCallOptionsSpec(&durable)
			require.NoError(t, err)
			require.Equal(t, optedIn, restored.WebTurnOptIn)
			require.Equal(t, optedIn, WebModelPolicy(WithCallOptions(context.Background(), restored)))
		})
	}
}

func TestWebModelPolicyRequiresOptInAndSessionPreferencesArePersisted(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coord.cfg.Config().Providers.Set("session-smart-provider", config.ProviderConfig{
		ID: "session-smart-provider", Type: openai.Name, APIKey: "test-key",
		Models: []catwalk.Model{{ID: "session-smart-model"}},
	})
	coord.cfg.Config().Providers.Set("session-fast-provider", config.ProviderConfig{
		ID: "session-fast-provider", Type: openai.Name, APIKey: "test-key",
		Models: []catwalk.Model{{ID: "session-fast-model"}},
	})
	sess, err := env.sessions.Create(t.Context(), "web model preference policy")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "session-smart-provider", Model: "session-smart-model"},
		&session.ModelSlotUpdate{Provider: "session-fast-provider", Model: "session-fast-model"}))

	defaultResolved, err := coord.resolveSessionModelsInternal(t.Context(), sess.ID, false)
	require.NoError(t, err)
	webCtx := WithCallOptions(t.Context(), &CallOptions{WebTurnOptIn: true})
	webResolved, err := coord.resolveSessionModelsInternal(webCtx, sess.ID, false)
	require.NoError(t, err)

	require.NotEqual(t, "session-smart-model", defaultResolved.smart.ModelCfg.Model)
	require.NotEqual(t, "session-fast-model", defaultResolved.fast.ModelCfg.Model)
	require.Equal(t, "session-smart-model", webResolved.smart.ModelCfg.Model)
	require.Equal(t, "session-fast-model", webResolved.fast.ModelCfg.Model)
}
