package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/zeebo/xxh3"
)

type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// HashID returns the XXH3 hash of a session ID (UUID) as a hex string.
func HashID(id string) string {
	h := xxh3.New()
	h.WriteString(id)
	return fmt.Sprintf("%x", h.Sum(nil))
}

type Todo struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form"`
}

// HasIncompleteTodos returns true if there are any non-completed todos.
func HasIncompleteTodos(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status != TodoStatusCompleted {
			return true
		}
	}
	return false
}

type Session struct {
	ID               string
	ParentSessionID  string
	Title            string
	MessageCount     int64
	PromptTokens     int64
	CompletionTokens int64
	SummaryMessageID string
	Cost             float64
	Todos            []Todo
	CreatedAt        int64
	UpdatedAt        int64

	SmartModelProvider        string
	SmartModelID              string
	SmartModelReasoningEffort string // "low", "medium", "high", or "max"
	FastModelProvider         string
	FastModelID               string
	FastModelReasoningEffort  string // "low", "medium", "high", or "max"

	// Worker/reviewer overrides — empty means "inherit the folder/system
	// default", same convention as the smart/fast fields above. Unlike
	// smart/fast, most sessions never set these: worker/reviewer are
	// optional sub-agent model slots (task #466).
	WorkerModelProvider          string
	WorkerModelID                string
	WorkerModelReasoningEffort   string
	ReviewerModelProvider        string
	ReviewerModelID              string
	ReviewerModelReasoningEffort string

	SystemPrompt    string
	YoloEnabled     bool
	CancelRequested bool // Only populated by ListAll; use IsCancelRequested() for live checks.

	// DeletedTodos holds the Content strings of todos that the operator
	// explicitly removed via the UI. mergeTodos uses this set as a tombstone
	// filter so the model cannot resurrect them during multi-step turns.
	DeletedTodos []string

	// Fork patch (operator UX): persisted from --max-cost / --max-tokens /
	// --timeout at run start so sessions show/locks can display budget.
	EndedReason      string  // "done","canceled","timeout","max_cost","max_tokens","error","crash",""
	BudgetMaxCost    float64 // --max-cost value, 0 if unlimited
	BudgetMaxTokens  int64   // --max-tokens value, 0 if unlimited
	BudgetTimeoutSec int64   // --timeout in seconds, 0 if unlimited

	// Wire-only fields filled by the web server when sending Session over WS;
	// NOT persisted to SQLite. Together they answer "is this session being
	// driven by another live process right now?" so the web UI can render
	// foreign sessions read-only with a "Followed: PID N" banner.
	OwnedExternal bool `json:",omitempty"` // a different live process holds the lock
	OwnedByPID    int  `json:",omitempty"` // PID of the lock holder, 0 if free / stale
}

// ModelSlotUpdate is an explicit provider/model pair for one session model
// slot (large or small), passed to Service.UpdateModels. See UpdateModels'
// doc comment for the nil-vs-non-nil semantics.
type ModelSlotUpdate struct {
	Provider string
	Model    string
}

