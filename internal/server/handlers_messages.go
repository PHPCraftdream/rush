package server

// Message CRUD handlers: loading a session's messages and deleting,
// editing, or pinning individual messages and message parts.

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
)

func handleLoadMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p LoadMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	msgs, err := a.Messages.List(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if msgs == nil {
		msgs = []message.Message{}
	}
	// Wrap with the source sessionID so the frontend can route even an
	// EMPTY list to the right store (sub-agent vs main session). Without
	// this, a lazy load_messages for a sub-agent session that's still empty
	// returns [], and the frontend can't tell whether to clear the active
	// chat or the sub-agent buffer — it would blindly clear the active.
	c.reply(msg.ID, EventMessagesList, map[string]any{
		"SessionID": p.SessionID,
		"Messages":  toMessagesWire(msgs),
	}, "")
}

func handleDeleteMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	// Delete publishes DeletedEvent internally; events.go broadcasts EventMessageDeleted.
	if err := a.Messages.Delete(ctx, p.MessageID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	for _, id := range p.MessageIDs {
		if err := a.Messages.Delete(ctx, id); err != nil {
			slog.Warn("ws: failed to delete message", "id", id, "err", err)
		}
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessageContent(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessageContentPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Replace text parts with the new content, keep all other parts intact
	newParts := make([]message.ContentPart, 0, len(m.Parts))
	replaced := false
	for _, part := range m.Parts {
		if _, ok := part.(message.TextContent); ok && !replaced {
			newParts = append(newParts, message.TextContent{Text: p.Content})
			replaced = true
		} else if _, ok := part.(message.TextContent); ok {
			// skip additional text parts — merged into first
		} else {
			newParts = append(newParts, part)
		}
	}
	if !replaced {
		newParts = append([]message.ContentPart{message.TextContent{Text: p.Content}}, newParts...)
	}
	m.Parts = newParts
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessageThinking(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessageThinkingPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	found := false
	for i, part := range m.Parts {
		if rc, ok := part.(message.ReasoningContent); ok {
			m.Parts[i] = message.ReasoningContent{
				Thinking:         p.Thinking,
				Signature:        rc.Signature,
				ThoughtSignature: rc.ThoughtSignature,
				ToolID:           rc.ToolID,
				ResponsesData:    rc.ResponsesData,
				StartedAt:        rc.StartedAt,
				FinishedAt:       rc.FinishedAt,
			}
			found = true
			break
		}
	}
	if !found {
		c.reply(msg.ID, EventError, nil, "message has no thinking part")
		return
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	m.Parts = append(m.Parts[:p.PartIndex], m.Parts[p.PartIndex+1:]...)
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	switch part := m.Parts[p.PartIndex].(type) {
	case message.TextContent:
		m.Parts[p.PartIndex] = message.TextContent{Text: p.Content}
	case message.ReasoningContent:
		m.Parts[p.PartIndex] = message.ReasoningContent{
			Thinking:         p.Content,
			Signature:        part.Signature,
			ThoughtSignature: part.ThoughtSignature,
			ToolID:           part.ToolID,
			ResponsesData:    part.ResponsesData,
			StartedAt:        part.StartedAt,
			FinishedAt:       part.FinishedAt,
		}
	case message.ToolCall:
		m.Parts[p.PartIndex] = message.ToolCall{
			ID:       part.ID,
			Name:     part.Name,
			Input:    p.Content,
			Finished: part.Finished,
		}
	case message.ToolResult:
		m.Parts[p.PartIndex] = message.ToolResult{
			ToolCallID: part.ToolCallID,
			Name:       part.Name,
			Content:    p.Content,
			IsError:    part.IsError,
		}
	default:
		c.reply(msg.ID, EventError, nil, "part type not editable")
		return
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTogglePinMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p TogglePinMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Messages.SetPinned(ctx, p.MessageID, p.Pinned); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
