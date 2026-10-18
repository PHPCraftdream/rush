package agent

// Tests for the per-turn role-slot resolution (plan D2, shared resolver
// roleSlotOverride), the explicit --effort carrier (plan R6), and the
// dispatch-time sub-agent slot refresh (plan D3):
//
// REVERT-CHECKS (each test must fail on exactly the listed single change):
//
//	TestResolveSessionModelsInternal_RoleSlotCascade — drop the
//	  role-slot hunk in resolveSessionModelsInternal
//	  (coordinator_models.go, the `if smartOverride == nil { ... }` block):
//	  the worker role resolves the smart slot (m-smart) instead of the
//	  worker slot/pin, so every ModelCfg.Model and smartSource assertion
//	  fails.
//	TestApplyModelOverridesResolvesRoleSlot — drop the role cascade in
//	  applyModelOverrides: the smart slot stays m-smart instead of the
//	  reviewer slot, and smartSource stays per-call/slot@start.
//	TestApplyModelOverrides_PinWinsWithFastOverridePresent — revert
//	  applyModelOverrides' role branch to the config-only form (round 1):
//	  the config worker slot (w-model) is resolved instead of the session
//	  worker pin (w-pin), so the first case's model assertion fails.
//	TestRoleSlotOverride_CarriesExplicitEffort — drop the roleEffort
//	  application in roleSlotOverride: the effort falls back to the
//	  resolved slot's (empty), so the ReasoningEffort assertions fail.
//	TestRunSubAgent_DispatchRefreshesWorkerSlot — delete the dispatch-time
//	  refresh block in coordinator_subagents.go (:199-204), or drop its
//	  params.slotModel conjunct: the mock's parent-turn-start model (w1)
//	  reaches the dispatched SessionAgentCall instead of the live slot's
//	  w2.
//	TestRunSubAgent_NonSlotDispatchKeepsModel — delete the
//	  `params.slotModel &&` gate (refreshing EVERY sub-agent at dispatch,
//	  the pre-round-4 behaviour): the agentic_fetch-shaped non-slot
//	  dispatch is rebuilt from the swapped slot (w1 → w2) and the w1
//	  assertion fails.
//	TestSubAgentDispatchRefreshesWorkerSlot — drop the dispatch-time
//	  refresh: refreshSubAgentSlotModel always returns (model, false),
//	  so the `ok` assertion on the stale-model case fails.
//	TestRoleSlotOverride_SessionPinThenConfigSlot — drop the session-pin
//	  branch from roleSlotOverride: pinned sessions resolve the config
//	  slot with the slot@start label instead of the pin, so the first two
//	  assertions fail.
//	TestRoleSlotConfigModel — drop the config-slot cascade (or the
//	  ""/smart guard): the ok/model assertions fail.
import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// roleSlotTestConfig is a minimal pure config for the slot helpers: no
// providers are needed because the helpers only read cfg.Models.
func roleSlotTestConfig() *config.Config {
	return &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeWorker:   {Provider: "prov", Model: "w-model"},
			config.SelectedModelTypeFast:     {Provider: "prov", Model: "f-model"},
			config.SelectedModelTypeReviewer: {Provider: "prov", Model: "r-model"},
		},
	}
}

// TestRoleSlotConfigModel pins the config-slot cascade: only the three
// non-smart roles resolve, only when the slot exists and is non-empty.
// Revert-check: dropping the ""/smart guard or the empty-Model guard makes
// the respective ok=false assertions fail with ok=true.
func TestRoleSlotConfigModel(t *testing.T) {
	t.Parallel()
	cfg := roleSlotTestConfig()

	for _, role := range []config.SelectedModelType{"", config.SelectedModelTypeSmart} {
		sel, ok := roleSlotConfigModel(cfg, role)
		require.False(t, ok, string(role))
		require.Equal(t, config.SelectedModel{}, sel, string(role))
	}

	sel, ok := roleSlotConfigModel(cfg, config.SelectedModelTypeWorker)
	require.True(t, ok)
	require.Equal(t, "w-model", sel.Model)

	sel, ok = roleSlotConfigModel(cfg, config.SelectedModelTypeFast)
	require.True(t, ok)
	require.Equal(t, "f-model", sel.Model)

	sel, ok = roleSlotConfigModel(cfg, config.SelectedModelTypeReviewer)
	require.True(t, ok)
	require.Equal(t, "r-model", sel.Model)

	missing := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{}}
	_, ok = roleSlotConfigModel(missing, config.SelectedModelTypeWorker)
	require.False(t, ok, "a missing slot key is not configured")

	empty := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeWorker: {Provider: "prov", Model: ""},
	}}
	_, ok = roleSlotConfigModel(empty, config.SelectedModelTypeWorker)
	require.False(t, ok, "an empty Model is not configured")
}

