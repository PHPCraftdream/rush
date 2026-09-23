package agent

// R9-2 (round-9 audit) regression coverage.
//
// The bug: RunWithOverrides fills in an omitted smart/fast slot from the
// target session's durable override (SmartModelID/FastModelID) on its own
// LOCAL smart/fast variables before calling applyModelOverrides -- but
// never re-arms ctx with the augmented pair. runInternal's 401-rebuild path
// (credentials.go's resolveCallModels) reads modelOverridesFrom(ctx), which
// still carries whatever the ORIGINAL caller armed (or nothing at all for
// a plain RunWithOverrides caller) -- never this function's own inherited
// fill-in. A nil slot then falls through applyModelOverrides to the
// GLOBAL config default, even though the session has its own persisted
// choice and the FIRST attempt (built from the locally-augmented pair)
// used it correctly.
//
// Verified by revert: removing `ctx = WithModelOverrides(ctx, smart, fast)`
// in RunWithOverrides (coordinator_run.go) makes this test fail: the
// second (rebuilt) call's SmartModel.Provider reverts to "global-provider"
// instead of staying "session-provider".

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRunWithOverrides_401RebuildKeepsInheritedSmartSlot_R9_2(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	// The session's OWN durable smart slot, distinct from the global
	// default -- the rebuild must keep using THIS one.
	cfg.Config().Providers.Set("session-provider", config.ProviderConfig{
		ID:         "session-provider",
		Type:       "openai",
		OAuthToken: &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Models:     []catwalk.Model{{ID: "session-model", DefaultMaxTokens: 4096}},
	})
	// The GLOBAL default smart slot -- must NEVER be what the rebuild uses
	// for this session, which has its own override.
	cfg.Config().Providers.Set("global-provider", config.ProviderConfig{
		ID:     "global-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "global-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Providers.Set("fast-provider", config.ProviderConfig{
		ID:     "fast-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "fast-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "global-provider", Model: "global-model"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: "fast-provider", Model: "fast-model"}

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	coord.refreshOAuth2TokenFn = func(context.Context, config.ProviderConfig) error { return nil }

	sess, err := env.sessions.Create(t.Context(), "r9-2 overrides rebuild inheritance")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "session-provider", Model: "session-model"}, nil))

	var calls []SessionAgentCall
	agent := newMockAgent("session-provider", 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		if len(calls) == 1 {
			return nil, c6Unauthorized()
		}
		return agentResultWithText("rebuilt with the inherited slot"), nil
	})
	coord.currentAgent = agent

	// Only the FAST slot is explicitly overridden; smart is omitted and
	// must inherit the session's own durable choice, both on the first
	// attempt AND across the 401 rebuild. ctx carries the SAME (nil,
	// fastOverride) pair app_run_setup.go arms for a real --fast-only CLI
	// invocation (WithModelOverrides, modelOverrideRequested branch) --
	// without that, modelOverridesFrom(ctx) would read (nil, nil) on
	// rebuild and resolveCallModels would fall through to
	// resolveSessionModels instead, which happens to already read the
	// session's own slots correctly and would mask this exact bug.
	fastOverride := &ModelOverride{Provider: "fast-provider", Model: "fast-model"}
	ctx := WithModelOverrides(t.Context(), nil, fastOverride)
	_, err = coord.RunWithOverrides(ctx, sess.ID, "prompt", nil, fastOverride)
	require.NoError(t, err)
	require.Len(t, calls, 2, "the 401 must have triggered exactly one rebuild+retry")

	for i, call := range calls {
		require.NotNil(t, call.SmartModel, "call %d", i)
		require.Equal(t, "session-provider", call.SmartModel.ModelCfg.Provider,
			"call %d: the omitted smart slot must inherit the session's own durable override, never the global default", i)
		require.Equal(t, "session-model", call.SmartModel.ModelCfg.Model, "call %d", i)
	}
}
