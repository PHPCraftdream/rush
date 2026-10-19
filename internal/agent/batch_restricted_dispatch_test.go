package agent

// R12-H1 restricted-run POLICY dispatch coverage for the NATIVE batch
// controls: each control is wrapped by the production restricted-run gate
// (restricted_tool.go wrapToolsWithRestrictedRun -> restrictedTool.Run) with
// a real permission service carrying a recording inner implementation, and
// dispatched with an armed run allowlist. This is dispatch-behaviour
// coverage and lives SEPARATELY from batch_toolset_inventory_test.go, which
// only checks tool NAMES: deleting the native classifications from
// restrictedToolActions fails the allow cases here, and removing the
// restrictedTool gate entirely fails the deny cases here.
//
// Static anchors:
//   - internal/agent/restricted_tool.go restrictedToolActions: the native
//     classifications batch_status/batch_output -> "read", batch_stop ->
//     "delete", batch_run/batch_add -> "delegate".
//   - internal/agent/restricted_tool.go restrictedTool.Run: the central
//     AuthorizeRestrictedTool dispatch and the denial short-circuit.
//   - internal/permission/permission.go RestrictedRunToolAuthorizer /
//     BuildRunAllowlist: the allowlist matching "name:action" entries.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// batchDispatchProbe is the RECORDING inner implementation: it records
// whether the inner tool actually ran behind the restricted gate.
type batchDispatchProbe struct {
	name   string
	called bool
}

func (p *batchDispatchProbe) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: p.name}
}

func (p *batchDispatchProbe) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.called = true
	return fantasy.NewTextResponse("dispatched"), nil
}

// fantasy.AgentTool also requires the provider-options pair.
func (p *batchDispatchProbe) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (p *batchDispatchProbe) SetProviderOptions(fantasy.ProviderOptions) {}

// TestBatchNativeControlsRestrictedRunPolicyDispatch dispatches every native
// batch control through the production restricted-run gate: an allowlist
// entry matching the control's declared action lets the recording inner tool
// run; an omitted control is denied before the inner tool runs.
func TestBatchNativeControlsRestrictedRunPolicyDispatch(t *testing.T) {
	t.Parallel()

	for _, control := range batchInventoryNativeControls {
		action := restrictedToolActions[control]
		require.NotEmpty(t, action,
			"precondition: native control %q must be classified for restricted dispatch", control)
		allow := control + ":" + action

		t.Run(control+" allowed by matching action", func(t *testing.T) {
			t.Parallel()
			probe := &batchDispatchProbe{name: control}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
				Restrict:   true,
				AllowTools: []string{allow},
			})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer))[0]
			resp, err := restricted.Run(
				context.WithValue(t.Context(), tools.SessionIDContextKey, "session"),
				fantasy.ToolCall{ID: "allow-" + control, Name: control},
			)
			require.NoError(t, err)
			require.False(t, resp.IsError, "allowlisted native control %q must dispatch", control)
			require.True(t, probe.called, "the inner tool must run behind the gate on allow")
		})

		t.Run(control+" denied when omitted from the allowlist", func(t *testing.T) {
			t.Parallel()
			probe := &batchDispatchProbe{name: control}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
				Restrict:   true,
				AllowTools: []string{"other_tool"},
			})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer))[0]
			resp, err := restricted.Run(
				context.WithValue(t.Context(), tools.SessionIDContextKey, "session"),
				fantasy.ToolCall{ID: "deny-" + control, Name: control},
			)
			require.NoError(t, err)
			require.True(t, resp.IsError, "omitted native control %q must be refused", control)
			require.False(t, probe.called, "the inner tool must never run behind the gate on deny")
		})

		t.Run(control+" denied when only a mismatched action is allowed", func(t *testing.T) {
			t.Parallel()
			mismatched := control + ":write"
			if action == "write" {
				mismatched = control + ":read"
			}
			probe := &batchDispatchProbe{name: control}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
				Restrict:   true,
				AllowTools: []string{mismatched},
			})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer))[0]
			resp, err := restricted.Run(
				context.WithValue(t.Context(), tools.SessionIDContextKey, "session"),
				fantasy.ToolCall{ID: "action-deny-" + control, Name: control},
			)
			require.NoError(t, err)
			require.True(t, resp.IsError,
				"a mismatched action grant must not authorize %q", control)
			require.False(t, probe.called)
		})
	}
}
