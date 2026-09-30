// A launch that panics before the executor starts (R2B-14): the job fails
// directly with the launch error. It must not go through the delegation
// re-check, whose refresh reads a resumed child's OLD last message as the
// outcome while the tool result says "failed to start".
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestAsyncTool_LaunchPanicOnResumedDelegationFailsRowNotChildsOldAnswer
// resumes a child that already has a finished answer, then panics before the
// executor launches.
//
// Revert-check: with launchExecutor's recover finalizing through
// armDelegation again, recheckChild refreshed the row from the child's last
// finished message and the row carried "OLD ANSWER" as its result.
func TestAsyncTool_LaunchPanicOnResumedDelegationFailsRowNotChildsOldAnswer(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	ctx := t.Context()
	parent, err := sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(ctx, "tc-first", parent.ID, "child")
	require.NoError(t, err)
	_, err = messages.Create(ctx, child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "OLD ANSWER"}, message.Finish{Reason: message.FinishReasonEndTurn}},
	})
	require.NoError(t, err)

	registry := newWorkLedger(func(AsyncCompletion) {})
	registry.store = store
	coord := &coordinator{
		asyncJobs: registry, permissions: panickyPermissions{}, sessions: sessions,
		messages: messages, subAgentDrivers: newSubAgentDriverRegistry(),
	}
	registry.coord = coord
	inner := fantasy.NewAgentTool(AgentToolName, "delegate", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		t.Error("the executor must never run")
		return fantasy.ToolResponse{}, nil
	})
	wrapped := &asyncTool{inner: inner, coordinator: coord, name: AgentToolName}
	callCtx := context.WithValue(ctx, tools.SessionIDContextKey, parent.ID)
	callCtx = context.WithValue(callCtx, tools.MessageIDContextKey, "msg-1")
	callCtx = WithCallOrigin(callCtx, message.OriginWeb)

	input := fmt.Sprintf(`{"prompt":"go on","resume_session_id":%q}`, child.ID)
	resp, err := wrapped.Run(callCtx, fantasy.ToolCall{ID: "call", Name: AgentToolName, Input: input})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "failed to start")

	row, err := store.Get(ctx, parent.ID, "call")
	require.NoError(t, err)
	require.Equal(t, "failed", row.State)
	require.Contains(t, row.ResultSummary.String, "failed to start")
	require.NotContains(t, row.ResultSummary.String, "OLD ANSWER", "the launch error, not the resumed child's previous turn, is this call's outcome")

	// The error result is the job's own ack: tagged, so the row is announced
	// (and its failure delivered) when the result is persisted.
	var tag ackTag
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &tag))
	require.Equal(t, row.ClaimID, tag.ClaimID)
	require.NotNil(t, registry.claimAck(ctx, parent.ID, message.ToolResult{ToolCallID: "call", Metadata: resp.Metadata}))
}