// TestRoleSlotOverride_SessionPinThenConfigSlot pins the cascade order: the
// role's session pin wins over the config slot, and the heartbeat source
// label reflects which tier answered. Revert-check: dropping the session-pin
// branch makes every pinned case return the config slot labelled
// slot@start; dropping the config-slot fallback makes the unpinned cases
// return ok=false.
func TestRoleSlotOverride_SessionPinThenConfigSlot(t *testing.T) {
	t.Parallel()
	cfg := roleSlotTestConfig()

	workerPin := session.Session{
		WorkerModelProvider:        "pin-prov",
		WorkerModelID:              "pin-worker",
		WorkerModelReasoningEffort: "high",
	}
	ovr, src, ok := roleSlotOverride(cfg, workerPin, config.SelectedModelTypeWorker, "")
	require.True(t, ok)
	require.Equal(t, hbSourceSessionOverride, src)
	require.Equal(t, "pin-worker", ovr.Model)
	require.Equal(t, "high", ovr.ReasoningEffort)

	reviewerPin := session.Session{
		ReviewerModelProvider:        "pin-prov",
		ReviewerModelID:              "pin-reviewer",
		ReviewerModelReasoningEffort: "low",
	}
	ovr, src, ok = roleSlotOverride(cfg, reviewerPin, config.SelectedModelTypeReviewer, "")
	require.True(t, ok)
	require.Equal(t, hbSourceSessionOverride, src)
	require.Equal(t, "pin-reviewer", ovr.Model)

	ovr, src, ok = roleSlotOverride(cfg, session.Session{}, config.SelectedModelTypeWorker, "")
	require.True(t, ok)
	require.Equal(t, hbSourceSlotStart, src)
	require.Equal(t, "w-model", ovr.Model)

	ovr, src, ok = roleSlotOverride(cfg, session.Session{}, config.SelectedModelTypeFast, "")
	require.True(t, ok)
	require.Equal(t, hbSourceSlotStart, src)
	require.Equal(t, "f-model", ovr.Model)

	_, _, ok = roleSlotOverride(cfg, session.Session{}, config.SelectedModelTypeSmart, "")
	require.False(t, ok, "smart never resolves through a role slot")
}

// TestResolveSessionModelsInternal_RoleSlotCascade is the D2 end-to-end for
// ONE resolution: a --role worker call resolves the smart slot from the
// worker config slot, a session worker pin overrides it, and no role leaves
// smart untouched — all within a single resolveSessionModelsInternal call.
// It does NOT claim to reload the slot between requests (the "live"
// cross-request behavior is covered by the app-level integration tests).
// Revert-check: dropping the role-slot hunk in resolveSessionModelsInternal
// makes every assertion below fail: the smart slot resolves m-smart with
// source slot@start regardless of role/pin.
func TestResolveSessionModelsInternal_RoleSlotCascade(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "m-smart", Name: "M Smart", DefaultMaxTokens: 4096},
			{ID: "m-worker", Name: "M Worker", DefaultMaxTokens: 4096},
			{ID: "m-reviewer", Name: "M Reviewer", DefaultMaxTokens: 4096},
		},
	})
	cfg := coord.cfg.Config()
	cfg.Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "m-worker"}
	cfg.Models[config.SelectedModelTypeReviewer] = config.SelectedModel{Provider: prov, Model: "m-reviewer"}

	roleCtx := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeWorker})

	// No pins: the worker role resolves the worker CONFIG slot, live.
	sess, err := coord.sessions.Create(t.Context(), "role-slot-live")
	require.NoError(t, err)
	resolved, err := coord.resolveSessionModelsInternal(roleCtx, sess.ID, false)
	require.NoError(t, err)
	require.Equal(t, "m-worker", resolved.smart.ModelCfg.Model)
	require.Equal(t, hbSourceSlotStart, resolved.smartSource)
	require.Equal(t, "m-smart", resolved.fast.ModelCfg.Model, "fast is untouched by the role resolution")

	// A session worker pin beats the config slot and is labelled as such.
	pinned, err := coord.sessions.Create(t.Context(), "role-slot-pin")
	require.NoError(t, err)
	require.NoError(t, coord.sessions.UpdateWorkerReviewerModels(t.Context(), pinned.ID,
		&session.ModelSlotUpdate{Provider: prov, Model: "m-worker"}, nil))
	resolved, err = coord.resolveSessionModelsInternal(roleCtx, pinned.ID, false)
	require.NoError(t, err)
	require.Equal(t, "m-worker", resolved.smart.ModelCfg.Model)
	require.Equal(t, hbSourceSessionOverride, resolved.smartSource)

	// No role in the context: smart stays the smart slot (unchanged behavior).
	resolved, err = coord.resolveSessionModelsInternal(context.Background(), sess.ID, false)
	require.NoError(t, err)
	require.Equal(t, "m-smart", resolved.smart.ModelCfg.Model)
	require.Equal(t, hbSourceSlotStart, resolved.smartSource)
}

