package agent

// R4-1/R4-3 round-trip unit tests: the durable row's persisted policy
// spec must survive the JSON boundary and RebuildSessionAgentCall must
// recompile it into the rebuilt call's restricted-run matcher.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// TestRebuildSessionAgentCall_RestoresRunAllowlistFromSpec pins the
// durable-restart half of R4-1: a SessionAgentCallData row carrying a
// RunAllowlistSpec must be rebuilt into a call whose compiled
// RunAllowlist is restricted and whose spec round-trips unchanged.
func TestRebuildSessionAgentCall_RestoresRunAllowlistFromSpec(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	data := session.SessionAgentCallData{
		SessionID: "spec-rebuild-probe",
		Prompt:    "hello",
		RunAllowlistSpec: &session.RunAllowlistSpec{
			Restrict:   true,
			AllowTools: []string{"view"},
			AllowBash:  []string{"exact:probe_allowed_cmd", "probe_prefix_*"},
		},
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)

	// (1) The rebuilt call carries a compiled, RESTRICTED matcher.
	require.NotNil(t, call.RunAllowlist,
		"a rebuilt call whose row carries a spec must have a compiled RunAllowlist")
	require.True(t, call.RunAllowlist.IsRestricted(),
		"the recompiled matcher must be restricted (Restrict: true in the spec)")

	// (2) The spec round-trips exactly through the rebuild.
	require.NotNil(t, call.RunAllowlistSpec)
	require.True(t, call.RunAllowlistSpec.Restrict)
	require.Equal(t, []string{"view"}, call.RunAllowlistSpec.AllowTools)
	require.Equal(t, []string{"exact:probe_allowed_cmd", "probe_prefix_*"}, call.RunAllowlistSpec.AllowBash)

	// (3) Drive the recompiled matcher through a REAL permission service:
	// armed as the per-call entry, an allowlisted plain tool must be
	// granted and a non-listed one denied. The bash-command half is NOT
	// asserted here: allowsRequest's command-provider interface is
	// unexported in permission, but tools.BashPermissionsParams (also
	// imported by the app-level tests) satisfies it, so the bash half is
	// covered through the same exported surface below.
	svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
	svc.AutoApproveSession("probe-session")
	mgr := svc.(permission.SessionRunAllowlistManager)
	mgr.SetSessionRunAllowlistForCall("probe-session", *call.RunAllowlist, call.LogicalCallID)

	allowed, err := svc.Request(context.Background(), permission.CreatePermissionRequest{
		SessionID:  "probe-session",
		ToolCallID: "r1",
		ToolName:   "view",
		Action:     "read",
		Params:     map[string]any{"path": "README.md"},
	})
	require.NoError(t, err)
	require.True(t, allowed, "an allowlisted plain tool (view) must be GRANTED under the recompiled per-call policy")

	allowed, err = svc.Request(context.Background(), permission.CreatePermissionRequest{
		SessionID:  "probe-session",
		ToolCallID: "r2",
		ToolName:   "write",
		Action:     "write",
		Params:     map[string]any{"path": "README.md"},
	})
	require.NoError(t, err)
	require.False(t, allowed, "a non-allowlisted plain tool (write) must be DENIED under the recompiled per-call policy")

	// Bash half via the exported command-carrying params type: the
	// exact-pattern command is allowed, the foreign one denied.
	allowed, err = svc.Request(context.Background(), permission.CreatePermissionRequest{
		SessionID:  "probe-session",
		ToolCallID: "r3",
		ToolName:   "bash",
		Action:     "execute",
		Params:     tools.BashPermissionsParams{Command: "probe_allowed_cmd", Description: "policy probe"},
	})
	require.NoError(t, err)
	require.True(t, allowed, "an AllowBash-exact command must be GRANTED under the recompiled per-call policy")

	allowed, err = svc.Request(context.Background(), permission.CreatePermissionRequest{
		SessionID:  "probe-session",
		ToolCallID: "r4",
		ToolName:   "bash",
		Action:     "execute",
		Params:     tools.BashPermissionsParams{Command: "probe_forbidden_cmd", Description: "policy probe"},
	})
	require.NoError(t, err)
	require.False(t, allowed, "a command matching no AllowBash pattern must be DENIED under the recompiled per-call policy")
}

// TestSessionAgentCallDataSpecJSONRoundTrip pins the durable JSON
// boundary: a non-nil spec survives marshal/unmarshal identically, and a
// nil spec serializes WITHOUT any run_allowlist_spec key and stays nil —
// the "row has no policy" (legacy) case must remain distinguishable from
// "policy present but inert" (Restrict: false).
func TestSessionAgentCallDataSpecJSONRoundTrip(t *testing.T) {
	call := SessionAgentCall{
		SessionID: "json-roundtrip-probe",
		Prompt:    "hello",
		RunAllowlistSpec: &permission.RunAllowlistSpec{
			Restrict:   true,
			AllowTools: []string{"view", "edit:write"},
			AllowBash:  []string{"exact:git status", "glob:ls *", "regex:^probe_.*"},
		},
	}

	data := ToSessionAgentCallData(call)
	raw, err := json.Marshal(data)
	require.NoError(t, err)

	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.RunAllowlistSpec, "a present spec must survive the JSON boundary")
	require.True(t, decoded.RunAllowlistSpec.Restrict)
	require.Equal(t, []string{"view", "edit:write"}, decoded.RunAllowlistSpec.AllowTools)
	require.Equal(t, []string{"exact:git status", "glob:ls *", "regex:^probe_.*"}, decoded.RunAllowlistSpec.AllowBash)

	roundTripped, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.NotNil(t, roundTripped.RunAllowlistSpec)
	require.True(t, roundTripped.RunAllowlistSpec.Restrict)
	require.Equal(t, call.RunAllowlistSpec.AllowTools, roundTripped.RunAllowlistSpec.AllowTools)
	require.Equal(t, call.RunAllowlistSpec.AllowBash, roundTripped.RunAllowlistSpec.AllowBash)

	// nil spec: SessionAgentCallData has no json tags at all (every field,
	// including this one, marshals under its exact Go name with no
	// omitempty — verified against the other pointer fields in this same
	// struct, e.g. SmartModel/FastModel), so a nil spec serializes as
	// `"RunAllowlistSpec":null`, not as an absent key. What must actually
	// hold — and what the rest of this test verifies — is that this null
	// unmarshals back to a nil Go pointer, not to an inert
	// zero-value *RunAllowlistSpec{}: that is what keeps "no policy
	// declared" distinguishable from "policy present but inert"
	// (Restrict: false), not JSON-level key omission.
	nilCall := SessionAgentCall{SessionID: "json-roundtrip-nil", Prompt: "hello"}
	nilData := ToSessionAgentCallData(nilCall)
	nilRaw, err := json.Marshal(nilData)
	require.NoError(t, err)
	require.Contains(t, string(nilRaw), `"RunAllowlistSpec":null`,
		"a nil spec must serialize as a JSON null (consistent with this struct's other untagged pointer fields), not be silently dropped or defaulted to an inert struct")

	var nilDecoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(nilRaw, &nilDecoded))
	require.Nil(t, nilDecoded.RunAllowlistSpec,
		"a legacy row without a spec must decode to a nil spec (never to an inert zero-value spec)")
	nilRoundTripped, err := FromSessionAgentCallData(nilDecoded)
	require.NoError(t, err)
	require.Nil(t, nilRoundTripped.RunAllowlistSpec)
}
