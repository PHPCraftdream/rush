package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/stretchr/testify/require"
)

// TestPolicyLeafConstructionBaseline freezes representative pre-refactor
// names, order, schemas and wrapper stacks without executing any tools.
func TestPolicyLeafConstructionBaseline(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	// A non-nil ledger enables async decoration; no execution is launched.
	coord.asyncJobs = &workLedger{}
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
		"bash":         "9b6b87744288dfd425b2fad09dbc45b0cb9fd2ebb0ceb2de6d6f80177fb72b67",
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
				wantStack := []string{"stall"}
				if !child {
					wantStack = append(wantStack, "hooks")
				}
				wantStack = append(wantStack, "restricted")
				if tool.Info().Name == "bash" || tool.Info().Name == "run_command" {
					wantStack = append(wantStack, "async")
				}
				wantStack = append(wantStack, "tool")
				require.Equal(t, wantStack, policyLeafBaselineStack(tool), "wrapper stack: %s", tool.Info().Name)

				info := tool.Info()
				// Descriptions may contain platform and temporary-directory data.
				// Freeze the complete remaining ToolInfo, including the schema.
				info.Description = ""
				data, err := json.Marshal(info)
				require.NoError(t, err)
				require.Equal(t, wantSchemas[info.Name], fmt.Sprintf("%x", sha256.Sum256(data)), "schema golden: %s", info.Name)
			}
			require.Equal(t, wantNames, names, "pinned filtered order")

			a := NewSessionAgent(SessionAgentOptions{Tools: built, RestrictedRuns: restrictedRunAuthorizer(env.permissions)}).(*sessionAgent)
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

func policyLeafBaselineStack(tool fantasy.AgentTool) []string {
	switch tool := tool.(type) {
	case *loggedTool:
		return append([]string{"logging"}, policyLeafBaselineStack(tool.inner)...)
	case *stallDetacherTool:
		return append([]string{"stall"}, policyLeafBaselineStack(tool.inner)...)
	case *hookedTool:
		return append([]string{"hooks"}, policyLeafBaselineStack(tool.inner)...)
	case restrictedTool:
		return append([]string{"restricted"}, policyLeafBaselineStack(tool.AgentTool)...)
	case *asyncTool:
		return append([]string{"async"}, policyLeafBaselineStack(tool.inner)...)
	default:
		return []string{"tool"}
	}
}