// TestApplyModelOverridesResolvesRoleSlot is D2 for explicit-override
// callers: an omitted smart override on a reviewer-role run takes the
// reviewer CONFIG slot from the same pinned snapshot. Fixture: this uses
// newTestCoordinator — the analogous TestApplyModelOverrides_PerCallSourceLabels
// already runs applyModelOverrides (including its unconditional
// pinCallTools) to completion on exactly this fixture shape.
// SetupAgents is required because newTestCoordinator's config.Init returns
// before Load's SetupAgents step when nothing extra is configured, leaving
// cfg.Agents empty and pinCallTools failing its coder-agent gate.
// Revert-check: dropping the role cascade in applyModelOverrides leaves the
// smart slot on m-smart with source slot@start, failing both assertions.
func TestApplyModelOverridesResolvesRoleSlot(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "m-smart", Name: "M Smart", DefaultMaxTokens: 4096},
			{ID: "m-reviewer", Name: "M Reviewer", DefaultMaxTokens: 4096},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeReviewer] = config.SelectedModel{Provider: prov, Model: "m-reviewer"}
	coord.cfg.SetupAgents()

	ctx := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeReviewer})
	resolved, err := coord.applyModelOverrides(ctx, "", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "m-reviewer", resolved.smart.ModelCfg.Model)
	require.Equal(t, hbSourceSlotStart, resolved.smartSource)
}

// TestApplyModelOverrides_PinWinsWithFastOverridePresent is P1-2: a session
// worker pin must survive a fast override riding along on the same call —
// the shared roleSlotOverride cascade reads the session, so the pin tier
// beats the config worker slot. The second case proves the config tier
// still fires when no pin exists.
// Revert-check: reverting applyModelOverrides' role branch to the
// config-only form (round 1) resolves the config worker slot (w-model)
// instead of the pin (w-pin), failing the first case's model assertion.
func TestApplyModelOverrides_PinWinsWithFastOverridePresent(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "m-smart", Name: "M Smart", DefaultMaxTokens: 4096},
			{ID: "w-model", Name: "W Model", DefaultMaxTokens: 4096},
		},
	})
	cfg := coord.cfg.Config()
	cfg.Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "w-model"}
	coord.cfg.SetupAgents()

	ctx := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeWorker})

	// A pinned session wins over the config slot even with a fast override.
	pinned, err := coord.sessions.Create(t.Context(), "pin-fast")
	require.NoError(t, err)
	require.NoError(t, coord.sessions.UpdateWorkerReviewerModels(t.Context(), pinned.ID,
		&session.ModelSlotUpdate{Provider: prov, Model: "w-pin"}, nil))
	require.NoError(t, coord.sessions.UpdateModels(t.Context(), pinned.ID, nil,
		&session.ModelSlotUpdate{Provider: prov, Model: "f-m"}))
	resolved, err := coord.applyModelOverrides(ctx, pinned.ID, nil, &ModelOverride{Provider: prov, Model: "f-m"})
	require.NoError(t, err)
	require.Equal(t, "w-pin", resolved.smart.ModelCfg.Model, "the session pin beats the config slot")
	require.Equal(t, "f-m", resolved.fast.ModelCfg.Model)
	require.Equal(t, hbSourceSessionOverride, resolved.smartSource)

	// No pins: the config worker slot is still the fallback tier.
	unpinned, err := coord.sessions.Create(t.Context(), "no-pin-fast")
	require.NoError(t, err)
	resolved, err = coord.applyModelOverrides(ctx, unpinned.ID, nil, &ModelOverride{Provider: prov, Model: "f-m"})
	require.NoError(t, err)
	require.Equal(t, "w-model", resolved.smart.ModelCfg.Model, "the config tier still fires without a pin")
	require.Equal(t, "f-m", resolved.fast.ModelCfg.Model)
}

