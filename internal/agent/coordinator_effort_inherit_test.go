package agent

// The session's durable reasoning effort was recorded for the specific
// model it ran (SmartModelID/FastModelID). An override naming a DIFFERENT
// model must not inherit it: applyModelOverrides (coordinator_models.go)
// deliberately clears the effort when the model changes, because running
// model B with model A's effort means higher cost -- or an outright
// provider rejection for models that don't support that effort level.
//
// Revert check: restoring the old unconditional fill-in (dropping the
// literal provider compare in inheritSessionEffort, coordinator_run.go)
// fails TestInheritSessionEffort's different-model cases, the
// "empty provider is a different provider" subtest (which pins the literal
// provider compare), and TestRunWithOverrides_EffortNotInheritedForDifferentModel.
// Additionally, replacing inheritSessionEffort's call in
// RunWithReservedOwnership with the old unconditional fill-in fails
// TestRunWithReservedOwnership_EffortNotInheritedForDifferentModel.

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestInheritSessionEffort(t *testing.T) {
	base := session.Session{
		SmartModelProvider:        "prov",
		SmartModelID:              "model-a",
		SmartModelReasoningEffort: "high",
		FastModelProvider:         "fprov",
		FastModelID:               "fmodel-a",
		FastModelReasoningEffort:  "medium",
	}

	t.Run("different model same provider not inherited", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "prov", Model: "model-b"}
		inheritSessionEffort(smart, nil, sess)
		require.Empty(t, smart.ReasoningEffort)
	})

	t.Run("same model inherited", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "prov", Model: "model-a"}
		inheritSessionEffort(smart, nil, sess)
		require.Equal(t, "high", smart.ReasoningEffort)
	})

	t.Run("explicit effort kept", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "prov", Model: "model-b", ReasoningEffort: "low"}
		inheritSessionEffort(smart, nil, sess)
		require.Equal(t, "low", smart.ReasoningEffort)
	})

	t.Run("different provider same model not inherited", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "other", Model: "model-a"}
		inheritSessionEffort(smart, nil, sess)
		require.Empty(t, smart.ReasoningEffort)
	})

	t.Run("empty provider is a different provider: not inherited", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "", Model: "model-a"}
		inheritSessionEffort(smart, nil, sess)
		require.Empty(t, smart.ReasoningEffort)
	})

	t.Run("session without effort leaves empty", func(t *testing.T) {
		sess := base
		sess.SmartModelReasoningEffort = ""
		smart := &ModelOverride{Provider: "prov", Model: "model-a"}
		inheritSessionEffort(smart, nil, sess)
		require.Empty(t, smart.ReasoningEffort)
	})

	t.Run("fast different model same provider not inherited", func(t *testing.T) {
		sess := base
		fast := &ModelOverride{Provider: "fprov", Model: "fmodel-b"}
		inheritSessionEffort(nil, fast, sess)
		require.Empty(t, fast.ReasoningEffort)
	})

	t.Run("fast same model inherited", func(t *testing.T) {
		sess := base
		fast := &ModelOverride{Provider: "fprov", Model: "fmodel-a"}
		inheritSessionEffort(nil, fast, sess)
		require.Equal(t, "medium", fast.ReasoningEffort)
	})

	t.Run("fast explicit effort kept", func(t *testing.T) {
		sess := base
		fast := &ModelOverride{Provider: "fprov", Model: "fmodel-b", ReasoningEffort: "low"}
		inheritSessionEffort(nil, fast, sess)
		require.Equal(t, "low", fast.ReasoningEffort)
	})

	t.Run("fast different provider same model not inherited", func(t *testing.T) {
		sess := base
		fast := &ModelOverride{Provider: "fother", Model: "fmodel-a"}
		inheritSessionEffort(nil, fast, sess)
		require.Empty(t, fast.ReasoningEffort)
	})

	t.Run("both slots at once", func(t *testing.T) {
		sess := base
		smart := &ModelOverride{Provider: "prov", Model: "model-a"}
		fast := &ModelOverride{Provider: "fprov", Model: "fmodel-a"}
		inheritSessionEffort(smart, fast, sess)
		require.Equal(t, "high", smart.ReasoningEffort)
		require.Equal(t, "medium", fast.ReasoningEffort)
	})

	t.Run("nil safety", func(t *testing.T) {
		sess := base
		require.NotPanics(t, func() { inheritSessionEffort(nil, nil, sess) })

		smart := &ModelOverride{Provider: "prov", Model: "model-a"}
		require.NotPanics(t, func() { inheritSessionEffort(smart, nil, sess) })
		require.Equal(t, "high", smart.ReasoningEffort)

		fast := &ModelOverride{Provider: "fprov", Model: "fmodel-a"}
		require.NotPanics(t, func() { inheritSessionEffort(nil, fast, sess) })
		require.Equal(t, "medium", fast.ReasoningEffort)
	})
}

