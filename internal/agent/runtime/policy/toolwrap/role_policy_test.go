package toolwrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

// leafProbeTool is the moved self-contained probe from the root
// policy_leaf_test.go fixtures: it records the last dispatch and can carry a
// dynamic restricted-run action.
type leafProbeTool struct {
	name   string
	action string
	called bool
	call   fantasy.ToolCall
}

func (p *leafProbeTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: p.name} }

func (p *leafProbeTool) ProviderOptions() fantasy.ProviderOptions { return fantasy.ProviderOptions{} }

func (p *leafProbeTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (p *leafProbeTool) RestrictedRunAction() string { return p.action }

func (p *leafProbeTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.called = true
	p.call = call
	return fantasy.NewTextResponse("read result"), nil
}

func TestPolicyLeafRewriteDenyRestrictionOnce(t *testing.T) {
	for _, mode := range []string{"rewrite", "deny", "restricted"} {
		t.Run(mode, func(t *testing.T) {
			probe := &leafProbeTool{name: "bash"}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowBash: []string{"exact:echo allowed"}})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			// Count hook invocations independently of the response metadata.
			counter := filepath.ToSlash(filepath.Join(t.TempDir(), "hooks"))
			cmd := `echo '{"updated_input":{"command":"echo allowed"}}'`
			switch mode {
			case "deny":
				cmd = `echo '{"decision":"deny","reason":"blocked"}'`
			case "restricted":
				cmd = "exit 0"
			}
			cmd = fmt.Sprintf("printf x >> '%s'; %s", counter, cmd)
			authorizer := &leafCountingAuthorizer{RestrictedRunAuthorizer: svc.(permission.RestrictedRunAuthorizer)}
			wrapped := DecoratePolicyTools([]fantasy.AgentTool{probe}, authorizer, newRunner(t, cmd), false)
			built := NewPolicyToolBuild(wrapped)
			leaf, err := built.Leaves.Resolve("bash")
			require.NoError(t, err)
			require.NotNil(t, leaf)
			entryLayers, hasLeaf := built.Leaves.leaf("bash")
			require.True(t, hasLeaf)
			// Stored layer compare, checked once; the exact independent stack
			// assertions (leaf and model, via private type switch) run in the
			// rewrite subtest below.
			require.Equal(t, []string{"hooks", "restricted", "tool"}, entryLayers.Layers)
			ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
			resp, err := leaf.Run(ctx, fantasy.ToolCall{ID: "call", Name: "bash", Input: `{"command":"echo denied"}`})
			require.NoError(t, err)
			count, err := os.ReadFile(counter)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(count), "x"), "hook executes exactly once")
			wantGateCalls := 1
			if mode == "deny" {
				wantGateCalls = 0
			}
			require.Equal(t, wantGateCalls, authorizer.calls, "restricted gate executes exactly once unless hook denies")
			require.Equal(t, mode != "rewrite", resp.IsError)
			require.Equal(t, mode == "rewrite", probe.called)
			if mode == "rewrite" {
				require.JSONEq(t, `{"command":"echo allowed"}`, probe.call.Input)
				// Exact independent stack assertions via private type switch:
				// the stored entry.Layers are not just repeated, the original
				// model stack and the resolved leaf stack are derived from the
				// actual wrapper types and their order.
				require.Len(t, wrapped, 1)
				require.Equal(t, []string{"hooks", "restricted", "tool"}, toolwrapStackNames(wrapped[0]), "original model stack unchanged")
				require.Equal(t, []string{"hooks", "restricted", "tool"}, toolwrapStackNames(leaf), "resolved leaf stack")
				// Session decoration prepends logging on top of the existing
				// wrapped stack, with the real authorizer carried through.
				sessionTools := DecorateSessionTools(wrapped, authorizer)
				require.Len(t, sessionTools, 1)
				require.Equal(t, []string{"logging", "hooks", "restricted", "tool"}, toolwrapStackNames(sessionTools[0]), "session decoration prepends logging")
			}
		})
	}
}

// toolwrapStackNames derives the wrapper stack by exact private type switch,
// so the assertion reflects actual wrapper types and their order.
func toolwrapStackNames(tool fantasy.AgentTool) []string {
	switch t := tool.(type) {
	case *loggedTool:
		return append([]string{"logging"}, toolwrapStackNames(t.inner)...)
	case *hookedTool:
		return append([]string{"hooks"}, toolwrapStackNames(t.inner)...)
	case *restrictedTool:
		return append([]string{"restricted"}, toolwrapStackNames(t.AgentTool)...)
	case restrictedTool:
		return append([]string{"restricted"}, toolwrapStackNames(t.AgentTool)...)
	case *roleGuardTool:
		return append([]string{"role"}, toolwrapStackNames(t.AgentTool)...)
	case roleGuardTool:
		return append([]string{"role"}, toolwrapStackNames(t.AgentTool)...)
	default:
		return []string{"tool"}
	}
}

