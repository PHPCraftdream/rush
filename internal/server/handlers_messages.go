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

// updateMessageAndVerify wraps a.Messages.Update for the four message-part
// editing handlers below (update content, update thinking, delete part,
// update part).
//
// It proves ONE thing: at the moment this function returns true, the row
// held the parts it just wrote. It does NOT prove the edit is durable. The
// agent turn that owns a mid-stream message keeps its own in-memory
// currentAssistant and, independently of anything this handler does, will
// write that in-memory copy back to the same row -- either from the next
// auto-checkpoint tick (conditional, fenced on checkpoint_generation) or,
// unconditionally and unfenced, from the terminal write at the end of the
// turn (internal/agent/agent_turn.go OnStepFinish -> a.messages.Update with
// a non-Partial Finish, which takes Update's "terminal update always wins"
// branch -- see internal/message/message.go's Update). Neither of those
// writers has any way to know an edit landed in between, so an edit that
// lands while the message is still mid-stream WILL be overwritten by
// stale in-memory content, deterministically, at the very latest by that
// terminal write. A client-side read-back cannot detect this because it
// runs before the overwrite happens.
//
// So instead of trying to make a mid-stream edit durable (that needs a real
// edit protocol reconciled with the agent's in-memory state -- out of reach
// from the WS handler layer, which has no visibility into currentAssistant),
// this function refuses the edit outright while the message is still
// mid-stream. It re-reads the row (never trusts the caller's possibly-stale
// in-memory `m`) and rejects with a conflict unless the freshly-read row is
// already terminally finished (!IsFinished() == still owned by the turn).
// This makes the guarantee the caller gets honest: "ok" is only ever
// returned for a row nothing else is going to touch again.
//
// task #569 / release-blocker F1 established that reporting "ok" over an
// edit that silently vanished is the actual bug; task #590 established that
// a same-instant read-back proves the write landed but not that it stays
// landed. Refusing mid-stream edits closes both: there is no window left in
// which the handler can reply "ok" and have the row change out from under
// the operator afterward.
func updateMessageAndVerify(ctx context.Context, a *appPkg.App, c *Client, msgID string, m message.Message) bool {
	// Re-read the row rather than trusting m's Finish part: m was loaded by
	// the caller (Get) before it mutated Parts for the edit, so it already
	// reflects the row's state at that moment, but re-reading here keeps this
	// check independent of caller discipline and closes the same race the
	// verification step below closes -- a concurrent terminal finish landing
	// between the caller's Get and this call.
	current, err := a.Messages.Get(ctx, m.ID)
	if err != nil {
		c.reply(msgID, EventError, nil, err.Error())
		return false
	}
	// Only assistant messages ever carry a Finish part -- user/system/tool
	// rows are written once and never streamed, so IsFinished() on them is
	// always false and must NOT be read as "still owned by a turn". Gate the
	// mid-stream check on Role == Assistant, or every user-message edit
	// would be refused too.
	if current.Role == message.Assistant && !current.IsFinished() {
		// The message is still owned by an in-flight agent turn (no terminal
		// Finish yet, or only a Partial one from the checkpoint ticker). Any
		// write here would be overwritten by that turn's next checkpoint or,
		// unconditionally, its terminal write -- see the doc comment above.
		// Refuse now rather than report an "ok" that will not stick.
		c.reply(msgID, EventError, nil, "message is still streaming and cannot be edited yet; please wait for it to finish")
		return false
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msgID, EventError, nil, err.Error())
		return false
	}
	// current was already terminal, so Update took the unconditional
	// "terminal update always wins" branch (message.Service.Update) -- no
	// re-read needed to confirm the write landed.
	return true
}

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
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
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
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
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
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
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
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
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
