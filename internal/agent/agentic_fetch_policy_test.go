package agent

import (
	"context"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

type fetchCountingAuthorizer struct {
	permission.RestrictedRunAuthorizer
	calls int
}

func (a *fetchCountingAuthorizer) AuthorizeRestrictedTool(opts permission.CreatePermissionRequest) (bool, bool) {
	a.calls++
	return a.RestrictedRunAuthorizer.(permission.RestrictedRunToolAuthorizer).AuthorizeRestrictedTool(opts)
}

func TestPrivateFetchPolicyBindingOmissionLogging(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			records := captureLogs(t)
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			authorizer := &fetchCountingAuthorizer{RestrictedRunAuthorizer: svc.(permission.RestrictedRunAuthorizer)}
			probe := stubTool{name: "view", resp: fantasy.NewTextErrorResponse("fetch failure")}
			child := decoratePolicyTools([]fantasy.AgentTool{probe}, nil, newRunner(t, `echo '{"decision":"deny"}'`), true)
			ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "fetch-session")
			ingressCalls := 0
			var registry *policyLeafRegistry
			if bound {
				ctx = withPrivateFetchIngress(ctx, func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
					ingressCalls++
					expected, err := registry.resolve("view")
					require.NoError(t, err)
					require.Equal(t, expected, leaf, "Private fetch resolves exact construction registry")
					require.Equal(t, "fetch-session", tools.GetSessionFromContext(ctx))
					require.Equal(t, "fetch-call", call.ID)
					return leaf.Run(ctx, call)
				})
			}
			built := buildPrivateFetchTools(ctx, child, authorizer)
			registry = built.leaves
			agent := NewSessionAgent(SessionAgentOptions{Tools: built.tools, RestrictedRuns: authorizer}).(*sessionAgent)
			wrapped := agent.tools.Copy()[0]
			resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "fetch-call", Name: "view", Input: `{"file_path":"x"}`})
			require.NoError(t, err)
			require.Equal(t, "fetch failure", resp.Content, "Child hook interception must remain omitted")
			require.Equal(t, 1, authorizer.calls, "Private fetch restriction checked once")
			require.Equal(t, map[bool]int{false: 0, true: 1}[bound], ingressCalls)
			recs := records()
			require.Len(t, recs, 1, "Private fetch failure logging exactly once in either binding state")
			require.Equal(t, "fetch-call", recs[0]["tool_call_id"])
			require.Equal(t, []string{"restricted", "tool"}, policyLeafBaselineStack(built.leaves.entries["view"].tool))
		})
	}
}
