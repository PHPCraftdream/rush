// sessionPromptForCall's mode reconciliation (A24, T9): the session's stored
// system prompt is a cache keyed on nothing at all today, so a session created
// before a worker was configured keeps running a prompt that says "edit the
// file yourself" against a tool set that has no edit tool. The stored prompt
// now advertises its mode through prompt.OrchestratorRuleMarker and is rebuilt
// — once per mode change — when the call's own mode, resolvedOverrides
// .orchestrator, disagrees.

package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/require"
)

// newSessionPromptCoordinator builds the smallest coordinator that resolves a
// session's models AND its stored system prompt: the config store, the DB
// services and the real coder prompt template. No worker slot yet — the tests
// add and remove one to switch the call's mode.
func newSessionPromptCoordinator(t *testing.T, env fakeEnv) *coordinator {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	register := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:     providerID,
			Type:   "openai",
			Models: []catwalk.Model{{ID: modelID}},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}
	cfg.Config().Models[config.SelectedModelTypeSmart] = register("smart-provider", "smart-model")
	cfg.Config().Models[config.SelectedModelTypeFast] = register("fast-provider", "fast-model")
	cfg.SetupAgents()

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	return &coordinator{
		cfg: cfg, sessions: env.sessions, messages: env.messages,
		permissions: env.permissions, history: env.history, filetracker: *env.filetracker,
		prompt: p, modelCache: csync.NewMap[string, cachedModelPair](),
	}
}

func setWorkerSlot(t *testing.T, coord *coordinator) {
	t.Helper()
	coord.cfg.Config().Providers.Set("worker-provider", config.ProviderConfig{
		ID: "worker-provider", Type: "openai",
		Models: []catwalk.Model{{ID: "worker-model"}},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{
		Provider: "worker-provider", Model: "worker-model",
	}
}

func clearWorkerSlot(t *testing.T, coord *coordinator) {
	t.Helper()
	delete(coord.cfg.Config().Models, config.SelectedModelTypeWorker)
}

func storedPrompt(t *testing.T, coord *coordinator, ctx context.Context, sessionID string) string {
	t.Helper()
	sess, err := coord.sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	return sess.SystemPrompt
}

// Revert-check: pointing both call sites back at resolveSessionSystemPrompt
// keeps the stored prompt of the pre-worker session forever, so the
// prompt_switched_to_orchestrator leg fails on the missing rule 7 marker.
func TestSessionPromptForCall_MatchesTheCallMode(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t)
	coord := newSessionPromptCoordinator(t, env)
	sess, err := coord.sessions.Create(ctx, "prompt-mode")
	require.NoError(t, err)

	// (a) no worker: the built prompt has no rule 7, and the first call
	// persists it — the resolve-and-persist contract is unchanged.
	pinned, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, pinned.orchestrator, "no worker slot: the call is not in orchestrator mode")
	got := coord.sessionPromptForCall(ctx, sess.ID, pinned)
	require.NotContains(t, got, prompt.OrchestratorRuleMarker)
	require.Equal(t, got, storedPrompt(t, coord, ctx, sess.ID), "the first call persists the prompt it builds")

	// (b) same mode again: the stored prompt is a cache, NOT rewritten.
	require.NoError(t, coord.sessions.UpdateSystemPrompt(ctx, sess.ID, got+"\ncustom tail"))
	same, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, same.orchestrator)
	require.Equal(t, got+"\ncustom tail", coord.sessionPromptForCall(ctx, sess.ID, same),
		"a prompt built in the same mode is returned untouched")

	// (c) the operator configures a worker: the call's tool set is
	// orchestrator-shaped while the stored prompt is not, so the prompt is
	// rebuilt and re-saved — exactly one cache miss for the mode change.
	setWorkerSlot(t, coord)
	withWorker, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, withWorker.orchestrator, "a configured worker puts the call in orchestrator mode")
	rebuilt := coord.sessionPromptForCall(ctx, sess.ID, withWorker)
	require.Contains(t, rebuilt, prompt.OrchestratorRuleMarker, "the rebuilt prompt carries rule 7")
	stored := storedPrompt(t, coord, ctx, sess.ID)
	require.Contains(t, stored, prompt.OrchestratorRuleMarker, "the session row was re-saved")
	require.NotContains(t, stored, "custom tail", "the re-save replaced the old prompt")

	// (d) the reverse change drops the marker again and re-saves.
	clearWorkerSlot(t, coord)
	withoutWorker, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, withoutWorker.orchestrator)
	back := coord.sessionPromptForCall(ctx, sess.ID, withoutWorker)
	require.NotContains(t, back, prompt.OrchestratorRuleMarker)
	require.NotContains(t, storedPrompt(t, coord, ctx, sess.ID), prompt.OrchestratorRuleMarker)

	// (e) a session with no stored prompt at all still goes through the
	// resolve-and-persist path, whatever the call's mode.
	fresh, err := coord.sessions.Create(ctx, "prompt-mode-fresh")
	require.NoError(t, err)
	seeded := coord.sessionPromptForCall(ctx, fresh.ID, withWorker)
	require.NotEmpty(t, seeded)
	require.Equal(t, seeded, storedPrompt(t, coord, ctx, fresh.ID))
	require.True(t, strings.Contains(seeded, prompt.OrchestratorRuleMarker) || strings.Contains(seeded, "You are Rush"),
		"the fallback path builds and persists a prompt")
}