func TestRunWithOverrides_EffortNotInheritedForDifferentModel(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	// The session's own smart slot: model A at effort "high"-class effort
	// (set below via UpdateReasoningEffort), provider "session-provider".
	cfg.Config().Providers.Set("session-provider", config.ProviderConfig{
		ID:     "session-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "session-model", DefaultMaxTokens: 4096}},
	})
	// The override's target: model B, a DIFFERENT model on another provider.
	cfg.Config().Providers.Set("other-provider", config.ProviderConfig{
		ID:     "other-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "other-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Providers.Set("fast-provider", config.ProviderConfig{
		ID:     "fast-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "fast-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "other-provider", Model: "other-model"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: "fast-provider", Model: "fast-model"}
	// config.Load skips SetupAgents when no provider is configured from the
	// environment (clean CI runners); NewCoordinator self-heals that, this
	// struct-literal coordinator must too, or the 401 path's UpdateModels
	// fails with errCoderAgentNotConfigured and the retry never runs.
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "effort not inherited for different model")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "session-provider", Model: "session-model"}, nil))
	require.NoError(t, env.sessions.UpdateReasoningEffort(t.Context(), sess.ID, "xhigh", ""))

	var calls []SessionAgentCall
	agent := newMockAgent("other-provider", 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		return agentResultWithText("ok"), nil
	})
	coord.currentAgent = agent

	// Scenario: the session ran model A (session-provider/session-model) at
	// effort "xhigh"; the override names model B (other-provider/
	// other-model) with NO effort -- model B must run WITHOUT model A's
	// "xhigh". This is the reviewer-pass / `rush run --model B` shape.
	override := &ModelOverride{Provider: "other-provider", Model: "other-model"}
	_, err = coord.RunWithOverrides(t.Context(), sess.ID, "prompt", override, nil)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "other-model", calls[0].SmartModel.ModelCfg.Model)
	require.Empty(t, calls[0].SmartModel.ModelCfg.ReasoningEffort,
		"an override for a different model must not inherit the session's effort")
}

func TestRunWithReservedOwnership_EffortNotInheritedForDifferentModel(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	// Same fixture as the RunWithOverrides twin test: the session's own
	// smart slot is model A; the global default (and the override target)
	// is model B.
	cfg.Config().Providers.Set("session-provider", config.ProviderConfig{
		ID:     "session-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "session-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Providers.Set("other-provider", config.ProviderConfig{
		ID:     "other-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "other-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Providers.Set("fast-provider", config.ProviderConfig{
		ID:     "fast-provider",
		Type:   "openai",
		Models: []catwalk.Model{{ID: "fast-model", DefaultMaxTokens: 4096}},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "other-provider", Model: "other-model"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: "fast-provider", Model: "fast-model"}
	// config.Load skips SetupAgents when no provider is configured from the
	// environment (clean CI runners); NewCoordinator self-heals that, this
	// struct-literal coordinator must too, or the 401 path's UpdateModels
	// fails with errCoderAgentNotConfigured and the retry never runs.
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	var calls []SessionAgentCall
	agent := newMockAgent("other-provider", 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		return agentResultWithText("ok"), nil
	})
	coord.currentAgent = agent

	// Session 1: model A at "xhigh"; the override names model B with NO
	// effort -- model B must run WITHOUT model A's "xhigh".
	foreign, err := env.sessions.Create(t.Context(), "reserved-ownership effort, other model")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), foreign.ID,
		&session.ModelSlotUpdate{Provider: "session-provider", Model: "session-model"}, nil))
	require.NoError(t, env.sessions.UpdateReasoningEffort(t.Context(), foreign.ID, "xhigh", ""))

	holdCtx, epoch, cancel, ok := coord.ReserveExclusive(t.Context(), foreign.ID)
	require.True(t, ok, "ReserveExclusive must succeed on an idle session")
	_, err = coord.RunWithReservedOwnership(holdCtx, foreign.ID, "prompt", epoch, cancel, func() {},
		&ModelOverride{Provider: "other-provider", Model: "other-model"}, nil)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "other-model", calls[0].SmartModel.ModelCfg.Model)
	require.Empty(t, calls[0].SmartModel.ModelCfg.ReasoningEffort,
		"the re-run path must not run model B with model A's session effort")

	// Positive control: an override naming the session's OWN model A still
	// inherits "xhigh" (this is what the fill-in exists for).
	own, err := env.sessions.Create(t.Context(), "reserved-ownership effort, own model")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), own.ID,
		&session.ModelSlotUpdate{Provider: "session-provider", Model: "session-model"}, nil))
	require.NoError(t, env.sessions.UpdateReasoningEffort(t.Context(), own.ID, "xhigh", ""))

	holdCtx, epoch, cancel, ok = coord.ReserveExclusive(t.Context(), own.ID)
	require.True(t, ok, "ReserveExclusive must succeed on an idle session")
	_, err = coord.RunWithReservedOwnership(holdCtx, own.ID, "prompt", epoch, cancel, func() {},
		&ModelOverride{Provider: "session-provider", Model: "session-model"}, nil)
	require.NoError(t, err)
	require.Len(t, calls, 2)
	require.Equal(t, "session-model", calls[1].SmartModel.ModelCfg.Model)
	require.Equal(t, "xhigh", calls[1].SmartModel.ModelCfg.ReasoningEffort,
		"the session's own model keeps its recorded effort on the re-run path")
}
