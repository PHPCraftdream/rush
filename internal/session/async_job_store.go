// AsyncJobStore is the durable job store for the phase-4 durable async core
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.5 step 2): the DB
// row, not memory, decides an async job's outcome. Claim is the durable,
// idempotent start; Transition is the ONE terminal-state CAS (DUR-1/DUR-2).
// Both wrap host_lock.go's lazy host registration -- this file wires it into
// the claim path, exactly as that file's own doc anticipates.
package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/google/uuid"
)

// JobKind is the async_jobs.kind vocabulary this step writes: 'bash'/
// 'run_command' map to JobKindCommand, 'agent' to JobKindAgent,
// 'agentic_fetch' to JobKindFetch (doc sec.5 step 2's mapping table).
type JobKind string

const (
	JobKindCommand JobKind = "command"
	JobKindAgent   JobKind = "agent"
	JobKindFetch   JobKind = "fetch"
)

// HashJobInput is the sha256-hex of a tool call's raw input: async_jobs.
// input_hash distinguishes an idempotent retry (same id, same input) from a
// distinct call colliding on the id (doc sec.3.1).
func HashJobInput(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// ErrAsyncChildSessionBusy is ASYNC-01's claim-time refusal (doc sec.3.8): a
// delegation naming a child session id a RUNNING row already claims. Claim
// first recovers the conflicting row when its host is provably dead and
// retries once; the refusal is what remains for a live or unknown host, or a
// conflict that survives the retry.
type ErrAsyncChildSessionBusy struct {
	ChildSessionID string
}

func (e *ErrAsyncChildSessionBusy) Error() string {
	return "the sub-agent still has work running from a previous delegation; wait for its result (it arrives on its own, so do not retry the delegation; to message the running sub-agent use inject_agent)"
}

// ErrAsyncJobInputMismatch is returned when a (owner, tool_call_id) key is
// reused with a DIFFERENT input hash: a distinct call colliding on the id,
// not an idempotent retry (doc sec.3.1). The row holding the key is either
// running or terminal but not yet announced (its "started" result is still to
// be written; a terminal row that is already history is archived instead), so
// the text says "unfinished", not "running".
type ErrAsyncJobInputMismatch struct {
	ToolCallID string
}

func (e *ErrAsyncJobInputMismatch) Error() string {
	return fmt.Sprintf("async job %s is already in use by an unfinished call with different input", e.ToolCallID)
}

// ErrAsyncJobGone is returned by MarkAnnounced when the target row no
// longer exists (its owner session was deleted, which cascades; Rerun only
// voids or re-pends rows, it never deletes them) -- distinct from a real
// I/O failure so callers can treat it as a benign no-op.
var ErrAsyncJobGone = errors.New("async job store: row no longer exists")

// ClaimParams is Claim's input.
type ClaimParams struct {
	Owner          string
	ToolCallID     string
	Kind           JobKind
	Input          string // hashed internally via HashJobInput
	ChildSessionID string // "" for a plain (non-delegation) job
	OriginCLI      bool
	Deadline       *time.Time
	TimeoutKind    string // "wake_only"|"terminate_and_wake"; ignored if Deadline is nil
	// ToolName is the exact tool name ("bash"/"run_command"/"agent"/
	// "agentic_fetch"), finer-grained than Kind's three-way bucket. Stored so
	// the driver's pull (doc sec.3.3, step 3) can render the same notice text
	// FormatAsyncCompletion produces today without a second formatter or a
	// second source of truth for the tool name.
	ToolName string
	// TimeoutSeconds is the originally-requested timeout duration in
	// seconds (0 if Deadline is nil), quoted verbatim in the eventual
	// timeout notice text -- see ToolName's doc for why this is stored
	// rather than recomputed at pull time.
	TimeoutSeconds int
}

// ClaimResult is Claim's output.
type ClaimResult struct {
	Row      db.AsyncJob
	Existing bool // idempotent repeat of an already-claimed (owner, tool_call_id)
}

// TransitionOutcome is Transition's three-way result (doc sec.3.1: the
// winner commits, the loser adopts the row's state; step-2 scope adds
// "gone" for a since-deleted row, e.g. a cascaded session delete).
type TransitionOutcome int

const (
	TransitionWon TransitionOutcome = iota
	TransitionLost
	TransitionGone
)

// TransitionParams is Transition's input: the terminal state, its cause,
// the result payload, and the wake bit, all set by the SAME CAS (doc
// sec.3.2/DUR-2).
type TransitionParams struct {
	Owner         string
	ToolCallID    string
	State         string // completed|failed|cancelled|timed_out|interrupted
	NoticeKind    string
	ResultSummary string
	ResultIsError bool
	Wake          bool
	// Delivery is the outbox state this transition sets (preserving 'void'
	// per the CAS's own CASE, doc sec.3.8). "" means "pending" (a pull
	// candidate), which every cause uses in step 3; "done" skips the pull
	// entirely (step 6's job_kill, doc sec.3.2).
	Delivery string
	// Reacted is written in the SAME statement (step 6): false (0) for every
	// cause except job_kill, which passes true so its row is neither debt
	// nor a future notice the instant the transition commits (doc sec.3.4's
	// wake paragraph) -- wake=false for job_kill already excludes it from
	// the debt predicate, this is belt-and-suspenders for any future reader
	// that only checks reacted/delivery.
	Reacted bool
	// ClaimID (A11) is the claim_id the CALLER's own executor captured at
	// Claim time, included in the CAS's WHERE so a stale executor from a
	// deleted-then-re-claimed row loses against the row's CURRENT claim_id
	// instead of overwriting it just because both share (owner, tool_call_id)
	// and state='running'. "" (the zero value) means the caller has no
	// claim_id of its own to assert (recovery, test seeding) -- Transition
	// then resolves it from the row's OWN current value in the same
	// transaction, preserving that caller's pre-A11 "any running row
	// matches" behavior exactly.
	ClaimID string
}

// TransitionResult is Transition's output.
type TransitionResult struct {
	Outcome TransitionOutcome
	Row     db.AsyncJob // valid for Won/Lost; zero value for Gone
}

// AsyncJobStore is the durable job store: *sql.DB + *db.Queries + this
// process's lazily-registered host identity (doc sec.3.6: "лениво, при
// первом claim"). One store per App/coordinator.
type AsyncJobStore struct {
	sqlDB   *sql.DB
	q       *db.Queries
	dataDir string
	pid     int
	label   string

	// mu guards messages only and is never held across I/O: the writer
	// connection is single (SetMaxOpenConns(1)), so a lock held while waiting
	// for it deadlocks against any writer transaction that then needs the same
	// lock (R2A-2). Everything a writer transaction may read here is lock-free
	// (host, readQ are atomic).
	mu sync.Mutex
	// regMu serialises host registration and teardown (ensureHost, Close,
	// CloseKeepLock, SimulateCrashForTest). It IS held across the registration
	// I/O, so nothing that can run inside a writer transaction ever takes it.
	regMu sync.Mutex
	// host is this store's registered identity; nil before the first Claim.
	// Published only after RegisterHost fully succeeded.
	host atomic.Pointer[HostIdentity]
	// messages backs the first-registration dead-host sweep's delegation
	// text read (doc sec.3.7, recoveredDelegationText) -- optional, wired
	// once via SetMessages before the first Claim in production
	// (internal/app/app_agent_setup.go). Left nil, the sweep still runs; a
	// recovered delegation just falls back to interruptedNoChildTextText
	// instead of quoting the child's last message.
	messages message.Service
	// readQ is A8/C9's fix: a separate read-only connection pool for the
	// cross-process readers (LiveJobs/LiveWorkForRoots/JobsInTree/ReactionDebtExists/
	// ListAsyncJobsForOwner), wired once via SetReadConn -- see that
	// method's doc. Nil means "no reader pool wired": every reader method
	// falls back to the writer's own *db.Queries (today's behavior),
	// through readQuerier().
	readQ atomic.Pointer[db.Queries]
}

// SetMessages wires messages for the first-registration dead-host sweep
// (see the messages field's own doc). Safe to call at most once, before the
// first Claim; a later call is a no-op in production but harmless in tests
// that call it repeatedly.
func (s *AsyncJobStore) SetMessages(messages message.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = messages
}

func (s *AsyncJobStore) messageService() message.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages
}