type Service interface {
	pubsub.Subscriber[Session]
	Create(ctx context.Context, title string) (Session, error)
	// CreateWithID creates a top-level session with a caller-chosen ID. Used
	// by `rush run --session <id>` to make CLI/CI invocations idempotent:
	// the same ID across runs continues the same conversation. Returns an
	// error if a row with that ID already exists (UNIQUE constraint).
	CreateWithID(ctx context.Context, id, title string) (Session, error)
	CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	GetLast(ctx context.Context) (Session, error)
	List(ctx context.Context) ([]Session, error)
	// ListAll returns every session including children (no parent_session_id
	// filter). Used by sessions gc for garbage collection.
	ListAll(ctx context.Context) ([]Session, error)
	// ListSubSessions returns every session whose parent_session_id
	// equals the argument, ordered oldest-first. Used by the
	// --aggregation=attach path and the reduction-loss warning to
	// gather a parent run's sub-agent fan-out outputs after Run()
	// returns.
	ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error)
	// GetCallTreeActivity returns the freshest message activity anywhere in
	// rootID's call tree (rootID itself plus every descendant reachable via
	// parent_session_id) in ONE recursive-CTE query, instead of a
	// per-node Messages.List + ListSubSessions walk. ok is false when the
	// tree has no messages at all (nothing to report).
	GetCallTreeActivity(ctx context.Context, rootID string) (activity CallTreeActivity, ok bool, err error)
	// GetCallTreeActivityBatch is the batch form of GetCallTreeActivity: it
	// computes the freshest call-tree activity for EVERY id in rootIDs,
	// chunking the root list internally so a single batch can never exceed
	// SQLite's variable-parameter limit (callTreeActivityBatchChunkSize roots
	// per underlying query). Used by `sessions list`, which otherwise walked
	// the whole descendant tree of every running session individually. The
	// returned map is keyed by root session ID; roots with no activity in
	// their tree are simply absent from the map.
	GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]CallTreeActivity, error)
	SetUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64) error
	SetSummaryAndUsage(ctx context.Context, sessionID, summaryMessageID string, promptTokens, completionTokens int64) error
	SetTodos(ctx context.Context, sessionID string, todos []Todo, deletedTodos []string) error
	// IncrementCost atomically adds delta to the session's cost via an
	// additive SQL UPDATE. Always prefer this over a read-modify-write of the
	// cost column when accruing per-step or per-sub-agent cost: it is race-free
	// under fan-out (multiple sub-agent goroutines completing concurrently
	// and each charging the same parent) and across processes that ever
	// share a session ID. Returns the refreshed session snapshot.
	//
	// Semantics for delta = 0: the implementation short-circuits to a
	// plain Get so callers can use IncrementCost(id, 0) as a "verify the
	// session exists and grab its current snapshot" call without paying
	// the cost of an UPDATE. This preserves the not-found error path for
	// callers like coordinator.updateParentSessionCost where a child
	// with zero accrued cost still wants to fail if the parent went
	// away. Pass a non-zero delta only when you actually want to charge.
	IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error)
	// TransferChildCostToParent moves the child session's cost accrued since
	// the last transfer into the parent session, atomically in one DB
	// transaction. It reads the child's persisted parent_cost_accounted
	// ledger, charges only the delta (cost - accounted, clamped >= 0) to the
	// parent via the atomic IncrementSessionCost UPDATE, and advances the
	// child's accounted marker to its current cost — all inside one tx so a
	// crash between the parent charge and the child bookkeeping cannot leave
	// them inconsistent. Idempotent: a repeat call with no new child cost
	// charges zero. Replaces the old in-memory baseline scheme that lost cost
	// on sub-agent error paths, process restarts, and failed charges.
	TransferChildCostToParent(ctx context.Context, childSessionID, parentSessionID string) error
	UpdateModels(ctx context.Context, sessionID string, smart, fast *ModelSlotUpdate) error
	UpdateReasoningEffort(ctx context.Context, sessionID, smartEffort, fastEffort string) error
	// UpdateWorkerReviewerModels and UpdateWorkerReviewerReasoningEffort are
	// UpdateModels/UpdateReasoningEffort's siblings for the optional
	// worker/reviewer slots (task #466).
	UpdateWorkerReviewerModels(ctx context.Context, sessionID string, worker, reviewer *ModelSlotUpdate) error
	UpdateWorkerReviewerReasoningEffort(ctx context.Context, sessionID, workerEffort, reviewerEffort string) error
	UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error
	Rename(ctx context.Context, id string, title string) error
	Delete(ctx context.Context, id string) error
	// ForkSession clones srcID into a brand-new session in a single DB
	// transaction: it creates a fresh session row and copies the source's
	// models, system prompt, todos, and every message. A failure at any
	// point rolls back the whole clone so no half-built fork is left for a
	// client to see; the caller receives the error instead. If title is
	// empty it defaults to "<src title> fork". Returns the committed fork.
	//
	// ForkSession is a thin wrapper around ForkSessionTx with the web fork
	// button's defaults (server-generated UUID, top-level session, every
	// message copied). Callers that need --at truncation, a caller-chosen
	// ID, or parent linkage (e.g. `rush sessions fork`) should call
	// ForkSessionTx directly instead of duplicating the transaction.
	ForkSession(ctx context.Context, srcID, title string) (Session, error)
	// ForkSessionTx is the single transactional fork implementation shared
	// by every fork entry point (web fork button, `rush sessions fork`).
	// It clones srcID into a brand-new session in one DB transaction: a
	// fresh session row copying the source's models, system prompt,
	// reasoning effort, and todos/deleted_todos, plus the first o.LimitMsgs
	// messages verbatim (all messages when o.LimitMsgs is 0). A failure at
	// any point rolls back the whole clone, so no half-built fork is ever
	// visible. Returns the committed fork and the number of messages copied.
	//
	// ForkOptions fields default as follows when left zero-valued:
	//   - NewID: a fresh uuid.New().String()
	//   - Title: "<src title> fork"
	//   - ParentID: "" (top-level session, no parent)
	//   - LimitMsgs: 0 means "copy every message"; otherwise it truncates to
	//     the first LimitMsgs messages (1-indexed) and the call fails if
	//     LimitMsgs is out of the range 1..len(source messages). An empty
	//     source (zero messages) is always a valid fork target as long as
	//     LimitMsgs is left at 0 — the range check is skipped in that case.
	//
	// Unlike ForkSession, ForkSessionTx does not publish a pubsub.CreatedEvent
	// after commit: some callers (e.g. the CLI) run in a separate process
	// from the one that will observe the fork, so publishing would be a
	// silent no-op there. Callers that need the event (the web path) publish
	// it themselves after this returns successfully.
	ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error)

	// CancelRequested flag: cross-process cancel signal.
	RequestCancel(ctx context.Context, sessionID string) error
	IsCancelRequested(ctx context.Context, sessionID string) (bool, error)
	ClearCancelRequest(ctx context.Context, sessionID string) error

	// Fork patch: ended_reason + budget persistence for operator UX.
	SetEndedReason(ctx context.Context, sessionID, reason string) error
	SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error

	// Cross-process message inject (foundation for `rush sessions inject`).
	// CreatePendingInject enqueues a signal row asking whichever process is
	// currently running the session to splice messageID into its live prompt.
	// DrainPendingInjects is called from PrepareStep to consume those rows.
	CreatePendingInject(ctx context.Context, inject PendingInject) error
	DrainPendingInjects(ctx context.Context, sessionID string) ([]PendingInject, bool, error)
	// PeekInterruptInject reads the OLDEST interrupt=true pending_injects row
	// for sessionID WITHOUT deleting it. Used by handleInterruptTick to read
	// the message reference before building call data, so it can call
	// ConsumeInterruptInjectAndEnqueue atomically.
	//
	// Returns (nil, nil) when no interrupt row is pending.
	PeekInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error)
	// ConsumeInterruptInjectAndEnqueue atomically consumes the pending inject
	// row identified by injectID (as read via PeekInterruptInject) and
	// enqueues it to the run queue in a single transaction. Matching on the
	// specific injectID (not "the oldest row") avoids silently consuming a
	// different row than the one callData was built from when a session has
	// more than one pending interrupt row.
	//
	// Returns (nil, nil) when injectID no longer refers to a pending row.
	// Returns (pi, nil) when the row was successfully consumed and enqueued.
	// Returns error on failure — the transaction is rolled back, so the row
	// remains for retry.
	ConsumeInterruptInjectAndEnqueue(ctx context.Context, sessionID, injectID, idempotencyKey string, callData []byte) (*PendingInject, error)
	// DeleteInterruptInject removes a specific pending inject row by ID.
	// Used by detached interrupt runs to delete the durable pending row AFTER
	// they have confirmed execution (acquired OS lock). P0-2 fix.
	DeleteInterruptInject(ctx context.Context, injectID string) error

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool

	// Durable run queue for orphaned/detached calls (task #340)
	EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error
	LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, leaseTTL time.Duration) (*RunQueueEntry, error)
	// RenewRunQueueLease extends id's lease expiry, but ONLY if it is still
	// leased by leasedBy — a lease already reassigned to a different owner
	// (this owner lost the race to a prior CleanupExpiredLeases recovery) is
	// never silently extended. Returns false (no error) when the renewal
	// did not apply for that reason; the caller must treat false as "this
	// execution no longer owns the row" rather than retry the renewal.
	RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error)
	// AckRunQueueEntry, NackRunQueueEntry, NackRunQueueEntryNoAttemptPenalty,
	// and TerminalFailRunQueueEntry all take leasedBy and only affect the
	// row if it is CURRENTLY still leased by that same value — found by the
	// fifth @oh review pass over #337-349: without this, an executor that
	// lost its lease to a CleanupExpiredLeases recovery (rare now that
	// executeEntry renews its lease for the duration of a real turn, but not
	// impossible under a pathological scheduling stall) could otherwise
	// mutate/delete a row a DIFFERENT, currently-live executor now owns.
	AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error)
	NackRunQueueEntry(ctx context.Context, id, leasedBy, lastError string) error
	NackRunQueueEntryNoAttemptPenalty(ctx context.Context, id, leasedBy, lastError string) error
	TerminalFailRunQueueEntry(ctx context.Context, id, leasedBy string) error
	ListPendingRunQueueEntries(ctx context.Context) ([]RunQueueEntry, error)
	ListStaleLeasedRunQueueEntries(ctx context.Context, beforeTime int64) ([]RunQueueEntry, error)
	CleanupExpiredLeases(ctx context.Context, beforeTime int64) error
	// GetRunQueueEntry looks up a single entry by ID, regardless of status.
	// Returns (nil, nil) if the row no longer exists (acked/deleted), unlike
	// most Get* methods on this interface which propagate sql.ErrNoRows --
	// mirrors GetOrphanOutboxEntry's convention, chosen for the same reason:
	// DrainSessionNow (run_queue_drain_session.go) uses this to distinguish
	// "genuinely gone" from "still leased by someone else" for a row it no
	// longer holds a lease on, and both are valid, error-free outcomes.
	GetRunQueueEntry(ctx context.Context, id string) (*RunQueueEntry, error)
	// HasOutstandingRunQueueEntriesForSession reports whether sessionID has
	// ANY durable run-queue row left, in EITHER status ('pending' or
	// 'leased') -- not just 'pending'. Added for task #610: the pending-only
	// GetOldestPendingRunQueueEntryForSession lookup that DrainSessionNow's
	// terminal path used to infer "queue empty" from is blind to a row
	// currently leased by a DIFFERENT owner (another process, or another
	// pump instance racing this one) -- that row is durable, outstanding,
	// and its outcome is unknown to this call, yet the pending-only check
	// cannot see it at all. This is an explicit query, not an inference from
	// a failed lease attempt, precisely so DrainSessionNow's terminal
	// DrainComplete decision does not depend on absence-of-evidence from a
	// query that was never scoped to answer "is the queue empty" in the
	// first place.
	HasOutstandingRunQueueEntriesForSession(ctx context.Context, sessionID string) (bool, error)

	// Orphan call outbox (P0-3 fix): durable fallback when main run queue enqueue fails
	WriteToOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) error
	ListPendingOrphanOutboxEntries(ctx context.Context) ([]db.OrphanCallOutbox, error)
	// DrainOrphanOutboxEntry atomically moves an entry from orphan_call_outbox to
	// session_run_queue in a single database transaction. This eliminates the
	// vulnerable 'processing' state that could leave entries stranded after crashes.
	// Returns true if the entry was successfully drained (moved to main queue),
	// false if the entry was already drained by another pump or not in pending state.
	DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error)
	GetOrphanOutboxEntry(ctx context.Context, id string) (*db.OrphanCallOutbox, error)
	// RecordOrphanOutboxFailure counts one failed drain attempt against an
	// entry and quarantines it at max_attempts. Separate from the atomic
	// drain transaction on purpose -- see its implementation.
	RecordOrphanOutboxFailure(ctx context.Context, id, lastError string) (OrphanOutboxFailureOutcome, error)
}

