package jobbatch

// EventType names an engine input. Its string equals the record type the
// accepted event produces.
type EventType string

const (
	EventSubmit            EventType = "submit"
	EventAdd               EventType = "add"
	EventStopRequested     EventType = "stop_requested"
	EventStarted           EventType = "started"
	EventStartRejected     EventType = "start_rejected"
	EventSettled           EventType = "settled"
	EventControlReported   EventType = "control"
	EventCapacityAvailable EventType = "capacity_available"
	EventDeadlineExpired   EventType = "deadline_expired"
	EventHostClosing       EventType = "host_closing"
	EventRelease           EventType = "release"
)

// RejectReason says why a start attempt did not produce an execution.
type RejectReason string

const (
	RejectCapacity  RejectReason = "capacity"
	RejectFailure   RejectReason = "failure"
	RejectCancelled RejectReason = "cancelled"
)

// ControlOp is the control operation a ControlReported answers.
type ControlOp string

const ControlOpStop ControlOp = "stop"

// Event is one engine input. Fields not listed for Type must be zero; the
// engine rejects an event that sets them. Direct Engine callers transfer
// ownership of the event to Apply.
//
//	submit             Root, Spec, Ref
//	add                Parent, Items, Ref
//	stop_requested     Node
//	started            Node, Token
//	start_rejected     Node, Token, Reason, Code
//	settled            Node, Token, Result
//	control            Node, Token, Op, Code
//	capacity_available Group (the empty string is the default group)
//	deadline_expired   Node, Token
//	host_closing       none
//	release            Root
type Event struct {
	Type   EventType    `json:"type"`
	Root   ID           `json:"root,omitempty"`
	Parent ID           `json:"parent,omitempty"`
	Node   ID           `json:"node,omitempty"`
	Spec   Spec         `json:"spec"`
	Items  []Spec       `json:"items,omitempty"`
	Ref    string       `json:"ref,omitempty"`
	Token  uint64       `json:"token,omitempty"`
	Reason RejectReason `json:"reason,omitempty"`
	Op     ControlOp    `json:"op,omitempty"`
	Code   string       `json:"code,omitempty"`
	Result Result       `json:"result"`
	Group  string       `json:"group,omitempty"`
}

// CommandType names an execution command emitted by the engine.
type CommandType string

const (
	CommandStartLeaf CommandType = "start"
	CommandStopLeaf  CommandType = "stop"
)

// Command is an effect the Runtime performs; the engine never executes it.
type Command struct {
	Type  CommandType `json:"type"`
	Node  ID          `json:"node"`
	Token uint64      `json:"token"`
	Cause Cause       `json:"cause,omitempty"` // StopLeaf only.

	// Launch is the full immutable launch of a StartLeaf; zero for StopLeaf.
	Launch Launch `json:"launch"`
}

// Notice is metadata of one selected notification; never a payload.
type Notice struct {
	Root   ID       `json:"root"`
	Node   ID       `json:"node"`
	Kind   NodeKind `json:"kind"`
	State  State    `json:"state"`
	Cause  Cause    `json:"cause,omitempty"`
	Counts Counts   `json:"counts"`
}

// Transition is the result of one accepted event.
type Transition struct {
	// Sequence is the monotonic count of accepted events, this one included.
	Sequence uint64
	Event    Event
	Changed  []NodeView // Changed nodes only, in settlement/creation order.
	Commands []Command
	Notices  []Notice
	// SettledRoots lists the roots that became terminal in this transition,
	// each with the aggregate Batch.Wait will return.
	SettledRoots []Summary
	// Final is true exactly once: HostClosing has been applied and every root
	// is terminal after this transition.
	Final bool
}
