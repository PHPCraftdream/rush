package agent

// R5-3 (P0 security review) regression test: RebuildSessionAgentCall used
// to construct the rebuilt CallOptions with ONLY FolderScope set, silently
// dropping DisableSubAgents, ModelRole, TimeoutOptionsSet,
// TimeoutExtendsOnProgress and TimeoutHardCap on every durable restart.
// Most seriously, a call originally declared single-agent (--agents
// single) could silently regain its delegation tools (agent,
// agentic_fetch) after a restart. This file proves the fix reconstructs
// ALL of them together and that each one actually takes effect downstream
// — not just that the struct fields got copied.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// TestRebuildSessionAgentCall_RestoresFullCallOptions is the R5-3 required
// regression: round-trip a call combining a FolderScope, DisableSubAgents,
// an explicit all-zero timeout policy (TimeoutOptionsSet=true with zero
// durations — deliberately zero, not "unset", per CallOptions.
// TimeoutOptionsSet's doc comment) and a non-default ModelRole through
// ToSessionAgentCallData -> JSON -> RebuildSessionAgentCall, then prove
// each field survives AND takes effect downstream: the pinned toolset (for
// DisableSubAgents), workerSubAgentActiveForCall (for ModelRole), and the
// stream watchdog policy resolver (for the timeout fields).
func TestRebuildSessionAgentCall_RestoresFullCallOptions(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true) // worker configured

	workDir := t.TempDir()
	scopeSpec := &permission.FolderScopeSpec{
		WorkingDir: workDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: "scoped", Ops: []permission.FileOp{permission.FileOpRead}},
		},
	}

	originalCall := SessionAgentCall{
		SessionID:       "r5-3-full-calloptions-probe",
		Prompt:          "hello",
		FolderScopeSpec: scopeSpec,
		CallOptions: &CallOptions{
			DisableSubAgents: true,
			ModelRole:        config.SelectedModelTypeFast,
			// Deliberate all-zero timeout policy: TimeoutOptionsSet=true
			// with both guarded fields at their zero value.
			TimeoutOptionsSet:        true,
			TimeoutExtendsOnProgress: false,
			TimeoutHardCap:           0,
		},
	}

	// Real JSON round trip through the durable-queue boundary, not just an
	// in-memory struct copy.
	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))

	require.NotNil(t, decoded.CallOptionsSpec, "CallOptionsSpec must survive the JSON boundary")
	assert.True(t, decoded.CallOptionsSpec.DisableSubAgents)
	assert.Equal(t, "fast", decoded.CallOptionsSpec.ModelRole)
	assert.True(t, decoded.CallOptionsSpec.TimeoutOptionsSet)
	assert.False(t, decoded.CallOptionsSpec.TimeoutExtendsOnProgress)
	assert.Zero(t, decoded.CallOptionsSpec.TimeoutHardCap)

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.CallOptions,
		"a rebuilt call whose row carries CallOptionsSpec/FolderScopeSpec must carry CallOptions")

	// (1) DisableSubAgents survived on the struct.
	assert.True(t, rebuilt.CallOptions.DisableSubAgents)
	// (2) ModelRole survived on the struct.
	assert.Equal(t, config.SelectedModelTypeFast, rebuilt.CallOptions.ModelRole)
	// (3) TimeoutOptionsSet/TimeoutExtendsOnProgress/TimeoutHardCap
	// survived on the struct.
	assert.True(t, rebuilt.CallOptions.TimeoutOptionsSet)
	assert.False(t, rebuilt.CallOptions.TimeoutExtendsOnProgress)
	assert.Zero(t, rebuilt.CallOptions.TimeoutHardCap)
	// (4) The folder scope still compiled (T12 must not regress).
	require.NotNil(t, rebuilt.CallOptions.FolderScope)
	inside := filepath.Join(workDir, "scoped", "f.txt")
	require.NoError(t, rebuilt.CallOptions.FolderScope.Check(inside, permission.FileOpRead))

	// --- Downstream effect #1: the pinned TOOLSET must actually lose the
	// delegation tools, exactly like RunSessionAgentCall's tool-rebind
	// block does for a rebuilt call.
	ctx := WithCallOptions(t.Context(), rebuilt.CallOptions)
	cfg := coord.cfg.Config()
	pinnedTools, pinErr := coord.pinCallTools(ctx, cfg)
	require.NoError(t, pinErr, "pinCallTools must succeed for this rebuilt call")
	require.NotNil(t, pinnedTools, "pinCallTools must succeed for this rebuilt call")
	var names []string
	for _, tl := range pinnedTools {
		names = append(names, tl.Info().Name)
	}
	assert.NotContains(t, names, AgentToolName,
		"DisableSubAgents must survive the rebuild and strip the agent tool from the pinned toolset")
	assert.NotContains(t, names, "agentic_fetch",
		"DisableSubAgents must survive the rebuild and strip agentic_fetch from the pinned toolset")

	// --- Downstream effect #2: ModelRole must survive and decide
	// sub-agent role selection (workerSubAgentActiveForCall): "fast" must
	// NOT trigger the smart-preferred-Worker-slot behavior, even with a
	// Worker model configured.
	assert.False(t, coord.workerSubAgentActiveForCall(ctx, cfg),
		"a rebuilt call's non-default ModelRole (fast) must survive and be read back, not silently reset to unset/smart")

	// --- Downstream effect #3: the watchdog must read the DELIBERATE
	// all-zero timeout policy, not fall back to the sessionAgent's shared
	// SetTimeoutOptions fields (R3-6) -- the shared fields are armed with a
	// visibly different policy so a silent revert to them would flip this
	// assertion.
	sa := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)
	sa.SetTimeoutOptions(true, 30*time.Minute)
	extends, hardCap := sa.watchdogTimeoutPolicyForCall(rebuilt.CallOptions)
	assert.False(t, extends,
		"the rebuilt call's deliberate all-zero timeout policy must survive, not inherit the shared extends flag")
	assert.Zero(t, hardCap,
		"the rebuilt call's deliberate all-zero timeout policy must survive, not inherit the shared hard cap")
}