type service struct {
	*pubsub.Broker[Session]
	db *sql.DB
	q  *db.Queries

	// readDB/qRead back the standalone, read-only hot paths (List, ListAll,
	// ListSubSessions, Get, GetLast, GetCallTreeActivity(Batch)) that don't
	// need read-your-own-write consistency with a subsequent write in the
	// same call. When a caller doesn't provide a separate reader (the
	// common NewService constructor, used by every test and by anything
	// that intentionally wants single-connection semantics against
	// :memory: databases), these simply alias db/q, so routing to them is
	// a no-op fallback to today's serialized behavior.
	//
	// Everything else (writes, transactions, and reads inside a
	// transaction like ForkSessionTx's GetSessionByID) intentionally keeps
	// using db/q — the single writer connection — so this split never
	// changes write semantics or read-after-write guarantees for anything
	// but the named standalone read paths.
	readDB *sql.DB
	qRead  *db.Queries
}

// CallTreeActivity is the freshest message activity found anywhere in a
// session's call tree (the session itself plus every descendant sub-agent
// session reachable via parent_session_id), as computed by the
// GetCallTreeActivity / GetCallTreeActivityBatch recursive-CTE queries.
type CallTreeActivity struct {
	// SessionID is the descendant (or root) session the activity belongs
	// to — i.e. which node in the tree produced LatestUnix.
	SessionID string
	// Role is the role of the freshest message ("assistant" / "tool" /
	// "user").
	Role string
	// LatestUnix is the newest message activity timestamp (max of
	// created_at / updated_at) across the whole tree.
	LatestUnix int64
}

