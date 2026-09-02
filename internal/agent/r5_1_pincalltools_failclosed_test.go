package agent

// R5-1 regression tests (P0, independent security review round 5): a
// scoped/provider-backed call (CallOptions.FolderScope or
// CallOptions.DiskProvider set) must FAIL, never silently widen to the
// shared toolset, when pinCallTools cannot build its own per-call toolset.
//
// Pre-fix, resolveSessionModelsInternal/applyModelOverrides/
// resolveCredentialsModels stored pinCallTools' nil result with no error
// signal. At turn start a nil pinned toolset falls back to
// a.tools.Copy() — the process-wide SHARED toolset (agent_turn.go) — which
// for a scoped/provider-backed call still carries the legacy
// view/write/edit/glob/grep/ls tools, MCP tools, and usually sub-agent
// tools: exactly what the per-call scoped/provider-backed build exists to
// strip. Since an SDK run auto-approves every tool call, nothing else
// would catch the widened toolset before the model could use it.
//
// Each scenario below is exercised through the actual SDK-shaped entry
// points (coordinator.Run / coordinator.RunWithCredentials — what
// sdk.Client.Run / sdk.Client.RunWithCredentials funnel into via
// app.ExecuteRun) and proves TWO things together:
//   - the call returns wrapping ErrScopedCallToolsUnavailable;
//   - the mock currentAgent's Run is NEVER invoked — i.e. no model request
//     is ever sent, because the failure happens before runInternal/buildCall.
//
// Two distinct injected failures, matching pinCallTools' two internal
// failure branches:
//   - "buildTools failure": the coder's per-call toolset build itself
//     returns an error (here: the embedded `agent` tool's sub-agent build
//     fails because the task agent is unconfigured).
//   - "ready-gate failure": buildTools succeeds, but draining the ready
//     gate afterwards reports an error from an async agent-build task.
//     pinCallToolsReadyGateSeam (coordinator_models.go) injects this
//     deterministically at the exact window pinCallTools' OWN readyWg.Wait
//     call is about to observe: Run/RunWithCredentials' own PRE-EXISTING,
//     unrelated top-level readyWg.Wait() gate runs BEFORE
//     resolveSessionModels/resolveCredentialsModels ever executes, so a
//     failure registered before the call starts would be caught by that
//     outer gate regardless of whether this fix exists at all — a
//     worthless regression test (passes identically with the fix reverted).
//     The seam lets the injected task land strictly AFTER the outer gate
//     already passed, so only pinCallTools' own internal Wait() call (the
//     one this fix's callers must react to) observes it.
//
// REVERT-CHECK PROCEDURE (all four Test...FailsClosed tests below):
//  1. In coordinator_models.go, remove the
//     `if resolved.tools == nil && scopedCallToolsRequired(ctx) { return
//     nil, ErrScopedCallToolsUnavailable }` block from
//     resolveSessionModelsInternal (and, for completeness, the identical
//     block in applyModelOverrides), and the matching block in
//     credentials.go's resolveCredentialsModels.
//  2. Run: go test ./internal/agent/ -run
//     'TestScopedRun_BuildToolsFailure_FailsClosed|TestScopedRun_ReadyGateFailure_FailsClosed|TestScopedRunWithCredentials_BuildToolsFailure_FailsClosed|TestScopedRunWithCredentials_ReadyGateFailure_FailsClosed'
//     -v
//  3. FAIL: every one of the four reports the mock agent's Run was invoked
//     (i.e. the call fell back to the shared toolset and actually ran a
//     turn) instead of returning ErrScopedCallToolsUnavailable.
//  4. Restore the blocks and PASS.

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// neverRunAgent is a mockSessionAgent whose Run/RunWithReservedOwnership
// fail the test immediately if invoked — the direct, unambiguous proof that
// "no model request was ever sent" for a call that must fail closed before
// reaching the turn.
func neverRunAgent(t *testing.T) *mockSessionAgent {
	t.Helper()
	return newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatal("the model must never be called: the scoped call should have failed closed before reaching the turn")
			return nil, nil
		})
}

// errSimulatedBuildToolsFailure/errSimulatedReadyGateFailure are the
// sentinels each scenario injects, so assertions can prove the RIGHT
// mechanism was exercised (via errors.Is / message match) rather than just
// "some error came back".
var (
	errSimulatedReadyGateFailure = errors.New("r5-1 test: simulated concurrent agent-build failure on the ready gate")
)

// breakTaskAgent removes the task agent from cfg so the coder's embedded
// `agent` sub-tool (present in AllowedTools by default) fails to build,
// making buildTools itself return a non-nil error — pinCallTools' FIRST
// failure branch.
func breakTaskAgent(coord *coordinator) {
	delete(coord.cfg.Config().Agents, config.AgentTask)
}

// armReadyGateSeam registers pinCallToolsReadyGateSeam to fail THIS call's
// pinCallTools' own readyWg.Wait() drain, deterministically and without
// touching the coordinator's top-level gate before the call even starts.
// Returns a cleanup that must run (deferred) so the seam does not leak into
// other tests in the same package/process.
func armReadyGateSeam(t *testing.T, coord *coordinator) {
	t.Helper()
	pinCallToolsReadyGateSeam = func() {
		coord.readyWg.Go(func() error { return errSimulatedReadyGateFailure })
	}
	t.Cleanup(func() { pinCallToolsReadyGateSeam = nil })
}