// SetReadConn wires a separate read-only connection pool for this store's
// cross-process readers (A8/C9, docs/reviews/2026-09-29-async-phase4-round1.md):
// LiveJobs/LiveWorkForRoots/JobsInTree/ReactionDebtExists/VisibleReactionDebtExists/
// ListAsyncJobsForOwner run on it instead of the single writer connection
// (SetMaxOpenConns(1)), so a web session-list re-poll no longer stalls
// behind a write transaction for up to busy_timeout (30s). Mirrors the same
// pattern session.NewServiceWithReader/message.NewServiceWithReader already
// use (internal/app/app.go): WAL mode guarantees a read on a separate
// connection observes every write this same process already committed, so
// this is purely a concurrency optimization, never a staleness risk. A nil
// readDB is a no-op -- readers keep using the writer connection.
func (s *AsyncJobStore) SetReadConn(readDB *sql.DB) {
	if readDB == nil {
		return
	}
	s.readQ.Store(db.New(readDB))
}

// readQuerier returns the read-pool queries if SetReadConn wired one, else
// the writer's own -- every read-only reader method funnels through this so
// a store with no reader pool wired keeps working exactly as before.
func (s *AsyncJobStore) readQuerier() *db.Queries {
	if q := s.readQ.Load(); q != nil {
		return q
	}
	return s.q
}

