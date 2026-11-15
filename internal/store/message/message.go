package message

// Fork patch: this file diverges from upstream in two ways.
//
//  1. CreateMessageParams adds `ReasoningEffort` and `Hidden`; the Service
//     interface adds `Notify` (DB-less pubsub for streaming deltas) and
//     `SetPinned`. Matching DB migrations live under
//     `internal/db/migrations/20260310*`, `20260311*`, `20260313000001`.
//
//  2. We removed upstream's debounced/coalesced update layer (defaultUpdate-
//     Debounce, pendingState, Flush/FlushAll). Our streaming path uses the
//     in-process pubsub (Notify) for high-frequency UI updates and falls back
//     to synchronous Update for terminal-state writes — this matches the
//     latest-snapshot ticker in `internal/agent/agent.go`.
//
// Before merging upstream changes here: read CHANGELOG.fork.md section 2
// ("internal/message/message.go") and section 4.C (DB migrations).

import (
	"context"
	"database/sql"
	"sync"

	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

type CreateMessageParams struct {
	Role             MessageRole
	Parts            []ContentPart
	Model            string
	Provider         string
	ReasoningEffort  string // "low", "medium", "high", or "max" - reasoning effort for Claude models
	IsSummaryMessage bool
	// Hidden marks the message as invisible in the UI. Used for silent
	// background summaries that provide context to the LLM without cluttering
	// the conversation view.
	Hidden bool
	// AutoResumed marks a user message that was created by Phase 4 autonomous
	// idle-resume, not typed by a human; surfaced as a web badge.
	AutoResumed bool
	// BackgroundJobNotice marks a system-injected background-job-completion
	// notice so the web renders it as a notice, not a human message.
	BackgroundJobNotice bool
	// NoticeKind distinguishes the kind of notice -- see Message.NoticeKind's
	// doc for the value set.
	NoticeKind string
	// Origin marks the entry channel this message arrived through
	// (message.OriginCLI/Web/SDK); empty = unspecified.
	Origin Origin
}

type Service interface {
	pubsub.Subscriber[Message]
	pubsub.Shutdowner
	Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error)
	// CreateTx is Create's transaction-aware counterpart (phase-4 step 3,
	// docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.3): the INSERT
	// runs on tx instead of this service's own connection, so a caller that
	// also needs to UPDATE another table (session_notices' delivery outbox) in
	// the SAME transaction can do so atomically -- "one
	// transaction per notice: UPDATE ... RETURNING -> INSERT the history
	// message -> id in the row". Deliberately does NOT publish CreatedEvent
	// (tx may still roll back) -- the caller commits tx first, then calls
	// PublishCreated with the returned Message.
	CreateTx(ctx context.Context, tx *sql.Tx, sessionID string, params CreateMessageParams) (Message, error)
	// PublishCreated publishes msg's CreatedEvent exactly as Create does,
	// for a caller that created the row itself via CreateTx after its own
	// transaction committed (see CreateTx's doc).
	PublishCreated(msg Message)
	Update(ctx context.Context, message Message) error
	// UpdateTx is Update's transaction-aware counterpart (phase-4 step 4,
	// docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.4): the same
	// write, but against tx instead of this service's own connection, and
	// with publishing deferred to the returned func -- call it ONLY after
	// tx commits (the reaction-debt marker writes the step's final message
	// and marks session_notices rows reacted=1 in ONE
	// transaction; publishing before commit could let a subscriber observe
	// a write that still rolls back). On success the returned func is never
	// nil; it publishes nothing when the update affected 0 rows (e.g. the
	// message was concurrently deleted). On error it is nil.
	UpdateTx(ctx context.Context, tx *sql.Tx, message Message) (publish func(), err error)
	// Notify publishes a message update to the UI without writing to the database.
	// Use this for high-frequency streaming updates where DB durability is not
	// required on every token; call Update at the end to persist the final state.
	Notify(message Message)
	Get(ctx context.Context, id string) (Message, error)
	List(ctx context.Context, sessionID string) ([]Message, error)
	// ListWithWatermark is List plus the session's delete-generation
	// watermark as of the same read (task #737): a per-session counter
	// bumped once on every Delete/ForceDelete call, read here BEFORE the
	// List query runs (0 for a session with no deletes yet). Callers that
	// forward this watermark to a client (the messages_list WS reply) let
	// that client detect a snapshot whose read is PROVABLY older than a
	// delete it has already applied -- see Message.DeleteGeneration's doc
	// comment for the full mechanism. Returned messages do NOT carry their
	// own DeleteGeneration (this is the session-level watermark, not a
	// per-message one); only Delete/ForceDelete populate that.
	ListWithWatermark(ctx context.Context, sessionID string) ([]Message, int64, error)
	// ListPaginated returns at most limit messages for a session, newest first
	// (DESC by created_at), skipping the first offset rows. It is the paginated
	// counterpart to List so callers can read just the window they need instead
	// of decoding the entire history.
	//
	// WARNING: calling this back-to-back with a separate Count call is racy
	// under concurrent inserts (the two are independent round trips, so a
	// message written in between can make the two disagree) - callers that
	// need a self-consistent (window, total) pair, most notably reading a
	// LIVE, still-growing transcript, should use ListPaginatedSnapshot instead.
	ListPaginated(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// Count returns the total number of messages stored for a session. Used by
	// callers that page with ListPaginated to render accurate "N earlier
	// omitted" markers without loading the full history.
	Count(ctx context.Context, sessionID string) (int64, error)
	// ListPaginatedSnapshot is the race-free counterpart to calling
	// ListPaginated and Count separately: it returns a window of at most limit
	// messages, newest-first, skipping the first offset rows, TOGETHER with
	// the total message count - both computed from a single consistent
	// snapshot pinned at the moment this call begins.
	//
	// This matters specifically for offset-based paging over a session whose
	// message list can grow WHILE it is being read (the primary use case:
	// read_delegation_transcript.go observing a live sub-agent delegation).
	// Offset counts back from the newest end of a DESC-ordered list, so it is
	// only meaningful relative to a fixed snapshot of "what the newest row
	// is" - if new messages are inserted between reading the total and
	// reading the window, a numeric offset silently refers to a different
	// logical position than the one the caller computed it against,
	// producing skipped or duplicated messages on the next read of a
	// still-growing transcript. ListPaginatedSnapshot pins the boundary row
	// (the row `offset` positions back from the newest, as of one atomic
	// query) FIRST, then fetches the window strictly at-or-before that
	// boundary via a keyset filter - so any message inserted after this call
	// begins is correctly excluded from both the returned total and the
	// returned window, rather than shifting either one.
	ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) (window []Message, total int64, err error)
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	// ListCandidateInterruptedAssistantSessions returns, for every session
	// whose CHRONOLOGICALLY LAST message is an unfinished assistant message,
	// that session's id, the candidate message's id, and the session's
	// parent session id. Used by app.recoverInterruptedTurns (task #774) to
	// find the small set of sessions that actually need recovery instead of
	// scanning every session's full message history. See
	// ListCandidateInterruptedAssistantSessions (messages.sql) for the exact
	// query and its tie-break/correctness notes.
	ListCandidateInterruptedAssistantSessions(ctx context.Context) ([]InterruptedAssistantCandidate, error)
	// StampInterruptedAssistantIfStillLast conditionally applies a
	// process-restart error finish to msg (task #777, P1 release blocker).
	// The write lands only if, atomically in the same statement, msg.ID is
	// still unfinished AND still the chronologically last message in
	// sessionID. Returns (applied=false, nil) if the candidate went stale
	// between discovery and this call (superseded by a newer message, or
	// finished concurrently by its live owner) -- callers must treat that as
	// "skip, do not retry", exactly like DeleteMessageIfTerminal's
	// 0-rows-affected contract. See StampInterruptedAssistantIfStillLast
	// (messages.sql) for the full rationale.
	StampInterruptedAssistantIfStillLast(ctx context.Context, sessionID string, msg Message) (applied bool, err error)
	Delete(ctx context.Context, id string) error
	// ForceDelete unconditionally deletes a message, bypassing the
	// DeleteMessageIfTerminal streaming guard. This is ONLY for callers that
	// have verified no live agent turn owns the row (session cancelled + idle,
	// or crash recovery after holder death). The caller must ensure the row
	// is truly orphaned before calling this method; otherwise it can corrupt
	// the transcript by deleting a message a live turn is still writing to.
	ForceDelete(ctx context.Context, id string) error
	// DeleteTx is Rerun truncation's batch delete: it removes the given rows
	// of sessionID on tx, UNCONDITIONALLY (no streaming guard -- the caller
	// must hold the same proofs as for ForceDelete), and returns exactly the
	// rows this statement deleted (an id already gone is absent). Nothing is
	// published: call the returned publish func only AFTER tx commits (it
	// bumps the delete generation and publishes one DeletedEvent per row).
	DeleteTx(ctx context.Context, tx *sql.Tx, sessionID string, ids []string) (deleted []Message, publish func(), err error)
	DeleteSessionMessages(ctx context.Context, sessionID string) error
	SetPinned(ctx context.Context, id string, pinned bool) error
	// SetUsage records this message's token accounting and prompt-cache
	// breakdown (task #469). Called by the agent right after the step's cost
	// delta is computed; separate from Update because the Finish part is
	// appended before usage is known.
	//
	// Callers should treat a failure as non-fatal: statistics must never
	// abort a turn.
	SetUsage(ctx context.Context, id string, usage TokenUsage) error
	// UsageBySession returns per-model token/cache/cost aggregates for a
	// session, together with the number of assistant messages that carry no
	// usage at all, so a caller can state what its numbers were computed
	// over instead of implying full coverage.
	UsageBySession(ctx context.Context, sessionID string) (UsageReport, error)
	// UsageByModelInRange aggregates usage across ALL sessions in a time
	// window (Unix seconds), grouped by the model that produced each message
	// rather than by the session's current model.
	UsageByModelInRange(ctx context.Context, since, until int64) (UsageReport, error)
	// UsageByDayInRange is the same window bucketed by local calendar day.
	UsageByDayInRange(ctx context.Context, since, until int64) ([]DayUsage, error)
}