// TestRoleSlotOverride_CarriesExplicitEffort is P2-6: an explicit --effort
// on a non-smart role run reaches the role slot's resolved model for THIS
// call, on both resolution paths, without touching the session.
// Revert-check: dropping the roleEffort application in roleSlotOverride
// lets the effort fall back to the resolved slot's (empty), failing both
// ReasoningEffort assertions.
func TestRoleSlotOverride_CarriesExplicitEffort(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "m-smart", Name: "M Smart", DefaultMaxTokens: 4096},
			{ID: "m-worker", Name: "M Worker", DefaultMaxTokens: 4096},
		},
	})
	cfg := coord.cfg.Config()
	cfg.Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "m-worker"}
	coord.cfg.SetupAgents()

	sess, err := coord.sessions.Create(t.Context(), "role-effort")
	require.NoError(t, err)
	ctx := WithCallOptions(
		WithRoleReasoningEffort(context.Background(), "high"),
		&CallOptions{ModelRole: config.SelectedModelTypeWorker},
	)

	resolved, err := coord.resolveSessionModelsInternal(ctx, sess.ID, false)
	require.NoError(t, err)
	require.Equal(t, "m-worker", resolved.smart.ModelCfg.Model)
	require.Equal(t, "high", resolved.smart.ModelCfg.ReasoningEffort)

	resolved, err = coord.applyModelOverrides(ctx, sess.ID, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "m-worker", resolved.smart.ModelCfg.Model)
	require.Equal(t, "high", resolved.smart.ModelCfg.ReasoningEffort)
}

// TestRunSubAgent_DispatchRefreshesWorkerSlot is P2-3's real dispatch test:
// the sub-agent model dispatched through the PRODUCTION entry point
// (runSubAgent) is rebuilt from the live worker slot, not the mock's
// parent-turn-start build. Mirrors
// TestRunSubAgent_CredentialWorkerFallbackUsesTenantSmart's harness.
// The subAgentParams literal carries slotModel: true — this represents the
// AGENT TOOL's dispatch path (agent_tool.go builds its child from the
// buildAgent role-slot cascade), the only path the dispatch-time refresh
// applies to.
// Revert-check: deleting the dispatch-time refresh block in
// coordinator_subagents.go (:199-204), or dropping its params.slotModel
// conjunct, leaves the mock's w1 on the
// dispatched SessionAgentCall, failing the w2 assertion.
func TestRunSubAgent_DispatchRefreshesWorkerSlot(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "w1", Name: "W1", DefaultMaxTokens: 4096},
			{ID: "w2", Name: "W2", DefaultMaxTokens: 4096},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "w1"}

	parent, err := env.sessions.Create(t.Context(), "dispatch-refresh")
	require.NoError(t, err)

	var observed SessionAgentCall
	agent := newMockAgent(prov, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = call
		return agentResultWithText("refreshed dispatch"), nil
	})
	// The mock's model represents the build at the PARENT TURN's start.
	agent.model = Model{ModelCfg: config.SelectedModel{Provider: prov, Model: "w1"}}

	// The operator swaps the worker slot mid-turn; the dispatch must pick
	// up w2, not the mock's stale w1.
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "w2"}
	ctx := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})
	resp, err := coord.runSubAgent(ctx, subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-1", ToolCallID: "call-1",
		Prompt: "go", SessionTitle: "Dispatch refresh", slotModel: true,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "w2", observed.SmartModel.ModelCfg.Model)
}

