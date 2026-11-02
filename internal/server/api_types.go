package server

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// APISession is a safe public projection of a stored session. Workspace roots,
// model configuration, prompts, tokens, and process details are deliberately
// excluded. Times are formatted as RFC 3339 UTC strings.
type APISession struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int64  `json:"message_count"`
}

// APIMessage is a faithful but deliberately reduced message projection.
// Binary attachments are omitted to avoid exposing paths or encoded private
// data; tool inputs/results remain visible as typed parts and may themselves
// contain paths supplied by a tool or model.
type APIMessage struct {
	ID        string           `json:"id"`
	SessionID string           `json:"session_id"`
	Role      string           `json:"role"`
	Parts     []APIMessagePart `json:"parts"`
	CreatedAt string           `json:"created_at"`
}

// APIMessagePart contains only user-facing text and tool call/result data.
type APIMessagePart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Input      string `json:"input,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Content    string `json:"content,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

// APIModel identifies an available model without provider credentials.
type APIModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Role     string `json:"role,omitempty"`
}

func apiSessionFromModel(value session.Session) APISession {
	return APISession{
		ID: value.ID, Title: value.Title,
		CreatedAt: apiTimestamp(value.CreatedAt), UpdatedAt: apiTimestamp(value.UpdatedAt),
		MessageCount: value.MessageCount,
	}
}

func apiMessageFromModel(value message.Message) APIMessage {
	parts := make([]APIMessagePart, 0, len(value.Parts))
	for _, part := range value.Parts {
		switch item := part.(type) {
		case message.TextContent:
			parts = append(parts, APIMessagePart{Type: "text", Text: item.Text})
		case message.ToolCall:
			parts = append(parts, APIMessagePart{Type: "tool_call", ID: item.ID, Name: item.Name, Input: item.Input})
		case message.ToolResult:
			parts = append(parts, APIMessagePart{Type: "tool_result", ToolCallID: item.ToolCallID, Name: item.Name, Content: item.Content, IsError: item.IsError})
		case message.ImageURLContent:
			parts = append(parts, APIMessagePart{Type: "image", Content: "external image omitted"})
		}
	}
	return APIMessage{
		ID: value.ID, SessionID: value.SessionID, Role: string(value.Role),
		Parts: parts, CreatedAt: apiTimestamp(value.CreatedAt),
	}
}

func apiTimestamp(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}
