package lab

import (
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

const SchemaVersion = 1

type Scenario struct {
	SchemaVersion int              `json:"schema_version"`
	TreeKey       tasktree.TreeKey `json:"tree_key"`
	Limits        tasktree.Limits  `json:"limits"`
	Steps         []Step           `json:"steps"`
}

type Step struct {
	Label     string             `json:"label"`
	Actor     tasktree.Actor     `json:"actor"`
	RequestID tasktree.RequestID `json:"request_id"`
	Payload   json.RawMessage    `json:"payload"`
	Expect    Expectation        `json:"expect"`
	Mode      string             `json:"mode,omitempty"`
}

type Expectation struct {
	IsError         *bool                `json:"is_error"`
	ProblemCode     tasktree.ProblemCode `json:"problem_code,omitempty"`
	Revision        *tasktree.Revision   `json:"revision,omitempty"`
	ReceiptRevision *tasktree.Revision   `json:"receipt_revision,omitempty"`
	Replayed        *bool                `json:"replayed,omitempty"`
	ActiveID        *tasktree.NodeID     `json:"active_id,omitempty"`
	NextID          *tasktree.NodeID     `json:"next_id,omitempty"`
	Progress        *tasktree.Progress   `json:"progress,omitempty"`
	Nodes           []NodeExpectation    `json:"nodes,omitempty"`
	RemovedIDs      []tasktree.NodeID    `json:"removed_ids,omitempty"`
	AbsentIDs       []tasktree.NodeID    `json:"absent_ids,omitempty"`
}

type NodeExpectation struct {
	ID         tasktree.NodeID    `json:"id"`
	ParentID   *tasktree.NodeID   `json:"parent_id,omitempty"`
	Kind       *tasktree.NodeKind `json:"kind,omitempty"`
	Title      *string            `json:"title,omitempty"`
	Status     *tasktree.Status   `json:"status,omitempty"`
	Reason     *string            `json:"reason,omitempty"`
	ActiveForm *string            `json:"active_form,omitempty"`
	Children   *[]tasktree.NodeID `json:"children,omitempty"`
}

type Checkpoint struct {
	SchemaVersion int               `json:"schema_version"`
	TreeKey       tasktree.TreeKey  `json:"tree_key"`
	Limits        tasktree.Limits   `json:"limits"`
	Envelope      tasktree.Envelope `json:"envelope"`
}

type StepResult struct {
	Label  string              `json:"label"`
	Result protocol.ToolResult `json:"result"`
}

type Report struct {
	Steps      []StepResult `json:"steps"`
	Checkpoint Checkpoint   `json:"checkpoint"`
}
