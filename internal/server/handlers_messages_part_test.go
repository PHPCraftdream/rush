package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
)

// createTestMessageWithAllParts creates a test message with all part types.
func createTestMessageWithAllParts(t *testing.T, ctx context.Context, a *appPkg.App, sessionID string) message.Message {
	t.Helper()

	m, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:               "call-1",
				Name:             "bash",
				Input:            `{"command":"ls"}`,
				ProviderExecuted: true,
				Finished:         true,
			},
			message.TextContent{Text: "narrative"},
			message.ReasoningContent{Thinking: "thoughts"},
			message.ToolResult{
				ToolCallID: "call-1",
				Name:       "bash",
				Content:    "output",
				Data:       "ZGF0YQ==",
				MIMEType:   "text/plain",
				Metadata:   `{"diff":"..."}`,
				IsError:    false,
			},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	return m
}

// TestHandleDeleteMessagePartTypeGuard verifies that delete_message_part
// only allows deleting TextContent and ReasoningContent, protecting
// structural parts (ToolCall, ToolResult, Finish) from deletion.
func TestHandleDeleteMessagePartTypeGuard(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "delete-part-test")
	require.NoError(t, err)

	// Build a hub and client to capture replies.
	hub := newHub()
	go hub.Run(ctx)
	t.Cleanup(func() { ctx.Done() })

	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	// Test 1: Deleting a ToolCall should fail.
	t.Run("delete_tool_call_fails", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(DeleteMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 0, // ToolCall
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-1",
			Type:    CmdDeleteMessagePart,
			Payload: payload,
		}

		handleDeleteMessagePart(ctx, a, client, msg)

		// Read the reply.
		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventError, reply.Type)
		require.Equal(t, "part type not deletable", reply.Error)

		// Verify message unchanged.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 5)
		require.IsType(t, message.ToolCall{}, updated.Parts[0])
	})

	// Test 2: Deleting a Finish should fail.
	t.Run("delete_finish_fails", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(DeleteMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 4, // Finish
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-2",
			Type:    CmdDeleteMessagePart,
			Payload: payload,
		}

		handleDeleteMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventError, reply.Type)
		require.Equal(t, "part type not deletable", reply.Error)

		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 5)
		require.IsType(t, message.Finish{}, updated.Parts[4])
	})

	// Test 3: Deleting TextContent should succeed.
	t.Run("delete_text_succeeds", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(DeleteMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 1, // TextContent
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-3",
			Type:    CmdDeleteMessagePart,
			Payload: payload,
		}

		handleDeleteMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventResponse, reply.Type)

		// Verify message has 4 parts and TextContent is gone.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 4)
		require.IsType(t, message.ToolCall{}, updated.Parts[0])
		require.IsType(t, message.ReasoningContent{}, updated.Parts[1]) // shifted
		require.IsType(t, message.ToolResult{}, updated.Parts[2])       // shifted
		require.IsType(t, message.Finish{}, updated.Parts[3])           // shifted

		// Verify no TextContent remains.
		for _, part := range updated.Parts {
			_, isText := part.(message.TextContent)
			require.False(t, isText, "TextContent should be deleted")
		}
	})

	// Test 4: Deleting ReasoningContent should succeed.
	t.Run("delete_reasoning_succeeds", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(DeleteMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 2, // ReasoningContent
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-4",
			Type:    CmdDeleteMessagePart,
			Payload: payload,
		}

		handleDeleteMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventResponse, reply.Type)

		// Verify message has 4 parts and ReasoningContent is gone.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 4)
		require.IsType(t, message.ToolCall{}, updated.Parts[0])
		require.IsType(t, message.TextContent{}, updated.Parts[1])
		require.IsType(t, message.ToolResult{}, updated.Parts[2]) // shifted
		require.IsType(t, message.Finish{}, updated.Parts[3])     // shifted

		// Verify no ReasoningContent remains.
		for _, part := range updated.Parts {
			_, isReasoning := part.(message.ReasoningContent)
			require.False(t, isReasoning, "ReasoningContent should be deleted")
		}
	})
}

// TestHandleUpdateMessagePartPreservesFields verifies that update_message_part
// preserves ToolCall.ProviderExecuted and ToolResult.Data/MIMEType/Metadata.
func TestHandleUpdateMessagePartPreservesFields(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "update-part-test")
	require.NoError(t, err)

	// Build a hub and client to capture replies.
	hub := newHub()
	go hub.Run(ctx)
	t.Cleanup(func() { ctx.Done() })

	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	// Test 1: Updating ToolCall Input preserves ProviderExecuted.
	t.Run("update_tool_call_preserves_provider_executed", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(UpdateMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 0, // ToolCall
			Content:   `{"command":"ls -la"}`,
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-1",
			Type:    CmdUpdateMessagePart,
			Payload: payload,
		}

		handleUpdateMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventResponse, reply.Type)

		// Verify ToolCall was updated and ProviderExecuted preserved.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 5)

		tc, ok := updated.Parts[0].(message.ToolCall)
		require.True(t, ok)
		require.Equal(t, `{"command":"ls -la"}`, tc.Input)
		require.True(t, tc.ProviderExecuted, "ProviderExecuted should be preserved")
		require.Equal(t, "call-1", tc.ID)
		require.Equal(t, "bash", tc.Name)
		require.True(t, tc.Finished)
	})

	// Test 2: Updating ToolResult Content preserves Data/MIMEType/Metadata.
	t.Run("update_tool_result_preserves_fields", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		payload, err := json.Marshal(UpdateMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 3, // ToolResult
			Content:   "updated output",
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-2",
			Type:    CmdUpdateMessagePart,
			Payload: payload,
		}

		handleUpdateMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventResponse, reply.Type)

		// Verify ToolResult was updated and other fields preserved.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 5)

		tr, ok := updated.Parts[3].(message.ToolResult)
		require.True(t, ok)
		require.Equal(t, "updated output", tr.Content)
		require.Equal(t, "ZGF0YQ==", tr.Data, "Data should be preserved")
		require.Equal(t, "text/plain", tr.MIMEType, "MIMEType should be preserved")
		require.Equal(t, `{"diff":"..."}`, tr.Metadata, "Metadata should be preserved")
		require.Equal(t, "call-1", tr.ToolCallID)
		require.Equal(t, "bash", tr.Name)
		require.False(t, tr.IsError)
	})

	// Test 3: Updating TextContent with garbage still works (sanity check).
	t.Run("update_text_content_sanity", func(t *testing.T) {
		m := createTestMessageWithAllParts(t, ctx, a, sess.ID)
		garbageContent := "garbage with \n newlines and \"quotes\""
		payload, err := json.Marshal(UpdateMessagePartPayload{
			MessageID: m.ID,
			PartIndex: 1, // TextContent
			Content:   garbageContent,
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-3",
			Type:    CmdUpdateMessagePart,
			Payload: payload,
		}

		handleUpdateMessagePart(ctx, a, client, msg)

		sentBytes := <-client.send
		var reply WSMessage
		require.NoError(t, json.Unmarshal(sentBytes, &reply))

		require.Equal(t, EventResponse, reply.Type)

		// Verify TextContent was updated.
		updated, err := a.Messages.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Len(t, updated.Parts, 5)

		tc, ok := updated.Parts[1].(message.TextContent)
		require.True(t, ok)
		require.Equal(t, garbageContent, tc.Text)
	})
}