func (s service) fromDBItem(item db.Session) Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	deletedTodos, err := unmarshalDeletedTodos(item.DeletedTodos)
	if err != nil {
		slog.Error("Failed to unmarshal deleted_todos", "session_id", item.ID, "error", err)
	}
	return Session{
		ID:               item.ID,
		ParentSessionID:  item.ParentSessionID.String,
		Title:            item.Title,
		MessageCount:     item.MessageCount,
		PromptTokens:     item.PromptTokens,
		CompletionTokens: item.CompletionTokens,
		SummaryMessageID: item.SummaryMessageID.String,
		Cost:             item.Cost,
		Todos:            todos,
		DeletedTodos:     deletedTodos,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,

		SmartModelProvider:        item.SmartModelProvider.String,
		SmartModelID:              item.SmartModelID.String,
		SmartModelReasoningEffort: item.SmartModelReasoningEffort.String,
		FastModelProvider:         item.FastModelProvider.String,
		FastModelID:               item.FastModelID.String,
		FastModelReasoningEffort:  item.FastModelReasoningEffort.String,

		WorkerModelProvider:          item.WorkerModelProvider.String,
		WorkerModelID:                item.WorkerModelID.String,
		WorkerModelReasoningEffort:   item.WorkerModelReasoningEffort.String,
		ReviewerModelProvider:        item.ReviewerModelProvider.String,
		ReviewerModelID:              item.ReviewerModelID.String,
		ReviewerModelReasoningEffort: item.ReviewerModelReasoningEffort.String,

		SystemPrompt: item.SystemPrompt,
		YoloEnabled:  item.YoloEnabled != 0,
	}
}