// TestRunSubAgent_NonSlotDispatchKeepsModel pins the agentic_fetch shape:
// a dispatch whose params.Agent model was NOT built from a config role slot
// (slotModel false, like agentic_fetch_tool.go's fast-built child) must NOT
// be refreshed at dispatch time — it keeps the model its parent turn
// started with. Mutation caught: deleting the `params.slotModel &&` gate in
// coordinator_subagents.go's refresh block refreshes every sub-agent, this
// dispatch is rebuilt w1 → w2, and the w1 assertion fails.
func TestRunSubAgent_NonSlotDispatchKeepsModel(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "w1", Name: "W1", DefaultMaxTokens: 4096},
			{ID: "w2", Name: "W2", DefaultMaxTokens: 4096},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "w1"}

	parent, err := env.sessions.Create(t.Context(), "non-slot-dispatch")
	require.NoError(t, err)

	var observed SessionAgentCall
	agent := newMockAgent(prov, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = call
		return agentResultWithText("stable non-slot dispatch"), nil
	})
	// The mock's model represents the fast-built child at the PARENT TURN's
	// start (agentic_fetch's shape), not a role-slot build.
	agent.model = Model{ModelCfg: config.SelectedModel{Provider: prov, Model: "w1"}}

	// The worker slot swaps mid-turn; a non-slot dispatch must ignore it.
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "w2"}
	ctx := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})
	resp, err := coord.runSubAgent(ctx, subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-1", ToolCallID: "call-1",
		Prompt: "go", SessionTitle: "Non-slot dispatch", // slotModel deliberately false
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "w1", observed.SmartModel.ModelCfg.Model,
		"a non-slot sub-agent keeps its parent-turn-start model; no dispatch-time refresh")
}

// TestSubAgentDispatchRefreshesWorkerSlot is D3: refreshSubAgentSlotModel
// rebuilds the sub-agent model from the live worker slot only when the slot
// actually moved, and leaves the model alone when nothing is configured.
// Revert-check: dropping the dispatch-time refresh (or gutting
// refreshSubAgentSlotModel to a no-op) makes the first assertion fail with
// ok=false; dropping the slot-match short-circuit makes the second fail
// with ok=true.
func TestSubAgentDispatchRefreshesWorkerSlot(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	prov := "role-prov"
	coord := newTestCoordinator(t, env, prov, config.ProviderConfig{
		ID:   prov,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "m-old-worker", Name: "M Old Worker", DefaultMaxTokens: 4096},
			{ID: "m-new-worker", Name: "M New Worker", DefaultMaxTokens: 4096},
		},
	})
	cfg := coord.cfg.Config()
	oldSlot := config.SelectedModel{Provider: prov, Model: "m-old-worker"}
	cfg.Models[config.SelectedModelTypeWorker] = oldSlot

	stale, _, err := coord.buildModelsFromCfg(t.Context(), cfg, oldSlot, oldSlot, true)
	require.NoError(t, err)

	// The operator swaps the worker slot mid-turn; ctx carries no CallOptions,
	// so workerSubAgentActiveForCall falls through to workerSubAgentActive:
	// activeRole "" is smart-active and the worker slot is configured, so the
	// worker slot is the refresh source.
	cfg.Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: prov, Model: "m-new-worker"}
	refreshed, ok := coord.refreshSubAgentSlotModel(context.Background(), stale)
	require.True(t, ok, "a moved slot must rebuild the dispatch model")
	require.Equal(t, "m-new-worker", refreshed.ModelCfg.Model)

	// A model already matching the live slot comes back unchanged.
	_, ok = coord.refreshSubAgentSlotModel(context.Background(), refreshed)
	require.False(t, ok, "a matching model needs no rebuild")

	// No slot configured at all: nothing to refresh. With the worker slot
	// gone the coordinator falls back to the smart slot, so that must go
	// too — otherwise the smart slot (still the old id) would rebuild.
	delete(cfg.Models, config.SelectedModelTypeWorker)
	delete(cfg.Models, config.SelectedModelTypeSmart)
	_, ok = coord.refreshSubAgentSlotModel(context.Background(), refreshed)
	require.False(t, ok, "an unconfigured slot cannot refresh")
}
