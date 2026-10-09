package protocol

import (
	"context"
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

// Backend is the task service used by the neutral agent protocol.
type Backend interface {
	Summary(context.Context, tasktree.TreeKey) (tasktree.Summary, error)
	View(context.Context, tasktree.TreeKey, tasktree.Selector) (tasktree.View, error)
	Mutate(context.Context, tasktree.TreeKey, tasktree.Actor, tasktree.RequestID, tasktree.Revision, tasktree.Command) (tasktree.MutationReply, error)
}

type Invocation struct {
	TreeKey   tasktree.TreeKey   `json:"tree_key"`
	Actor     tasktree.Actor     `json:"actor"`
	RequestID tasktree.RequestID `json:"request_id"`
}

type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Details struct {
	Summary  tasktree.Summary     `json:"summary"`
	Receipt  *tasktree.Receipt    `json:"receipt,omitempty"`
	Replayed bool                 `json:"replayed,omitempty"`
	Created  []tasktree.NodeBrief `json:"created,omitempty"`
	View     *tasktree.View       `json:"view,omitempty"`
	Problem  *tasktree.Problem    `json:"problem,omitempty"`
}

type ToolResult struct {
	Text    string  `json:"text"`
	Details Details `json:"details"`
	IsError bool    `json:"is_error"`
}