// NewAsyncJobStore builds a store bound to sqlDB (the App's writer
// connection) and dataDir (for the host lock file -- a SEPARATE concern
// from the DB rows, doc sec.3.6). Host registration is deferred to the
// first Claim (lazy), not performed here.
func NewAsyncJobStore(sqlDB *sql.DB, dataDir string, pid int, label string) *AsyncJobStore {
	return &AsyncJobStore{sqlDB: sqlDB, q: db.New(sqlDB), dataDir: dataDir, pid: pid, label: label}
}

// ensureHost lazily registers this store's host identity exactly once (doc
// sec.3.6), returning its id. Safe for concurrent callers; registration
// failure is surfaced to the caller (Claim), never silently retried here.
//
// Registration is a writer-connection DB call plus file I/O, so it runs under
// regMu only -- never s.mu (R2A-2: a writer transaction that reads HostID or
// the read pool while registration held s.mu waited for a lock whose holder
// waited for that transaction's connection). The identity is published to the
// lock-free host pointer once it is complete.
//
// Doc sec.3.6/3.7: the process that actually performs registration runs ONE
// sweep over every dead host right after, outside every lock -- a concurrent
// caller that only observes an already-registered host (the common case)
// never pays for this sweep's DB/lock-probe cost.
func (s *AsyncJobStore) ensureHost(ctx context.Context) (string, error) {
	if h := s.host.Load(); h != nil {
		return h.ID, nil
	}
	s.regMu.Lock()
	if h := s.host.Load(); h != nil {
		s.regMu.Unlock()
		return h.ID, nil
	}
	h, err := RegisterHost(ctx, s.dataDir, s.pid, s.label, s.q)
	if err != nil {
		s.regMu.Unlock()
		return "", err
	}
	s.host.Store(h)
	s.regMu.Unlock()

	if _, sweepErr := s.SweepDeadHosts(ctx, s.messageService()); sweepErr != nil {
		slog.Warn("async job store: first-registration dead-host sweep failed", "err", sweepErr)
	}
	return h.ID, nil
}

// HostID returns this store's registered host id, or "" before the first
// successful Claim. Lock-free, so it is safe inside a writer transaction.
func (s *AsyncJobStore) HostID() string {
	if h := s.host.Load(); h != nil {
		return h.ID
	}
	return ""
}

// Claim is the durable, idempotent job start (doc sec.3.1/3.8, sec.5 step
// 2/6): one transaction, read-by-key first. Same input hash -> idempotent
// "existing" (by whatever state the row is in -- running or terminal, never
// a second executor). Different hash -> ErrAsyncJobInputMismatch. A
// delegation naming a child session id a RUNNING row already claims:
// the conflicting row's host is recovered (doc sec.3.7's primitive) and the
// claim retried ONCE if that host is provably dead; a live or unknown host,
// or a conflict that survives the retry, is ErrAsyncChildSessionBusy --
// always BEFORE "started" (async_tool.go never sees a job to report as
// running).
func (s *AsyncJobStore) Claim(ctx context.Context, p ClaimParams) (ClaimResult, error) {
	hostID, err := s.ensureHost(ctx)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("async job store: claim: register host: %w", err)
	}
	inputHash := HashJobInput(p.Input)

	const maxAttempts = 2 // fresh attempt + one retry after a dead-host recovery
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, conflict, err := s.claimOnce(ctx, p, hostID, inputHash)
		if err != nil {
			return ClaimResult{}, err
		}
		if conflict == nil {
			return result, nil
		}
		if attempt == maxAttempts-1 {
			break
		}
		// ASYNC-01 (doc sec.3.8): recover the conflicting row IFF its host is
		// provably dead, then retry the claim once. RecoverDeadHost itself is
		// a no-op (RecoveryOutcome{}, nil) for a live or unknown host, so the
		// retry below simply re-observes the same conflict and the loop falls
		// through to the refusal after the last attempt.
		if _, recErr := s.RecoverDeadHost(ctx, conflict.HostID, s.messageService()); recErr != nil {
			slog.Warn("async job store: claim: dead-host recovery of conflicting row failed; refusing the delegation",
				"child_session_id", p.ChildSessionID, "host_id", conflict.HostID, "err", recErr)
			break
		}
	}
	return ClaimResult{}, &ErrAsyncChildSessionBusy{ChildSessionID: p.ChildSessionID}
}