// TestScopedRun_BuildToolsFailure_FailsClosed covers Client.Run (via
// coordinator.Run): a folder-scoped call whose per-call toolset build fails
// must return an error and must never fall back to the shared toolset / run
// a turn.
func TestScopedRun_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-run-buildtools")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	breakTaskAgent(coord)

	_, runErr := coord.Run(ctx, sess.ID, "do something")
	require.Error(t, runErr, "a scoped call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestScopedRun_ReadyGateFailure_FailsClosed is
// TestScopedRun_BuildToolsFailure_FailsClosed's ready-gate counterpart:
// buildTools succeeds, but the ready-gate drain pinCallTools performs right
// after it reports an error.
func TestScopedRun_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-run-readygate")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	armReadyGateSeam(t, coord)

	_, runErr := coord.Run(ctx, sess.ID, "do something")
	require.Error(t, runErr, "a scoped call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestScopedRunWithCredentials_BuildToolsFailure_FailsClosed is
// TestScopedRun_BuildToolsFailure_FailsClosed's RunWithCredentials
// counterpart (sdk.Client.RunWithCredentials's underlying path).
func TestScopedRunWithCredentials_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-creds-buildtools")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	breakTaskAgent(coord)

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "cred-provider",
			Type:     ProviderTypeOpenAI,
			APIKey:   "sk-r5-1-test",
			Models:   []CredentialModel{{ID: "cred-model", ContextWindow: 200000, DefaultMaxTokens: 4096}},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "cred-provider", Model: "cred-model"},
			RoleFast:  {Provider: "cred-provider", Model: "cred-model"},
		},
	}

	_, runErr := coord.RunWithCredentials(ctx, sess.ID, "do something", creds)
	require.Error(t, runErr, "a scoped credentialed call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestScopedRunWithCredentials_ReadyGateFailure_FailsClosed is
// TestScopedRun_ReadyGateFailure_FailsClosed's RunWithCredentials
// counterpart.
func TestScopedRunWithCredentials_ReadyGateFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coord.currentAgent = neverRunAgent(t)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-creds-readygate")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	armReadyGateSeam(t, coord)

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "cred-provider",
			Type:     ProviderTypeOpenAI,
			APIKey:   "sk-r5-1-test",
			Models:   []CredentialModel{{ID: "cred-model", ContextWindow: 200000, DefaultMaxTokens: 4096}},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "cred-provider", Model: "cred-model"},
			RoleFast:  {Provider: "cred-provider", Model: "cred-model"},
		},
	}

	_, runErr := coord.RunWithCredentials(ctx, sess.ID, "do something", creds)
	require.Error(t, runErr, "a scoped credentialed call must fail closed instead of silently falling back to the shared toolset")
	assert.ErrorIs(t, runErr, ErrScopedCallToolsUnavailable)
}

// TestScopedRun_DiskProviderOnly_BuildToolsFailure_FailsClosed covers the
// OTHER scoping signal (CallOptions.DiskProvider) independently of
// FolderScope, at the resolveSessionModels boundary directly — the exact
// function whose caller-side gap this fix closes — proving the fail-closed
// check does not depend specifically on FolderScope being set.
func TestScopedRun_DiskProviderOnly_BuildToolsFailure_FailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-diskprovider-only")
	require.NoError(t, err)

	fake := newFakeDiskProvider(nil)
	ctx := WithCallOptions(t.Context(), &CallOptions{DiskProvider: fake})

	breakTaskAgent(coord)

	_, resolveErr := coord.resolveSessionModels(ctx, sess.ID)
	require.Error(t, resolveErr)
	assert.ErrorIs(t, resolveErr, ErrScopedCallToolsUnavailable)
}

// TestUnscopedRun_BuildToolsFailure_StillFallsBackToSharedToolset is the
// negative control: an ordinary call with NO FolderScope/DiskProvider must
// keep today's legacy behavior when its per-call toolset build fails —
// nil tools, no error, shared toolset fallback at turn start. This fix is
// scoped narrowly to calls that specifically asked for a
// restricted/redirected toolset; it must not turn an unrelated buildTools
// hiccup on an ordinary call into a hard failure.
func TestUnscopedRun_BuildToolsFailure_StillFallsBackToSharedToolset(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-unscoped-buildtools")
	require.NoError(t, err)

	breakTaskAgent(coord)

	pinned, resolveErr := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, resolveErr, "an unscoped call's buildTools failure must still fall back, not error")
	assert.Nil(t, pinned.tools, "pinCallTools must still report nil so the turn falls back to the shared toolset")
}

// TestUnscopedRun_ReadyGateFailure_StillFallsBackToSharedToolset mirrors the
// above for the ready-gate branch.
func TestUnscopedRun_ReadyGateFailure_StillFallsBackToSharedToolset(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	sess, err := coord.sessions.Create(t.Context(), "r5-1-unscoped-readygate")
	require.NoError(t, err)

	armReadyGateSeam(t, coord)

	pinned, resolveErr := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, resolveErr, "an unscoped call's ready-gate failure must still fall back, not error")
	assert.Nil(t, pinned.tools, "pinCallTools must still report nil so the turn falls back to the shared toolset")
}

// NOTE: pinCallToolsReadyGateSeam is package-scoped mutable state (like
// runTurnToolsSnapshotSeam). None of the tests in this file call
// t.Parallel() — armReadyGateSeam's t.Cleanup clears the seam after each
// test, but two tests arming it concurrently would race each other's
// assignment.
