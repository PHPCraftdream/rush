package agent

// R6-3 regression tests (P1, independent security review round 6): R5-1's
// fail-closed guard (scopedCallToolsRequired) only tripped for
// CallOptions.FolderScope/DiskProvider. pinCallTools ALSO consumes
// DisableSubAgents and ModelRole while building the per-call toolset
// (applyCallDisableSubAgents, workerSubAgentActiveForCall ->
// buildToolsAgentConfigForCall), but a build failure for a call carrying
// ONLY one of those two options still represented itself as a bare nil
// slice with no error — every caller (the three ordinary model-resolution
// call sites AND RunSessionAgentCall's durable-replay tool-rebind block)
// then fell back to the shared, unscoped toolset. For a durable
// `--agents single` row in particular, that meant the restarted turn could
// silently REGAIN the `agent`/`agentic_fetch` delegation tools it was
// declared not to have, precisely on a per-call tool-build failure.
//
// The fix widens scopedCallToolsRequired/scopedCallOptionsRequireDistinctTools
// to also cover DisableSubAgents and any non-empty ModelRole, and moves the
// fail-closed decision INSIDE pinCallTools itself (new signature:
// ([]fantasy.AgentTool, error)) so every caller gets a single, unambiguous
// answer instead of having to remember to call scopedCallToolsRequired(ctx)
// on the side.
//
// Each scenario below drives the REAL execution path end to end (never just
// the trigger predicate in isolation — R5-3's own round-trip test made
// exactly that mistake and could not catch this class of bug) and proves
// TWO things together:
//   - the call returns an error wrapping ErrScopedCallToolsUnavailable;
//   - the mock currentAgent's Run is NEVER invoked — no model request is
//     ever sent with the shared, unscoped/undisabled toolset.
//
// Two distinct injected failures are used, matching pinCallTools' two
// internal failure branches (mirroring R5-1's own test file):
//   - "ready-gate failure" (armReadyGateSeam, defined in
//     r5_1_pincalltools_failclosed_test.go): buildTools succeeds, but
//     draining the ready gate afterwards reports an error. This is used
//     for every DisableSubAgents=true/ModelRole="" scenario because
//     applyCallDisableSubAgents strips BOTH `agent` and `agentic_fetch`
//     from AllowedTools before buildTools ever tries to construct them, so
//     breakTaskAgent's "task agent not configured" injection (buildTools'
//     OTHER failure branch) has nothing left to break in that case.
//   - "buildTools failure" (breakTaskAgent): used for ModelRole-only
//     scenarios (DisableSubAgents=false, so `agent` stays in
//     AllowedTools) and for the one DisableSubAgents=true+bypass scenario
//     below, where ModelRole=Smart with a Worker model configured makes
//     applyCallDisableSubAgents's bypass keep the `agent` tool present
//     specifically so buildTools' OWN error branch (not just the ready
//     gate) is proven to fail closed for a DisableSubAgents=true call too.
//
// REVERT-CHECK PROCEDURE:
//  1. In coordinator_models.go, narrow scopedCallOptionsRequireDistinctTools
//     back to `opts.FolderScope != nil || opts.DiskProvider != nil` (drop
//     the DisableSubAgents/ModelRole disjuncts).
//  2. Run: go test ./internal/agent/ -run 'TestR6_3' -v
//  3. FAIL: every test below reports the mock agent's Run was invoked (the
//     call fell back to the shared toolset) instead of returning
//     ErrScopedCallToolsUnavailable.
//  4. Restore the widened predicate and the tests PASS again.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ---- durable replay path: RebuildSessionAgentCall + RunSessionAgentCall ----