// claimOnce is one attempt of Claim's transaction: read-by-key (idempotent
// repeat), then -- for a delegation -- the ASYNC-01 partial-unique-index
// conflict check, then the insert. A non-nil conflict return means a RUNNING
// row already claims p.ChildSessionID; the transaction is rolled back
// (nothing was written) and the caller decides whether to recover+retry.
func (s *AsyncJobStore) claimOnce(ctx context.Context, p ClaimParams, hostID, inputHash string) (ClaimResult, *db.AsyncJob, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, nil, fmt.Errorf("async job store: claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	q := db.New(tx)
	existing, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID})
	switch {
	case err == nil:
		// B14/A14b: an existing row that is already HISTORY (terminal state,
		// delivery done/void) is not an in-flight claim -- a reused
		// tool_call_id (a provider that numbers calls per response) must
		// start a NEW row, not be refused for up to 7 days as "already
		// started earlier"/"different input" (see ArchiveAsyncJobToolCallID's
		// own doc for the state!='running' guard's importance). R2A-6: a
		// terminal row the model already saw announced but whose notice is
		// still undelivered (delivery='pending', announced=1) is history too
		// -- its notice stays pullable under the archived key. Archive the
		// old row out of the active key, then fall through to the fresh-claim
		// path below exactly as if no row existed. A terminal row that is not
		// announced yet is NOT archived: its "started" result is still to be
		// written, and a repeat of the same call answers idempotently.
		if isArchivableHistoryRow(existing) {
			archivedID := fmt.Sprintf("%s%s%s", p.ToolCallID, archivedToolCallIDMarker, uuid.NewString())
			rows, archErr := q.ArchiveAsyncJobToolCallID(ctx, db.ArchiveAsyncJobToolCallIDParams{
				NewToolCallID: archivedID, UpdatedAt: time.Now().Unix(),
				OwnerSessionID: p.Owner, OldToolCallID: p.ToolCallID,
			})
			if archErr != nil {
				return ClaimResult{}, nil, fmt.Errorf("async job store: claim: archive reused tool_call_id: %w", archErr)
			}
			if rows > 0 {
				// R2A-4: check-ins that named the row by its old key follow it.
				if _, err := q.RepointSessionNoticesJobToolCallID(ctx, db.RepointSessionNoticesJobToolCallIDParams{
					NewJobToolCallID: sql.NullString{String: archivedID, Valid: true}, Owner: p.Owner,
					OldJobToolCallID: sql.NullString{String: p.ToolCallID, Valid: true},
				}); err != nil {
					return ClaimResult{}, nil, fmt.Errorf("async job store: claim: repoint notices of archived row: %w", err)
				}
				break // fresh claim: child-conflict check + insert below
			}
			// Lost a race archiving this row (should not happen under the
			// single-writer serialization this store relies on elsewhere) --
			// fall back to treating it as still-active below.
		}
		if existing.InputHash != inputHash {
			return ClaimResult{}, nil, &ErrAsyncJobInputMismatch{ToolCallID: p.ToolCallID}
		}
		if err := tx.Commit(); err != nil {
			return ClaimResult{}, nil, fmt.Errorf("async job store: claim: commit idempotent read: %w", err)
		}
		return ClaimResult{Row: existing, Existing: true}, nil, nil
	case errors.Is(err, sql.ErrNoRows):
		// Fresh claim -- fall through to the child-conflict check + insert.
	default:
		return ClaimResult{}, nil, fmt.Errorf("async job store: claim: read existing: %w", err)
	}

	if p.ChildSessionID != "" {
		conflict, err := q.GetRunningAsyncJobByChildSession(ctx, sql.NullString{String: p.ChildSessionID, Valid: true})
		switch {
		case err == nil:
			return ClaimResult{}, &conflict, nil
		case errors.Is(err, sql.ErrNoRows):
			// No conflicting RUNNING delegation -- proceed.
		default:
			return ClaimResult{}, nil, fmt.Errorf("async job store: claim: child conflict check: %w", err)
		}
	}

	now := time.Now().Unix()
	params := db.ClaimAsyncJobParams{
		OwnerSessionID: p.Owner,
		ToolCallID:     p.ToolCallID,
		Kind:           string(p.Kind),
		ToolName:       p.ToolName,
		TimeoutSeconds: int64(p.TimeoutSeconds),
		InputHash:      inputHash,
		HostID:         hostID,
		// ClaimID (A11): a random id minted for THIS claim alone, carried by
		// the executor and later threaded back into Transition's CAS so a
		// stale executor from a deleted-then-re-claimed row cannot commit
		// onto the row this fresh claim just created (see the migration's
		// own doc for the ABA scenario this closes).
		ClaimID:   uuid.NewString(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if p.ChildSessionID != "" {
		params.ChildSessionID = sql.NullString{String: p.ChildSessionID, Valid: true}
	}
	if p.OriginCLI {
		params.OriginCli = 1
	}
	if p.Deadline != nil {
		params.DeadlineAt = sql.NullInt64{Int64: p.Deadline.Unix(), Valid: true}
		params.TimeoutKind = sql.NullString{String: p.TimeoutKind, Valid: true}
	}
	row, err := q.ClaimAsyncJob(ctx, params)
	if err != nil {
		return ClaimResult{}, nil, fmt.Errorf("async job store: claim: insert: %w", err)
	}
	if p.ChildSessionID != "" {
		// A1 item 2: a FRESH delegation on this child starts with a clean
		// slate -- any reacted_failed left over from an earlier, now-finished
		// delegation to the SAME child must not leak into this one's verdict
		// before this delegation has even produced a single reaction. Scoped
		// by owner=child (async_jobs.owner_session_id/session_notices.owner
		// for the CHILD's own rows), same tables/columns
		// MarkReactedWithMessageUpdate clears on a real reaction.
		if _, err := q.ClearReactedFailedForOwner(ctx, db.ClearReactedFailedForOwnerParams{
			UpdatedAt: now, OwnerSessionID: p.ChildSessionID,
		}); err != nil {
			return ClaimResult{}, nil, fmt.Errorf("async job store: claim: clear stale reacted_failed (async_jobs): %w", err)
		}
		if _, err := q.ClearSessionNoticesReactedFailedForOwner(ctx, db.ClearSessionNoticesReactedFailedForOwnerParams{
			UpdatedAt: now, Owner: p.ChildSessionID,
		}); err != nil {
			return ClaimResult{}, nil, fmt.Errorf("async job store: claim: clear stale reacted_failed (session_notices): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ClaimResult{}, nil, fmt.Errorf("async job store: claim: commit: %w", err)
	}
	return ClaimResult{Row: row}, nil, nil
}

// Transition is the ONE terminal-state CAS (DUR-1/DUR-2): one transaction,
// scoped to state='running' so only the first committer wins. A losing
// caller (0 rows affected) re-reads the row in the SAME transaction and
// reports TransitionLost with whatever state is there, or TransitionGone if
// the row no longer exists at all. Always preserves an existing 'void'
// delivery (doc sec.3.8) -- this is the only terminal-transition query, not
// one of two.
func (s *AsyncJobStore) Transition(ctx context.Context, p TransitionParams) (TransitionResult, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("async job store: transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := db.New(tx)
	wake := int64(0)
	if p.Wake {
		wake = 1
	}
	resultIsError := int64(0)
	if p.ResultIsError {
		resultIsError = 1
	}
	delivery := p.Delivery
	if delivery == "" {
		delivery = "pending"
	}
	reacted := int64(0)
	if p.Reacted {
		reacted = 1
	}
	claimID := p.ClaimID
	if claimID == "" {
		// A11: no claim_id of the caller's own to assert -- resolve the
		// row's CURRENT claim_id in this SAME transaction so the predicate
		// below is a no-op (matches whatever is already there) and this
		// caller's pre-A11 "any running row matches" behavior is preserved.
		// sql.ErrNoRows (row already gone) is not an error here: the CAS
		// below will simply affect 0 rows and the existing lost/gone
		// handling underneath takes over exactly as before this field
		// existed.
		if current, getErr := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID}); getErr == nil {
			claimID = current.ClaimID
		}
	}
	row, err := q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State:          p.State,
		Delivery:       delivery,
		NoticeKind:     p.NoticeKind,
		ResultSummary:  sql.NullString{String: p.ResultSummary, Valid: true},
		ResultIsError:  sql.NullInt64{Int64: resultIsError, Valid: true},
		Wake:           wake,
		Reacted:        reacted,
		UpdatedAt:      time.Now().Unix(),
		OwnerSessionID: p.Owner,
		ToolCallID:     p.ToolCallID,
		ClaimID:        claimID,
	})
	if err == nil {
		if err := tx.Commit(); err != nil {
			return TransitionResult{}, fmt.Errorf("async job store: transition: commit: %w", err)
		}
		return TransitionResult{Outcome: TransitionWon, Row: row}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TransitionResult{}, fmt.Errorf("async job store: transition: cas: %w", err)
	}

	// Lost the CAS (row already terminal) or the row is gone entirely --
	// read the current state in the SAME transaction.
	current, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID})
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return TransitionResult{}, fmt.Errorf("async job store: transition: commit (gone): %w", err)
		}
		return TransitionResult{Outcome: TransitionGone}, nil
	}
	if err != nil {
		return TransitionResult{}, fmt.Errorf("async job store: transition: read after lost cas: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TransitionResult{}, fmt.Errorf("async job store: transition: commit (lost): %w", err)
	}
	return TransitionResult{Outcome: TransitionLost, Row: current}, nil
}