func marshalTodos(todos []Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]Todo, error) {
	if data == "" {
		return []Todo{}, nil
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []Todo{}, err
	}
	return todos, nil
}

func marshalDeletedTodos(contents []string) (string, error) {
	if len(contents) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(contents)
	if err != nil {
		return "[]", err
	}
	return string(data), nil
}

func unmarshalDeletedTodos(data string) ([]string, error) {
	if data == "" || data == "[]" {
		return []string{}, nil
	}
	var contents []string
	if err := json.Unmarshal([]byte(data), &contents); err != nil {
		return []string{}, err
	}
	return contents, nil
}

func NewService(q *db.Queries, conn *sql.DB) Service {
	broker := pubsub.NewBroker[Session]()
	return &service{
		Broker: broker,
		db:     conn,
		q:      q,
		readDB: conn,
		qRead:  q,
	}
}

// NewServiceWithReader is NewService plus a separate read-only connection
// (readConn/qRead) for the standalone hot read paths documented on the
// service struct. Production wiring (internal/app.New) uses this with
// db.ConnectRead's WAL-mode read-only pool so heavy reads (call-tree CTEs,
// `sessions list`/`grep`) run concurrently with the single writer connection
// instead of queuing behind it. Every other caller (tests, and anything
// against a :memory: database where a genuinely separate reader either
// can't see the writer's data or isn't worth the complexity) should keep
// using plain NewService, which makes readDB/qRead alias the writer and so
// behaves exactly as it did before this split.
func NewServiceWithReader(q *db.Queries, conn *sql.DB, qRead *db.Queries, readConn *sql.DB) Service {
	svc := NewService(q, conn).(*service)
	if qRead != nil && readConn != nil {
		svc.qRead = qRead
		svc.readDB = readConn
	}
	return svc
}
