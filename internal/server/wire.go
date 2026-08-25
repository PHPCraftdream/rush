package server

import "github.com/PHPCraftdream/rush/internal/message"

// PartWire is the JSON wire format for a ContentPart sent to the browser.
// All fields use PascalCase to match the TypeScript interface names.
type PartWire struct {
	Type string `json:"type"`

	// text
	Text string `json:"Text,omitempty"`

	// thinking
	Thinking string `json:"Thinking,omitempty"`

	// tool_call
	ID       string `json:"ID,omitempty"`
	Name     string `json:"Name,omitempty"`
	Input    string `json:"Input,omitempty"`
	Finished bool   `json:"Finished,omitempty"`

	// tool_result
	ToolCallID string `json:"ToolCallID,omitempty"`
	Content    string `json:"Content,omitempty"`
	IsError    bool   `json:"IsError,omitempty"`
	Metadata   string `json:"Metadata,omitempty"`

	// finish
	Reason        string `json:"Reason,omitempty"`
	FinishMessage string `json:"Message,omitempty"`
	Details       string `json:"Details,omitempty"`
	// Partial mirrors message.Finish.Partial: true when this Finish part was
	// written by the auto-checkpoint ticker mid-stream, not a real step-end.
	// The web UI uses this (task #590) to tell an in-flight assistant
	// message apart from a terminally finished one, so it can hide the Edit
	// control while editing it would be silently overwritten by the turn's
	// next checkpoint or terminal write.
	Partial bool `json:"Partial,omitempty"`
}

// UsageWire is the per-message token accounting sent to the browser.
//
// CacheHitRatio is a POINTER so it can be null: a provider that does not
// report caching has no ratio, and emitting 0 there would be indistinguishable
// from a genuine cache miss. The browser must render "n/a" on null rather than
// substituting a number.
type UsageWire struct {
	InputTokens         int64    `json:"InputTokens"`
	OutputTokens        int64    `json:"OutputTokens"`
	ReasoningTokens     int64    `json:"ReasoningTokens,omitempty"`
	CacheCreationTokens int64    `json:"CacheCreationTokens"`
	CacheReadTokens     int64    `json:"CacheReadTokens"`
	PromptTokens        int64    `json:"PromptTokens"`
	TotalTokens         int64    `json:"TotalTokens"`
	CostUSD             float64  `json:"CostUSD"`
	Provider            string   `json:"Provider,omitempty"`
	Model               string   `json:"Model,omitempty"`
	CacheSupport        string   `json:"CacheSupport,omitempty"`
	CacheHitRatio       *float64 `json:"CacheHitRatio"`
	Estimated           bool     `json:"Estimated,omitempty"`
}

func toUsageWire(u *message.TokenUsage) *UsageWire {
	if u == nil {
		return nil
	}
	w := &UsageWire{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		ReasoningTokens:     u.ReasoningTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens,
		PromptTokens:        u.PromptTokens(),
		TotalTokens:         u.TotalTokens,
		CostUSD:             u.CostUSD,
		Provider:            u.Provider,
		Model:               u.Model,
		CacheSupport:        string(u.CacheSupport),
		Estimated:           u.Estimated,
	}
	if ratio, ok := u.CacheHitRatio(); ok {
		w.CacheHitRatio = &ratio
	}
	return w
}

// MessageWire is the JSON wire format for a Message sent to the browser.
type MessageWire struct {
	ID                  string     `json:"ID"`
	Role                string     `json:"Role"`
	SessionID           string     `json:"SessionID"`
	Parts               []PartWire `json:"Parts"`
	Model               string     `json:"Model"`
	Provider            string     `json:"Provider"`
	ReasoningEffort     string     `json:"ReasoningEffort,omitempty"`
	CreatedAt           int64      `json:"CreatedAt"`
	UpdatedAt           int64      `json:"UpdatedAt"`
	IsSummaryMessage    bool       `json:"IsSummaryMessage"`
	Pinned              bool       `json:"Pinned"`
	Hidden              bool       `json:"Hidden"`
	AutoResumed         bool       `json:"AutoResumed"`
	BackgroundJobNotice bool       `json:"BackgroundJobNotice"`
	Usage               *UsageWire `json:"Usage,omitempty"`
	// DeleteGeneration is the delete-generation watermark (task #737,
	// replacing task #731's RowID field -- message.Message.DeleteGeneration's
	// doc comment has the full mechanism). Only ever non-zero on the
	// payload of an EventMessageDeleted push -- toMessageWire is also used
	// for message_created/message_updated broadcasts and the messages_list
	// snapshot's per-message rows, none of which populate
	// Message.DeleteGeneration server-side, so this field is 0/omitted
	// there. The web client reads it ONLY off message_deleted.
	DeleteGeneration int64 `json:"DeleteGeneration,omitempty"`
}

func toPartWire(part message.ContentPart) PartWire {
	switch p := part.(type) {
	case message.TextContent:
		return PartWire{Type: "text", Text: p.Text}
	case message.ReasoningContent:
		return PartWire{Type: "thinking", Thinking: p.Thinking}
	case message.ToolCall:
		return PartWire{Type: "tool_call", ID: p.ID, Name: p.Name, Input: p.Input, Finished: p.Finished}
	case message.ToolResult:
		return PartWire{Type: "tool_result", ToolCallID: p.ToolCallID, Name: p.Name, Content: p.Content, IsError: p.IsError, Metadata: p.Metadata}
	case message.Finish:
		return PartWire{Type: "finish", Reason: string(p.Reason), FinishMessage: p.Message, Details: p.Details, Partial: p.Partial}
	default:
		return PartWire{Type: "unknown"}
	}
}

func toMessageWire(m message.Message) MessageWire {
	parts := make([]PartWire, len(m.Parts))
	for i, p := range m.Parts {
		parts[i] = toPartWire(p)
	}
	return MessageWire{
		ID:                  m.ID,
		Role:                string(m.Role),
		SessionID:           m.SessionID,
		Parts:               parts,
		Model:               m.Model,
		Provider:            m.Provider,
		ReasoningEffort:     m.ReasoningEffort,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
		IsSummaryMessage:    m.IsSummaryMessage,
		Pinned:              m.Pinned,
		Hidden:              m.Hidden,
		AutoResumed:         m.AutoResumed,
		BackgroundJobNotice: m.BackgroundJobNotice,
		Usage:               toUsageWire(m.Usage),
		DeleteGeneration:    m.DeleteGeneration,
	}
}

func toMessagesWire(msgs []message.Message) []MessageWire {
	result := make([]MessageWire, len(msgs))
	for i, m := range msgs {
		result[i] = toMessageWire(m)
	}
	return result
}