// MarkAnnounced is the ack gate's DB half (DUR-7): marks that the "started"
// tool result for (owner, toolCallID) is persisted. Unconditional on state
// (a job that raced to terminal before its own "started" write commits
// must still be marked announced). Returns ErrAsyncJobGone if the row no
// longer exists (its owner session was deleted) -- a benign no-op for the
// caller, not a failure.
func (s *AsyncJobStore) MarkAnnounced(ctx context.Context, owner, toolCallID string) error {
	rows, err := s.q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
		UpdatedAt: time.Now().Unix(), OwnerSessionID: owner, ToolCallID: toolCallID,
	})
	if err != nil {
		return fmt.Errorf("async job store: mark announced: %w", err)
	}
	if rows == 0 {
		return ErrAsyncJobGone
	}
	return nil
}

// DeleteUnannounced is abort's DB half (ASYNC-05): the "started" tool-
// result write itself failed, so the row (scoped to announced=0, so a row
// that won the ack-gate race concurrently is never deleted out from under
// it) is removed without a trace.
func (s *AsyncJobStore) DeleteUnannounced(ctx context.Context, owner, toolCallID string) error {
	if _, err := s.q.DeleteUnannouncedAsyncJob(ctx, db.DeleteUnannouncedAsyncJobParams{
		OwnerSessionID: owner, ToolCallID: toolCallID,
	}); err != nil {
		return fmt.Errorf("async job store: delete unannounced: %w", err)
	}
	return nil
}