// TestBuildCall_UsesTheReconciledSessionPrompt pins the buildCall call site
// (coordinator_run.go). resolveTurnConfig places SystemPromptOverride ABOVE the
// call's own pinned SystemPrompt, so a stale stored prompt would win over the
// freshly built one for every later turn of the session — exactly the A24
// shape, where the override is the reconciled prompt instead of the raw row.
//
// Revert-check: pointing this call site back at resolveSessionSystemPrompt
// returns the stale row, so the marker assertions below fail.
func TestBuildCall_UsesTheReconciledSessionPrompt(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t)
	coord := newSessionPromptCoordinator(t, env)
	sess, err := coord.sessions.Create(ctx, "prompt-mode-build")
	require.NoError(t, err)
	require.NoError(t, coord.sessions.UpdateSystemPrompt(ctx, sess.ID,
		"You are Rush, an AI coding assistant.\ncustom tail"))

	setWorkerSlot(t, coord)
	pinned, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, pinned.orchestrator, "precondition: the worker slot puts the call in orchestrator mode")

	call, err := coord.buildCall(ctx, sess.ID, "prompt", pinned, nil)
	require.NoError(t, err)
	require.Contains(t, call.SystemPromptOverride, prompt.OrchestratorRuleMarker,
		"the override must be the reconciled prompt, not the stale stored row")
	require.NotContains(t, call.SystemPromptOverride, "custom tail")
	require.Contains(t, storedPrompt(t, coord, ctx, sess.ID), prompt.OrchestratorRuleMarker,
		"the call site re-saved the session row too")
}

// TestRunInternal_UsesTheReconciledSessionPrompt pins the runInternal call site
// (the other half of the same replacement) through the mock agent: the
// SessionAgentCall the agent is actually handed carries the reconciled prompt
// as its override.
//
// Revert-check: pointing this call site back at resolveSessionSystemPrompt
// hands the agent the stale row, so the marker assertions below fail.
func TestRunInternal_UsesTheReconciledSessionPrompt(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t)
	coord := newSessionPromptCoordinator(t, env)
	sess, err := coord.sessions.Create(ctx, "prompt-mode-run")
	require.NoError(t, err)
	require.NoError(t, coord.sessions.UpdateSystemPrompt(ctx, sess.ID,
		"You are Rush, an AI coding assistant.\ncustom tail"))

	setWorkerSlot(t, coord)
	pinned, err := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, pinned.orchestrator, "precondition: the worker slot puts the call in orchestrator mode")

	var observed SessionAgentCall
	coord.currentAgent = newMockAgent("smart-provider", 4096,
		func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			observed = call
			return agentResultWithText("ok"), nil
		})

	_, err = coord.runInternal(ctx, sess.ID, "prompt", pinned)
	require.NoError(t, err)
	require.NotEmpty(t, observed.SystemPromptOverride, "the agent must be handed an override")
	require.Contains(t, observed.SystemPromptOverride, prompt.OrchestratorRuleMarker,
		"the override must be the reconciled prompt, not the stale stored row")
	require.NotContains(t, observed.SystemPromptOverride, "custom tail")
}
