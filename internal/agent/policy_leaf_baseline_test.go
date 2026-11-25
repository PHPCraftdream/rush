package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPolicyLeafConstructionBaseline freezes representative pre-refactor
// names, order, schemas and wrapper stacks without executing any tools.
func TestPolicyLeafConstructionBaseline(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	cfg := coord.cfg.Config()
	cfg.Hooks = map[string][]config.HookConfig{
		hooks.EventPreToolUse: {{Command: "exit 0"}},
	}
	require.NoError(t, cfg.ValidateHooks())
	allowed := []string{"view", "run_command", "fetch", "bash", "ask_question"}
	agentCfg := config.Agent{AllowedTools: allowed, AllowedMCP: map[string][]string{}}
	wantNames := []string{"ask_question", "bash", "fetch", "run_command", "view"}
	wantSchemas := map[string]string{
		"ask_question": "50543d487c7b4a54383e1926d0c5b29bc04322e8c7432e56165a491cbf767a83",
		"bash":         "e87ae395ca1867258916a3f22d2aaa68fd7e4d15d9e9a4a153885903ef2df478",
		"fetch":        "2511b98c205dd8b40b534035dc7f25c09b7c517ca84d082988a6beddf6aab38a",
		"run_command":  "5842cf359ae8103b62a5cd7fdff4de7a52c5fb3e79c36824b4958e4dc2fa302c",
		"view":         "d55c0dedd688a46ed1c3ea8934f1a6d4bb8fd946f770e03080d6903d2a45d704",
	}

	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%t", child), func(t *testing.T) {
			built, err := coord.buildTools(t.Context(), cfg, agentCfg, child)
			require.NoError(t, err)
			var names []string
			for _, tool := range built {
				names = append(names, tool.Info().Name)
				// The batch world installs no stall-detach or async wrapper:
				// both seams are identity rehomes (batch_rehome_tool_
				// metadata.go), so the retained baseline stack is
				// hooks (top-level only) -> restricted -> tool.
				var wantStack []string
				if !child {
					wantStack = append(wantStack, "hooks")
				}
				wantStack = append(wantStack, "restricted")
				wantStack = append(wantStack, "tool")
				require.Equal(t, wantStack, policyLeafBaselineStack(tool), "wrapper stack: %s", tool.Info().Name)

				info := tool.Info()
				// Descriptions may contain platform and temporary-directory data.
				// Freeze the complete remaining ToolInfo, including the schema.
				info.Description = ""
				data, err := json.Marshal(info)
				require.NoError(t, err)
				assert.Equal(t, wantSchemas[info.Name], fmt.Sprintf("%x", sha256.Sum256(data)), "schema golden: %s", info.Name)
			}
			require.Equal(t, wantNames, names, "pinned filtered order")

			a := NewSessionAgent(SessionAgentOptions{Tools: built, RestrictedRuns: toolwrap.RestrictedRunAuthorizer(env.permissions)}).(*sessionAgent)
			for i, tool := range a.tools.Copy() {
				require.Equal(t, append([]string{"logging"}, policyLeafBaselineStack(built[i])...), policyLeafBaselineStack(tool), "constructor stack")
			}
			a.SetTools(built)
			for i, tool := range a.tools.Copy() {
				require.Equal(t, append([]string{"logging"}, policyLeafBaselineStack(built[i])...), policyLeafBaselineStack(tool), "SetTools stack")
			}
		})
	}
}

// toolwrapPkgPath pins the exact package of the known wrapper types so the
// name match below can never collide with same-named types elsewhere.
const toolwrapPkgPath = "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

// policyLeafBaselineStack derives the wrapper stack independently of the
// registry entry via scoped, safe reflection (no unsafe, no .Interface on
// unexported fields): each toolwrap wrapper type is recognized by its exact
// PkgPath/Name and recursed through its single inner AgentTool field using
// only reflect.Value operations (FieldByName/Kind/Elem are all legal on
// unexported fields). Unknown types are leaves, so a new wrapper type fails
// the baseline loudly instead of hiding.
func policyLeafBaselineStack(tool fantasy.AgentTool) []string {
	return policyLeafBaselineStackValue(reflect.ValueOf(tool))
}

func policyLeafBaselineStackValue(v reflect.Value) []string {
	for {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return []string{"tool"}
			}
			v = v.Elem()
			continue
		case reflect.Struct:
			t := v.Type()
			var field, layer string
			if t.PkgPath() == toolwrapPkgPath {
				switch t.Name() {
				case "loggedTool":
					field, layer = "inner", "logging"
				case "hookedTool":
					field, layer = "inner", "hooks"
				case "restrictedTool":
					field, layer = "AgentTool", "restricted"
				case "roleGuardTool":
					field, layer = "AgentTool", "role"
				}
			}
			if field == "" {
				return []string{"tool"}
			}
			return append([]string{layer}, policyLeafBaselineStackValue(v.FieldByName(field))...)
		default:
			return []string{"tool"}
		}
	}
}
