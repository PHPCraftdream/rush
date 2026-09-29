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
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
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
// delegation naming a child session id a RUNNING row already claims. Every
// conflict is a refusal in this step -- dead-host recovery of the
// conflicting row is step 5/6, not here.
type ErrAsyncChildSessionBusy struct {
	ChildSessionID string
}

func (e *ErrAsyncChildSessionBusy) Error() string {
	return "the sub-agent still has work running from a previous delegation; wait for its result"
}

// ErrAsyncJobInputMismatch is returned when a (owner, tool_call_id) key is
// reused with a DIFFERENT input hash: a distinct call colliding on the id,
// not an idempotent retry (doc sec.3.1).
type ErrAsyncJobInputMismatch struct {
	ToolCallID string
}

func (e *ErrAsyncJobInputMismatch) Error() string {
	return fmt.Sprintf("async job %s is already running with different input", e.ToolCallID)
}

// ErrAsyncJobGone is returned by MarkAnnounced when the target row no
// longer exists (e.g. a Rerun truncation raced it) -- distinct from a real
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

	mu   sync.Mutex
	host *HostIdentity
	// messages backs the first-registration dead-host sweep's delegation
	// text read (doc sec.3.7, recoveredDelegationText) -- optional, wired
	// once via SetMessages before the first Claim in production
	// (internal/app/app_agent_setup.go). Left nil, the sweep still runs; a
	// recovered delegation just falls back to interruptedNoChildTextText
	// instead of quoting the child's last message.
	messages message.Service
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
// Doc sec.3.6/3.7: the process that actually performs registration runs ONE
// sweep over every dead host right after, outside s.mu -- a concurrent
// caller that only observes an already-registered host (the common case)
// never pays for this sweep's DB/lock-probe cost.
func (s *AsyncJobStore) ensureHost(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.host != nil {
		id := s.host.ID
		s.mu.Unlock()
		return id, nil
	}
	h, err := RegisterHost(ctx, s.dataDir, s.pid, s.label, s.q)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.host = h
	messages := s.messages
	s.mu.Unlock()

	if _, sweepErr := s.SweepDeadHosts(ctx, messages); sweepErr != nil {
		slog.Warn("async job store: first-registration dead-host sweep failed", "err", sweepErr)
	}
	return h.ID, nil
}

// HostID returns this store's registered host id, or "" before the first
// successful Claim.
func (s *AsyncJobStore) HostID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host == nil {
		return ""
	}
	return s.host.ID
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
		if _, recErr := s.RecoverDeadHost(ctx, conflict.HostID, s.messages); recErr != nil {
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
		CreatedAt:      now,
		UpdatedAt:      now,
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
// longer exists (e.g. a Rerun truncation raced it) -- a benign no-op for
// the caller, not a failure.
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

// Get reads the current row for (owner, toolCallID), or sql.ErrNoRows if
// none exists. A thin read-only wrapper -- production readers move to the
// DB in a later step (doc sec.5 step 7); this exists now for tests and any
// caller that already needs a direct row read (e.g. diagnostics).
func (s *AsyncJobStore) Get(ctx context.Context, owner, toolCallID string) (db.AsyncJob, error) {
	return s.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: toolCallID})
}

// Close releases this store's host identity (doc sec.3.6's owner-exit
// path), if one was ever registered. Safe to call on a store that never
// claimed anything (no-op).
func (s *AsyncJobStore) Close(ctx context.Context) error {
	s.mu.Lock()
	h := s.host
	s.host = nil
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.Close(ctx, s.q)
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
	s.mu.Lock()
	h := s.host
	s.host = nil
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	err := h.lock.Release()
	unmarkOwnHostID(h.ID)
	return err
}
