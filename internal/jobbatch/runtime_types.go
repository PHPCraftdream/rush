package jobbatch

import (
	"context"
	"time"
)

// RunnerConfig configures one Runner.
type RunnerConfig struct {
	CommitHook      func(context.Context, Transition) error
	CommitTimeout   time.Duration // Positive when CommitHook is set.
	Limits          Limits
	Executor        Executor
	ControlTimeout  time.Duration // Per physical Stop/Dispose call, from dispatch; positive.
	RecordRetention int           // Records kept for Events; positive.
}

// SubmitRequest submits a root. Root must be a batch.
type SubmitRequest struct {
	Root ID
	Spec Spec
	Ref  string // Optional caller reference; journaled, never payload.
}

// AddRequest appends items to a nonterminal batch.
type AddRequest struct {
	Parent ID
	Items  []Spec
	Ref    string
}

// OutputChunk is one read of an execution's output stream.
type OutputChunk struct {
	Data      []byte `json:"data,omitempty"` // A caller-owned copy.
	Next      uint64 `json:"next"`
	Oldest    uint64 `json:"oldest"`
	EOF       bool   `json:"eof,omitempty"`
	Truncated bool   `json:"truncated,omitempty"` // The cursor was older than Oldest.
}

// RecordType names a public feed record.
type RecordType string

const (
	RecordSubmit            RecordType = "submit"
	RecordAdd               RecordType = "add"
	RecordStopRequested     RecordType = "stop_requested"
	RecordStarted           RecordType = "started"
	RecordStartRejected     RecordType = "start_rejected"
	RecordSettled           RecordType = "settled"
	RecordControl           RecordType = "control"
	RecordCapacityAvailable RecordType = "capacity_available"
	RecordDeadlineExpired   RecordType = "deadline_expired"
	RecordHostClosing       RecordType = "host_closing"
	RecordRelease           RecordType = "release"
	RecordCommand           RecordType = "command"
	RecordNotice            RecordType = "notice"
	RecordClosed            RecordType = "closed"
)

// Record is one public feed entry: metadata only, never a payload, Summary,
// Details, message, output, argument or error text. Fields not meaningful
// for a type are zero.
//
//	event records   Root/Node/Token/State/Cause/Reason/Code/Group/Ref as the
//	                event carries them; submit and add list the created IDs
//	                in IDs (add: Node is the parent)
//	command         Command, Node, Token, Cause
//	notice          Root, Node, Kind, State, Cause, Counts
//	closed          the last record, with Sequence S_F and Cursor F
type Record struct {
	Cursor   uint64       `json:"cursor"`
	Sequence uint64       `json:"sequence"`
	Type     RecordType   `json:"type"`
	Root     ID           `json:"root,omitempty"`
	Node     ID           `json:"node,omitempty"`
	Token    uint64       `json:"token,omitempty"`
	State    State        `json:"state,omitempty"`
	Cause    Cause        `json:"cause,omitempty"`
	Reason   RejectReason `json:"reason,omitempty"`
	Op       ControlOp    `json:"op,omitempty"`
	Code     string       `json:"code,omitempty"`
	Group    string       `json:"group,omitempty"`
	Ref      string       `json:"ref,omitempty"`
	IDs      []ID         `json:"ids,omitempty"`
	Command  CommandType  `json:"command,omitempty"`
	Kind     NodeKind     `json:"kind,omitempty"`
	Counts   *Counts      `json:"counts,omitempty"`
}

// EventPage is one read of the public feed. Cursors start at 1. Head is the
// inclusive newest cursor (0 when empty); Oldest is the oldest retained
// (Head+1 when empty); Next is the last returned cursor plus one. Final is 0
// until the Runner is Closed, then F forever; Closed iff Final != 0.
type EventPage struct {
	Records []Record `json:"records,omitempty"`
	Next    uint64   `json:"next"`
	Oldest  uint64   `json:"oldest"`
	Head    uint64   `json:"head"`
	Final   uint64   `json:"final,omitempty"`
}