// deleteUnannouncedForClaim is DeleteUnannounced for a caller that read the row
// first (dead-host recovery): it removes that incarnation only. Two recoverers
// can list the same unannounced row; if one deletes it and a live host claims
// the same tool_call_id before the other's delete, a key-only delete would
// remove the live host's row (R3A-3). Reports whether this call deleted it.
func (s *AsyncJobStore) deleteUnannouncedForClaim(ctx context.Context, owner, toolCallID, claimID string) (bool, error) {
	rows, err := s.q.DeleteUnannouncedAsyncJobForClaim(ctx, db.DeleteUnannouncedAsyncJobForClaimParams{
		OwnerSessionID: owner, ToolCallID: toolCallID, ClaimID: claimID,
	})
	if err != nil {
		return false, fmt.Errorf("async job store: delete unannounced (claim): %w", err)
	}
	return rows > 0, nil
}

// Get reads the current row for (owner, toolCallID), or sql.ErrNoRows if
// none exists. A thin read-only wrapper for tests and any caller that
// needs a direct row read (e.g. diagnostics); the cross-process readers are
// in async_job_reader.go.
func (s *AsyncJobStore) Get(ctx context.Context, owner, toolCallID string) (db.AsyncJob, error) {
	return s.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: toolCallID})
}

// Close releases this store's host identity (doc sec.3.6's owner-exit
// path), if one was ever registered. Safe to call on a store that never
// claimed anything (no-op).
func (s *AsyncJobStore) Close(ctx context.Context) error {
	h := s.takeHost()
	if h == nil {
		return nil
	}
	// A clean exit takes this host's session-driver markers with it (the host
	// lock is still held here, so no row ever outlives its host's liveness).
	if _, err := s.q.DeleteSessionDriversForHost(ctx, h.ID); err != nil {
		slog.Warn("async job store: close: delete session driver markers failed", "host_id", h.ID, "err", err)
	}
	return h.Close(ctx, s.q)
}

