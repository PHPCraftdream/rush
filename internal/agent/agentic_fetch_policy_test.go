package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
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

// fetchCaptureLogs is the local capture harness for the private-fetch
// logging assertion below (the root captureLogs helper moved; the JSON
// handler swap keeps the identical record stream).
func fetchCaptureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	})
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

func TestPrivateFetchPolicyBindingOmissionLogging(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			records := fetchCaptureLogs(t)
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
					expected, err := registry.Resolve("view")
					require.NoError(t, err)
					require.Equal(t, expected, leaf, "Private fetch resolves exact construction registry")
					require.Equal(t, "fetch-session", tools.GetSessionFromContext(ctx))
					require.Equal(t, "fetch-call", call.ID)
					return leaf.Run(ctx, call)
				})
			}
			built := buildPrivateFetchTools(ctx, child, authorizer, newBuildResourceScope())
			registry = built.Leaves
			agent := NewSessionAgent(SessionAgentOptions{Tools: built.Tools, RestrictedRuns: authorizer}).(*sessionAgent)
			wrapped := agent.tools.Copy()[0]
			resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "fetch-call", Name: "view", Input: `{"file_path":"x"}`})
			require.NoError(t, err)
			require.Equal(t, "fetch failure", resp.Content, "Child hook interception must remain omitted")
			require.Equal(t, 1, authorizer.calls, "Private fetch restriction checked once")
			require.Equal(t, map[bool]int{false: 0, true: 1}[bound], ingressCalls)
			recs := records()
			require.Len(t, recs, 1, "Private fetch failure logging exactly once in either binding state")
			require.Equal(t, "fetch-call", recs[0]["tool_call_id"])
			resolved, resolveErr := built.Leaves.Resolve("view")
			require.NoError(t, resolveErr)
			require.NotNil(t, resolved, "private fetch registry keeps the pinned view leaf")
		})
	}
}