// TestRoleCapCollectionRetainsMalformedPolicyPoison keeps the removed
// exported MergeRoleToolContracts coverage with the private merge, using a
// reconstructed valid literal role contract.
func TestRoleCapCollectionRetainsMalformedPolicyPoison(t *testing.T) {
	t.Run("merge preserves empty groups and detached intersections", func(t *testing.T) {
		own := []string{"view", "grep"}
		contract := &session.RoleContract{Version: 1, Tools: &own, Inherited: [][]string{{"view"}, {}}}
		require.NoError(t, contract.Validate())

		merged := mergeRoleToolContracts([]*session.RoleContract{contract})
		require.Len(t, merged, 1)
		require.NoError(t, merged[0].Validate())
		require.Equal(t, [][]string{{"view", "grep"}, {"view"}, {}}, merged[0].Inherited)
		require.False(t, roleContractAllowsName(merged[0], "view"), "intersection with the empty inherited cap stays deny-all")
		require.False(t, roleContractAllowsName(merged[0], "bash"))

		// Mutating the merged output must not leak back into the source
		// contract, and an explicit empty inherited cap must survive as a
		// deny-all group.
		require.NotNil(t, merged[0].Inherited[2], "an explicit empty inherited cap must remain deny-all")
		require.Empty(t, merged[0].Inherited[2])
		merged[0].Inherited[0][0] = "bash"
		require.Equal(t, "view", (*contract.Tools)[0], "merge must clone, not alias, the source contract")
		require.Equal(t, "view", contract.Inherited[0][0], "merge must clone, not alias, the source contract")

		bare := &session.RoleContract{Version: session.RoleContractVersion}
		require.NoError(t, bare.Validate())
		require.Nil(t, mergeRoleToolContracts([]*session.RoleContract{bare}))
	})

	t.Run("malformed policy poison", func(t *testing.T) {
		for _, contract := range []*session.RoleContract{nil, {Version: 0}} {
			merged := mergeRoleToolContracts([]*session.RoleContract{contract})
			require.Len(t, merged, 1)
			require.Error(t, merged[0].Validate(), "a malformed cap must not disappear into unrestricted policy")
		}
	})
}

func TestPolicyLeafChildHooksOmittedAndUnknownClosed(t *testing.T) {
	probe := &leafProbeTool{name: "view"}
	wrapped := DecoratePolicyTools([]fantasy.AgentTool{probe}, nil, newRunner(t, `echo '{"decision":"deny"}'`), true)
	built := NewPolicyToolBuild(wrapped)
	leaf, err := built.Leaves.Resolve("view")
	require.NoError(t, err)
	resp, err := leaf.Run(t.Context(), fantasy.ToolCall{Name: "view"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.True(t, probe.called)
	missing, err := built.Leaves.Resolve("bash")
	require.Error(t, err)
	require.Nil(t, missing)
	missing, err = (*PolicyLeafRegistry)(nil).Resolve("view")
	require.Error(t, err)
	require.Nil(t, missing)
}

func TestPolicyLeafDynamicActionPreserved(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			probe := &leafProbeTool{name: "mcp_dynamic_tool", action: "execute"}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			spec := permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"mcp_dynamic_tool:read"}}
			if allowed {
				spec.AllowTools = []string{"mcp_dynamic_tool:execute"}
			}
			allowlist, err := permission.BuildRunAllowlist(spec)
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			built := NewPolicyToolBuild(DecoratePolicyTools([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer), newRunner(t, "exit 0"), false))
			leaf, err := built.Leaves.Resolve(probe.name)
			require.NoError(t, err)
			resp, err := leaf.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "call", Name: probe.name})
			require.NoError(t, err)
			require.Equal(t, allowed, probe.called)
			require.Equal(t, !allowed, resp.IsError)
		})
	}
}

type leafCountingAuthorizer struct {
	permission.RestrictedRunAuthorizer
	calls int
}

func (a *leafCountingAuthorizer) AuthorizeRestrictedRun(opts permission.CreatePermissionRequest) (bool, bool) {
	a.calls++
	return a.RestrictedRunAuthorizer.AuthorizeRestrictedRun(opts)
}