type service struct {
	*pubsub.Broker[Message]
	q db.Querier

	// qRead backs the standalone, read-only hot paths (Get, List,
	// ListPaginated, Count, ListUserMessages, ListAllUserMessages) that
	// don't need read-your-own-write consistency with a subsequent write in
	// the same call — most notably transcript pagination, which can walk a
	// large OFFSET and would otherwise sit in the same single-connection
	// queue as every in-flight agent message write. Defaults to q (see
	// NewService), so callers that don't opt into a separate reader get
	// today's serialized behavior unchanged.
	qRead db.Querier

	// deleteGenMu guards deleteGen.
	deleteGenMu sync.Mutex
	// deleteGen is the task #737 per-session delete-generation counter:
	// keyed by sessionID, incremented once on every Delete/ForceDelete
	// call for that session. Replaces task #731's MAX(rowid)-based
	// watermark, which did not move for a delete of a non-tail message
	// (see Message.DeleteGeneration's doc comment).
	//
	// An in-memory map is correct here specifically because the SAME
	// server process that publishes the DeletedEvent also serves
	// ListWithWatermark reads — there is no cross-process or persisted-
	// state requirement. A server restart resets every session's counter
	// to 0; that is intentional and safe, not a bug: a long-lived browser
	// client may still hold a higher remembered high-water mark from
	// before the restart, which just makes every snapshot compare as
	// stale until that client's own state catches up, forcing the
	// (already-correct) epoch/tombstone fallback more often than strictly
	// needed. That is the conservative direction — it can never cause a
	// resurrection, only an occasional unnecessary merge. Do NOT persist
	// this counter to the DB to "fix" that; it is not broken.
	deleteGen map[string]int64
}