// CloseKeepLock is Close's forced-shutdown counterpart (A12): a forced
// shutdown means live Run goroutines may still be writing through this store
// when the App tears down -- releasing the host lock here (as Close does)
// would let another process see this host as dead and recover its still-
// in-flight rows out from under it (DUR-5's "process alive iff it holds the
// lock" only holds for a process that has actually exited, not one whose
// goroutines are merely uncooperative past the shutdown grace period). This
// forgets the in-memory host handle WITHOUT releasing the OS lock, deleting
// the async_hosts row, or touching the lock file -- the OS releases the lock
// automatically when the process truly exits, same as an uncontrolled crash;
// a later recoverer then sees exactly what DUR-6 expects. The pin (and the
// own-host-id mark, which stays) lasts for the process's whole life on
// purpose: the stuck goroutines share no single completion signal, so no
// point exists at which an in-process release is provably safe (R2C-10); a
// long-lived embedder must exit after a forced shutdown, and until then the
// rows stay `running` on a locked host that a new App in this process cannot
// recover. Safe to call on a store that never claimed anything (no-op).
func (s *AsyncJobStore) CloseKeepLock() {
	h := s.takeHost()
	// Pin the OS lock: the *os.File finalizer would release it at next GC.
	if h != nil {
		retainHostLockUntilExit(h.lock)
	}
}

// SimulateCrashForTest releases this store's own OS host lock and forgets
// its "own id" marking, WITHOUT deleting the async_hosts/async_jobs rows or
// the lock file itself -- modeling a process that died: the OS releases its
// file locks automatically on exit, and nothing else about on-disk state
// changes. This is the ONLY way a same-process, multi-App test (doc sec.6's
// "two App instances on one data dir" scenarios) can make a LATER
// RecoverDeadHost/ProbeHost call -- even one issued by another store in the
// SAME test process -- correctly see this host as dead, since IsOwnHostID's
// registry is otherwise scoped to the whole OS process, not to an
// individual App/store instance (see that registry's own doc). Test-only:
// no production code path calls this -- Close is the real, clean exit.
func (s *AsyncJobStore) SimulateCrashForTest() error {
	h := s.takeHost()
	if h == nil {
		return nil
	}
	err := h.lock.Release()
	unmarkOwnHostID(h.ID)
	return err
}

// takeHost detaches the registered identity, first waiting out an in-flight
// registration (regMu) so a Close racing ensureHost cannot leave a host
// published after teardown.
func (s *AsyncJobStore) takeHost() *HostIdentity {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.host.Swap(nil)
}

// isArchivableHistoryRow reports whether row no longer holds its tool_call_id
// key: terminal, and either fully delivered/voided or announced with its
// notice merely still pending (R2A-6). Mirrors ArchiveAsyncJobToolCallID's
// own WHERE clause.
func isArchivableHistoryRow(row db.AsyncJob) bool {
	if row.State == "running" {
		return false
	}
	return row.Delivery == "done" || row.Delivery == "void" || (row.Delivery == "pending" && row.Announced != 0)
}

// archivedToolCallIDMarker separates the original tool_call_id from the uuid
// in an archived row's key (ArchiveAsyncJobToolCallID).
const archivedToolCallIDMarker = "#reused#"

// displayToolCallID is the tool_call_id the model saw: an archived row's key
// with the archive suffix removed. A pending row archived by R2A-6 is pulled
// under its archived key, but its notice must still name the model's id.
func displayToolCallID(key string) string {
	if i := strings.Index(key, archivedToolCallIDMarker); i >= 0 {
		return key[:i]
	}
	return key
}