// TestR6_3_RunSessionAgentCall_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed
// is the review's headline required regression: a durable
// DisableSubAgents=true row with NO folder scope, replayed through the REAL
// RebuildSessionAgentCall -> RunSessionAgentCall path, whose per-call
// toolset build fails, must refuse the row instead of restarting the turn
// on the shared toolset (which still carries agent/agentic_fetch).
func TestR6_3_RunSessionAgentCall_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	originalCall := SessionAgentCall{
		SessionID: "r6-3-durable-disablesubagents-readygate",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			DisableSubAgents: true,
		},
	}

	// Real JSON round trip through the durable-queue boundary, matching
	// R5-3's own pattern.
	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Nil(t, decoded.FolderScopeSpec, "this row must carry no folder scope")
	require.NotNil(t, decoded.CallOptionsSpec)
	assert.True(t, decoded.CallOptionsSpec.DisableSubAgents)

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.CallOptions)
	require.True(t, rebuilt.CallOptions.DisableSubAgents)
	require.Nil(t, rebuilt.CallOptions.FolderScope,
		"proving the fix isn't merely piggybacking on the FolderScope trigger")

	armReadyGateSeam(t, coord)

	_, runErr := coord.RunSessionAgentCall(t.Context(), rebuilt)
	require.Error(t, runErr,
		"a durable DisableSubAgents-only call whose ready-gate drain fails must refuse the row, not restart on the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_RunSessionAgentCall_DisableSubAgentsTrue_SmartWorkerBypass_BuildToolsFailure_FailsClosed
// proves buildTools' OWN error branch (not merely the ready-gate branch)
// also fails closed for a DisableSubAgents=true durable call. DisableSubAgents
// alone strips BOTH `agent`/`agentic_fetch` before buildTools ever tries to
// construct them (see applyCallDisableSubAgents), leaving nothing for
// breakTaskAgent to break; ModelRole=Smart with a Worker model configured
// triggers applyCallDisableSubAgents's documented bypass, which keeps the
// `agent` tool in AllowedTools (only agentic_fetch stays stripped) so its
// construction — and breakTaskAgent's injected failure — is actually
// exercised.
func TestR6_3_RunSessionAgentCall_DisableSubAgentsTrue_SmartWorkerBypass_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true) // worker configured, for the bypass
	coord.currentAgent = neverRunAgent(t)

	originalCall := SessionAgentCall{
		SessionID: "r6-3-durable-disablesubagents-bypass-buildtools",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			DisableSubAgents: true,
			ModelRole:        config.SelectedModelTypeSmart,
		},
	}

	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Nil(t, decoded.FolderScopeSpec)

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.CallOptions)
	require.True(t, rebuilt.CallOptions.DisableSubAgents)
	require.Equal(t, config.SelectedModelTypeSmart, rebuilt.CallOptions.ModelRole)
	require.Nil(t, rebuilt.CallOptions.FolderScope)

	breakTaskAgent(coord)

	_, runErr := coord.RunSessionAgentCall(t.Context(), rebuilt)
	require.Error(t, runErr,
		"a durable DisableSubAgents=true call whose per-call toolset buildTools call fails must refuse the row, not restart on the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_RunSessionAgentCall_ModelRoleOnly_BuildToolsFailure_FailsClosed
// covers the OTHER half of the widened trigger: a durable row with a
// tool-shaping ModelRole but DisableSubAgents=false and no folder scope.
// DisableSubAgents=false means `agent` stays in AllowedTools, so
// breakTaskAgent's injected failure is exercised directly.
func TestR6_3_RunSessionAgentCall_ModelRoleOnly_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true) // worker configured
	coord.currentAgent = neverRunAgent(t)

	originalCall := SessionAgentCall{
		SessionID: "r6-3-durable-modelrole-buildtools",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			ModelRole: config.SelectedModelTypeFast,
		},
	}

	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "fast", decoded.CallOptionsSpec.ModelRole)

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.CallOptions)
	require.False(t, rebuilt.CallOptions.DisableSubAgents)
	require.Nil(t, rebuilt.CallOptions.FolderScope)
	require.Equal(t, config.SelectedModelTypeFast, rebuilt.CallOptions.ModelRole)

	breakTaskAgent(coord)

	_, runErr := coord.RunSessionAgentCall(t.Context(), rebuilt)
	require.Error(t, runErr,
		"a durable ModelRole-only call whose per-call toolset build fails must refuse the row, not restart on the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// ---- ordinary (non-durable) calls: the three model-resolution call sites ----

// TestR6_3_ScopedRun_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed
// covers coordinator.Run -> resolveSessionModelsInternal, the first of the
// three ordinary call sites.
func TestR6_3_ScopedRun_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r6-3-run-disablesubagents")
	require.NoError(t, err)

	ctx := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true})

	armReadyGateSeam(t, coord)

	_, runErr := coord.Run(ctx, sess.ID, "do something")
	require.Error(t, runErr,
		"a DisableSubAgents-only call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_ScopedRun_ModelRoleOnly_BuildToolsFailure_FailsClosed is the
// ModelRole-only counterpart at the same call site.
func TestR6_3_ScopedRun_ModelRoleOnly_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true) // worker configured
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r6-3-run-modelrole")
	require.NoError(t, err)

	ctx := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeFast})

	breakTaskAgent(coord)

	_, runErr := coord.Run(ctx, sess.ID, "do something")
	require.Error(t, runErr,
		"a ModelRole-only call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_ScopedRunWithOverrides_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed
// covers coordinator.RunWithOverrides -> applyModelOverrides, the second of
// the three ordinary call sites (R5-1's own suite never exercised this one).
func TestR6_3_ScopedRunWithOverrides_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r6-3-runoverrides-disablesubagents")
	require.NoError(t, err)

	ctx := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true})

	armReadyGateSeam(t, coord)

	smart := &ModelOverride{Provider: "smart-provider", Model: "smart-model"}
	fast := &ModelOverride{Provider: "fast-provider", Model: "fast-model"}
	_, runErr := coord.RunWithOverrides(ctx, sess.ID, "do something", smart, fast)
	require.Error(t, runErr,
		"a DisableSubAgents-only RunWithOverrides call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_ScopedRunWithCredentials_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed
// covers coordinator.RunWithCredentials -> resolveCredentialsModels, the
// third of the three ordinary call sites.
func TestR6_3_ScopedRunWithCredentials_DisableSubAgentsOnly_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r6-3-creds-disablesubagents")
	require.NoError(t, err)

	ctx := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true})

	armReadyGateSeam(t, coord)

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "cred-provider",
			Type:     ProviderTypeOpenAI,
			APIKey:   "sk-r6-3-test",
			Models:   []CredentialModel{{ID: "cred-model", ContextWindow: 200000, DefaultMaxTokens: 4096}},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "cred-provider", Model: "cred-model"},
			RoleFast:  {Provider: "cred-provider", Model: "cred-model"},
		},
	}

	_, runErr := coord.RunWithCredentials(ctx, sess.ID, "do something", creds)
	require.Error(t, runErr,
		"a DisableSubAgents-only credentialed call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestR6_3_UnscopedRun_NoToolShapingOptions_StillFallsBack is the negative
// control: a call whose CallOptions carries neither
// FolderScope/DiskProvider/DisableSubAgents nor a ModelRole (e.g. only
// MaxCost/AllowPeakHours) must keep today's legacy fallback behavior when
// its per-call toolset build fails — this fix must not turn every
// CallOptions-carrying call into a hard failure, only the ones that
// actually change tool composition.
func TestR6_3_UnscopedRun_NoToolShapingOptions_StillFallsBack(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	sess, err := coord.sessions.Create(t.Context(), "r6-3-unscoped-other-options")
	require.NoError(t, err)

	ctx := WithCallOptions(t.Context(), &CallOptions{MaxCost: 5, AllowPeakHours: true})

	breakTaskAgent(coord)

	pinned, resolveErr := coord.resolveSessionModels(ctx, sess.ID)
	require.NoError(t, resolveErr,
		"a call with no tool-shaping options must still fall back, not error, on a per-call toolset build failure")
	assert.Nil(t, pinned.tools)
}