func NewService(q db.Querier) Service {
	return &service{
		Broker:    pubsub.NewBroker[Message](),
		q:         q,
		qRead:     q,
		deleteGen: make(map[string]int64),
	}
}

// NewServiceWithReader is NewService plus a separate read-only Querier
// (qRead) for the standalone hot read paths documented on the service
// struct. Production wiring (internal/app.New) passes db.ConnectRead's
// WAL-mode read-only pool here so transcript pagination and message listing
// run concurrently with the single writer connection instead of queuing
// behind it. Passing a nil qRead behaves exactly like NewService.
//
// qRead is deliberately the concrete *db.Queries type, not the db.Querier
// interface: a nil *db.Queries boxed into a db.Querier interface value is
// itself non-nil (Go's typed-nil-in-interface gotcha — the interface has a
// type descriptor even though the underlying pointer is nil), which would
// make the "if qRead != nil" fallback below never fire. That is exactly
// the case internal/app.New hits whenever db.ConnectRead fails and
// deliberately passes a nil *db.Queries reader intending to fall back to
// the writer (see app.go's doc comment on that path) — with the interface
// type, this method would silently install the typed-nil as qRead instead
// of falling back, and every subsequent s.qRead.* call would nil-dereference.
func NewServiceWithReader(q db.Querier, qRead *db.Queries) Service {
	svc := NewService(q).(*service)
	if qRead != nil {
		svc.qRead = qRead
	}
	return svc
}
