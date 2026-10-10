package tasktree

import "fmt"

const (
	SchemaVersion        = 1
	RootID        NodeID = "n0"
)

type (
	TreeKey   string
	NodeID    string
	RequestID string
	Revision  uint64
)

type NodeKind string

const (
	KindGroup NodeKind = "group"
	KindTask  NodeKind = "task"
)

type Status string

const (
	Pending    Status = "pending"
	InProgress Status = "in_progress"
	Completed  Status = "completed"
	Blocked    Status = "blocked"
	Abandoned  Status = "abandoned"
)

type Operation string

const (
	OpView    Operation = "view"
	OpInit    Operation = "init"
	OpAdd     Operation = "add"
	OpStart   Operation = "start"
	OpDone    Operation = "done"
	OpBlock   Operation = "block"
	OpUnblock Operation = "unblock"
	OpDrop    Operation = "drop"
	OpEdit    Operation = "edit"
	OpMove    Operation = "move"
	OpRemove  Operation = "rm"
	OpReopen  Operation = "reopen"
)

type ActorKind string

const (
	ActorAgent    ActorKind = "agent"
	ActorOperator ActorKind = "operator"
)

type Actor struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
}

type Limits struct {
	MaxNodes       int `json:"max_nodes"`
	MaxDepth       int `json:"max_depth"`
	MaxTitleBytes  int `json:"max_title_bytes"`
	MaxReasonBytes int `json:"max_reason_bytes"`
	MaxTombstones  int `json:"max_tombstones"`
	MaxReceipts    int `json:"max_receipts"`
}

type Selector struct {
	ID       NodeID `json:"id,omitempty"`
	Text     string `json:"text,omitempty"`
	WithinID NodeID `json:"within_id,omitempty"`
}

type Draft struct {
	Kind       NodeKind `json:"kind"`
	Title      string   `json:"title"`
	ActiveForm string   `json:"active_form,omitempty"`
	Children   []Draft  `json:"children,omitempty"`
}

type Command struct {
	Op         Operation `json:"op"`
	Target     Selector  `json:"target"`
	Drafts     []Draft   `json:"drafts,omitempty"`
	ParentID   NodeID    `json:"parent_id,omitempty"`
	BeforeID   NodeID    `json:"before_id,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Title      *string   `json:"title,omitempty"`
	ActiveForm *string   `json:"active_form,omitempty"`
	RemoveIDs  []NodeID  `json:"ids,omitempty"`
}

type Node struct {
	ID         NodeID   `json:"id"`
	ParentID   NodeID   `json:"parent_id,omitempty"`
	Kind       NodeKind `json:"kind"`
	Title      string   `json:"title"`
	ActiveForm string   `json:"active_form,omitempty"`
	Status     Status   `json:"status,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Children   []NodeID `json:"children,omitempty"`
}

type Tombstone struct {
	ID    NodeID   `json:"id"`
	Kind  NodeKind `json:"kind"`
	Title string   `json:"title"`
	Actor Actor    `json:"actor"`
}

type TitleGuard struct {
	Kind  NodeKind `json:"kind"`
	Title string   `json:"title"`
}

type Snapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	Initialized   bool                 `json:"initialized"`
	RootID        NodeID               `json:"root_id"`
	NextID        uint64               `json:"next_id"`
	Nodes         map[NodeID]Node      `json:"nodes"`
	Tombstones    map[NodeID]Tombstone `json:"tombstones"`
	TitleGuards   []TitleGuard         `json:"title_guards"`
}

type Delta struct {
	Created   []NodeID `json:"created,omitempty"`
	Updated   []NodeID `json:"updated,omitempty"`
	Removed   []NodeID `json:"removed,omitempty"`
	Completed []NodeID `json:"completed,omitempty"`
}

type Progress struct {
	Total        int  `json:"total"`
	Pending      int  `json:"pending"`
	InProgress   int  `json:"in_progress"`
	Completed    int  `json:"completed"`
	Blocked      int  `json:"blocked"`
	Abandoned    int  `json:"abandoned"`
	Actionable   int  `json:"actionable"`
	Unfinished   int  `json:"unfinished"`
	Settled      int  `json:"settled"`
	AllSettled   bool `json:"all_settled"`
	AllCompleted bool `json:"all_completed"`
}

type Summary struct {
	TreeKey     TreeKey  `json:"tree_key,omitempty"`
	Revision    Revision `json:"revision"`
	Initialized bool     `json:"initialized"`
	Progress    Progress `json:"progress"`
	ActiveID    NodeID   `json:"active_id"`
	NextID      NodeID   `json:"next_id"`
	ActivePath  []string `json:"active_path,omitempty"`
	NextPath    []string `json:"next_path,omitempty"`
}

type NodeBrief struct {
	ID       NodeID   `json:"id"`
	ParentID NodeID   `json:"parent_id,omitempty"`
	Kind     NodeKind `json:"kind,omitempty"`
	Title    string   `json:"title,omitempty"`
	Status   Status   `json:"status,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Removed  bool     `json:"removed,omitempty"`
}

type NodeView struct {
	ID         NodeID     `json:"id"`
	ParentID   NodeID     `json:"parent_id,omitempty"`
	Kind       NodeKind   `json:"kind"`
	Title      string     `json:"title"`
	ActiveForm string     `json:"active_form,omitempty"`
	Status     Status     `json:"status,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Progress   Progress   `json:"progress"`
	Children   []NodeView `json:"children,omitempty"`
}

type View struct {
	Summary Summary  `json:"summary"`
	Root    NodeView `json:"root"`
}

type ProblemCode string

const (
	CodeInvalidInput       ProblemCode = "invalid_input"
	CodeConflict           ProblemCode = "conflict"
	CodeAmbiguousTarget    ProblemCode = "ambiguous_target"
	CodeNotFound           ProblemCode = "not_found"
	CodeRemoved            ProblemCode = "removed"
	CodeRemovedByOperator  ProblemCode = "removed_by_operator"
	CodeInvalidTransition  ProblemCode = "invalid_transition"
	CodeInvalidTargetKind  ProblemCode = "invalid_target_kind"
	CodeLimitExceeded      ProblemCode = "limit_exceeded"
	CodeUninitialized      ProblemCode = "uninitialized"
	CodeAlreadyInitialized ProblemCode = "already_initialized"
	CodeRequestReused      ProblemCode = "request_reused"
	CodeForbidden          ProblemCode = "forbidden"
	CodeInvalidSnapshot    ProblemCode = "invalid_snapshot"
)

type Candidate struct {
	ID     NodeID   `json:"id"`
	Path   []string `json:"path"`
	Status Status   `json:"status,omitempty"`
}

type Problem struct {
	Code       ProblemCode `json:"code"`
	Message    string      `json:"message"`
	Expected   Revision    `json:"expected_revision,omitempty"`
	Current    Revision    `json:"current_revision,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

func (p *Problem) Error() string {
	return fmt.Sprintf("%s: %s", p.Code, p.Message)
}
