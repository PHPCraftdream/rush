package agent

import (
	"context"
	"net/http"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAgenticFetchExhaustedDepthRefusesBeforeResources(t *testing.T) {
	for _, mode := range []string{"explicit zero", "legacy context marker", "stale context with saved zero"} {
		t.Run(mode, func(t *testing.T) {
			env := testEnv(t)
			root, err := env.sessions.Create(t.Context(), "exhausted fetch parent")
			require.NoError(t, err)
			store := env.sessions.(session.AgentDepthStore)
			require.NoError(t, store.LowerAgentDepth(t.Context(), root.ID, 0))
			ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, root.ID)
			ctx = context.WithValue(ctx, tools.MessageIDContextKey, "fetch-message")
			switch mode {
			case "explicit zero":
				ctx, err = WithAgentDepth(ctx, 0)
			case "legacy context marker":
				ctx, err = WithPersistedSessionAgentDepth(ctx, env.sessions, root.ID)
			case "stale context with saved zero":
				ctx, err = WithAgentDepth(ctx, DefaultAgentDepth)
			}
			require.NoError(t, err)
			// Missing permission/config services make any resource-stage entry fail.
			coord := &coordinator{sessions: env.sessions}
			tool, err := coord.agenticFetchTool(ctx, &http.Client{})
			require.NoError(t, err)
			before, err := env.sessions.ListAll(t.Context())
			require.NoError(t, err)
			response, err := tool.Run(ctx, fantasy.ToolCall{
				ID: "fetch-call", Name: tools.AgenticFetchToolName,
				Input: `{"prompt":"read the page","url":"https://example.com/page"}`,
			})
			require.NoError(t, err)
			require.True(t, response.IsError)
			require.Equal(t, ErrAgentDepthExceeded.Error(), response.Content)
			after, err := env.sessions.ListAll(t.Context())
			require.NoError(t, err)
			require.Len(t, after, len(before), "refused fetch cannot create a child")
		})
	}
}