// TestRebuildSessionAgentCall_CallOptionsSpecAloneWithoutFolderScope pins
// the other half of the R5-3 fix: a call with DisableSubAgents/ModelRole/
// timeout policy but NO folder scope must still get rebuiltCallOptions
// populated (the old code only ever allocated rebuiltCallOptions when
// FolderScopeSpec was non-nil), and RunSessionAgentCall's tool-rebind gate
// must trigger for it too (proven here via the same condition
// RunSessionAgentCall checks, since exercising RunSessionAgentCall itself
// requires a live provider).
func TestRebuildSessionAgentCall_CallOptionsSpecAloneWithoutFolderScope(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	originalCall := SessionAgentCall{
		SessionID: "r5-3-calloptions-only-probe",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			DisableSubAgents: true,
		},
	}

	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Nil(t, decoded.FolderScopeSpec, "this call must not carry a folder scope")
	require.NotNil(t, decoded.CallOptionsSpec)

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.CallOptions,
		"a rebuilt call whose row carries ONLY a CallOptionsSpec (no folder scope) must still carry CallOptions")
	assert.True(t, rebuilt.CallOptions.DisableSubAgents)
	assert.Nil(t, rebuilt.CallOptions.FolderScope, "no folder scope was declared, so none must be compiled")

	// The RunSessionAgentCall tool-rebind gate (coordinator_interrupt.go)
	// must trigger for this call even without a FolderScope: DisableSubAgents
	// alone must be enough to attach CallOptions to the turn's context and
	// rebuild the pinned toolset, or the fix regresses back to "silently
	// falls back to the shared unscoped toolset."
	triggersRebind := rebuilt.CallOptions.FolderScope != nil || rebuilt.CallOptions.DisableSubAgents || rebuilt.CallOptions.ModelRole != ""
	require.True(t, triggersRebind,
		"RunSessionAgentCall's tool-rebind gate must trigger for a DisableSubAgents-only rebuilt call")

	ctx := WithCallOptions(t.Context(), rebuilt.CallOptions)
	cfg := coord.cfg.Config()
	pinnedTools, pinErr := coord.pinCallTools(ctx, cfg)
	require.NoError(t, pinErr)
	require.NotNil(t, pinnedTools)
	var names []string
	for _, tl := range pinnedTools {
		names = append(names, tl.Info().Name)
	}
	assert.NotContains(t, names, AgentToolName)
	assert.NotContains(t, names, "agentic_fetch")
}
