// turnStream.onToolResult is the only production caller of
// augmentUnknownToolResult (agent_turn_stream.go's fork-patch hook). The unit
// tests in tool_suggest_test.go cover the rewrite itself; these drive the real
// entry point end to end — a real message service, the plain Create path, and
// the rewrite observed in what was actually persisted.
package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// toolSuggestFixture builds the thinnest turnStream that reaches the hook: a
// sessionAgent with real messages and real tools but asyncJobs nil, so every
// result is persisted by onToolResult's plain Create path.
type toolSuggestFixture struct {
	svc message.Service
	ts  *turnStream
}

func newToolSuggestFixture(t *testing.T, sessionID string) *toolSuggestFixture {
	t.Helper()
	// Same connection helper the ack-gate fixture uses: it opens a migrated
	// DB and disables foreign keys, so the session row does not have to exist
	// for the tool message to be created.
	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
	svc := message.NewService(db.New(conn))
	sa := &sessionAgent{
		messages: svc,
		tools: csync.NewSliceFrom([]fantasy.AgentTool{
			fantasy.NewAgentTool("bash", "run a command", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("out"), nil
			}),
			fantasy.NewAgentTool("grep", "search files", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("out"), nil
			}),
			fantasy.NewAgentTool("view", "read a file", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("out"), nil
			}),
		}),
	}
	return &toolSuggestFixture{
		svc: svc,
		ts: &turnStream{
			a: sa, ctx: t.Context(), currentAssistant: &message.Message{SessionID: sessionID},
			bumpActivity: func() {}, toolFinished: func() {},
		},
	}
}

// savedResult persists one fantasy tool result through onToolResult and
// returns the ToolResult part of the newest message.
func (f *toolSuggestFixture) savedResult(t *testing.T, result fantasy.ToolResultContent) message.ToolResult {
	t.Helper()
	require.NoError(t, f.ts.onToolResult(result))
	msgs, err := f.svc.List(context.Background(), f.ts.currentAssistant.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	require.NotEmpty(t, last.Parts)
	tr, ok := last.Parts[0].(message.ToolResult)
	require.True(t, ok, "first part is the persisted ToolResult, got %T", last.Parts[0])
	return tr
}

func TestOnToolResult_AugmentsUnknownToolError(t *testing.T) {
	t.Parallel()

	t.Run("close match is rewritten with a suggestion", func(t *testing.T) {
		t.Parallel()
		f := newToolSuggestFixture(t, "sugg-1")
		saved := f.savedResult(t, fantasy.ToolResultContent{
			ToolCallID: "c1", ToolName: "gash",
			Result: fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: gash")},
		})
		require.True(t, saved.IsError, "the error flag is kept")
		require.Contains(t, saved.Content, `unknown tool "gash"`)
		require.Contains(t, saved.Content, `did you mean "bash"?`)
	})

	t.Run("distant name gets the available list", func(t *testing.T) {
		t.Parallel()
		f := newToolSuggestFixture(t, "sugg-2")
		saved := f.savedResult(t, fantasy.ToolResultContent{
			ToolCallID: "c2", ToolName: "totally_bogus_tool",
			Result: fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: totally_bogus_tool")},
		})
		require.True(t, saved.IsError)
		require.Contains(t, saved.Content, `unknown tool "totally_bogus_tool"`)
		require.Contains(t, saved.Content, "Available tools: bash, grep, view")
	})

	t.Run("non tool-not-found errors pass through", func(t *testing.T) {
		t.Parallel()
		f := newToolSuggestFixture(t, "sugg-3")
		saved := f.savedResult(t, fantasy.ToolResultContent{
			ToolCallID: "c3", ToolName: "bash",
			Result: fantasy.ToolResultOutputContentError{Error: errors.New("permission denied")},
		})
		require.True(t, saved.IsError)
		require.Equal(t, "permission denied", saved.Content)
	})

	t.Run("successful results pass through", func(t *testing.T) {
		t.Parallel()
		f := newToolSuggestFixture(t, "sugg-4")
		saved := f.savedResult(t, fantasy.ToolResultContent{
			ToolCallID: "c4", ToolName: "view",
			Result: fantasy.ToolResultOutputContentText{Text: "file contents"},
		})
		require.False(t, saved.IsError)
		require.Equal(t, "file contents", saved.Content)
	})
}
