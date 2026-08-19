// Durable run queue (session_run_queue) for orphaned/detached agent
// calls: entry and payload types, enqueue, lease/renew/ack/nack
// lifecycle, stale-lease recovery, and the DB-to-domain converters.

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/google/uuid"
)

// RunQueueEntry represents a durable orphaned/detached call that needs execution.
// It wraps SessionAgentCall data serialized as JSON for persistence.
type RunQueueEntry struct {
	ID              string
	SessionID       string
	CallData        string // JSON-serialized SessionAgentCall
	Status          string // pending | leased | acked
	LeasedBy        string
	LeasedAt        int64
	LeaseExpiresAt  int64
	Attempts        int64
	LastError       string
	TerminalFailure bool
	CreatedAt       int64
	UpdatedAt       int64
}

// ModelCfg is a JSON-serializable subset of config.SelectedModel
// (task #340, ROUND 3 migration). It mirrors the JSON-tagged fields
// of config.SelectedModel without importing the config package to avoid
// import cycles. The coordinator (which can import config) reconstructs
// the full Model from this data during pump execution.
type ModelCfg struct {
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	ReasoningEffort  string         `json:"reasoning_effort,omitempty"`
	Think            bool           `json:"think,omitempty"`
	MaxTokens        int64          `json:"max_tokens,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	TopK             *int64         `json:"top_k,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	ProviderOptions  map[string]any `json:"provider_options,omitempty"`
}

// SessionAgentCallData is a durable, serializable subset of agent.SessionAgentCall
// that can be stored in the run queue and reconstructed after process restart.
// It contains only the fields needed to execute a call, excluding process-local
// pointers and transient state (task #340, ROUND 3 migration).
//
// This is a mirror of agent.SessionAgentCall without creating an import cycle.
// The agent package converts between SessionAgentCall and SessionAgentCallData.
type SessionAgentCallData struct {
	SessionID   string
	Prompt      string
	Attachments []message.Attachment
	// ProviderOptions, Temperature, TopP, TopK, FrequencyPenalty, PresencePenalty
	// are NOT serialized here — they are pure functions of (Model, ProviderConfig)
	// computed via mergeCallOptions/getProviderOptions during pump execution.
	// Only ModelCfg is serialized because it contains the per-session pinned snapshot
	// (task #265, P0-1, task #340 ROUND 3).
	// LogicalCallID is the stable identifier for this logical request, used to build
	// the idempotency key for durable queue operations (P2-1 fix).
	LogicalCallID   string
	MaxOutputTokens int64
	NonInteractive  bool
	// SystemPromptOverride, if non-empty, replaces the agent's global system prompt
	SystemPromptOverride string
	// MaxCost aborts the run if total session cost exceeds this value (0 = no cap)
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this value
	MaxTokens int64
	// ExistingMessageID, when non-empty, marks this call as referencing a
	// user message that already exists in the DB (created by another process)
	ExistingMessageID string
	// InjectID, when non-empty, is the ID of a pending_injects row that
	// must be deleted AFTER successful OS lock acquisition (P0-2 fix)
	InjectID string
	// Model configuration overrides (task #265, P0-1) - we serialize ModelCfg
	// because it's the per-session pinned snapshot. The actual Model (with
	// live fantasy.LanguageModel and CatwalkCfg) is reconstructed by the
	// coordinator during pump execution (ROUND 3).
	// Pointers, so "explicitly set" is distinguishable from "zero value"
	SmartModel         *ModelCfg
	FastModel          *ModelCfg
	SystemPromptPrefix *string
	SystemPrompt       *string
}

// RunQueue constants
const (
	RunQueueStatusPending = "pending"
	RunQueueStatusLeased  = "leased"
	// acked is terminal and not stored (acked entries are deleted)
)

// EnqueueRunQueueEntry adds a call to the durable run queue.
// The caller should generate idempotencyKey ONCE and reuse it across retries.
// Returns error if the enqueue fails (caller should not proceed without durability).
//
// Idempotent on idempotencyKey (P2-1): the underlying INSERT uses
// ON CONFLICT(id) DO NOTHING, so retrying with the SAME key after an earlier
// attempt already committed the row is a no-op, not an error. sqlc's :one
// query reports that as sql.ErrNoRows (zero rows returned on conflict) —
// treated here as success, since the durable row this call wanted to exist
// already does.
func (s *service) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	now := time.Now().Unix()
	_, err := s.q.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID:        idempotencyKey,
		SessionID: sessionID,
		CallData:  string(callData),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// ceilUnixSeconds converts t to a whole-Unix-seconds timestamp, rounding UP
// (ceiling) rather than truncating down. Used for lease_expires_at (an
// INTEGER/whole-seconds DB column): rounding up guarantees the persisted
// deadline is never earlier than the true, sub-second-precision intended
// deadline (only ever up to ~1s later), which is the safe direction for a
// "how long is this lease still valid" column — see the P0-3 fix note at
// LeaseRunQueueEntry's call site for the full rationale.
func ceilUnixSeconds(t time.Time) int64 {
	if t.Nanosecond() == 0 {
		return t.Unix()
	}
	return t.Unix() + 1
}

// LeaseRunQueueEntry atomically claims the oldest pending entry for a session.
// Returns nil, nil if no pending entry exists (not an error).
// Uses a transactional pattern similar to ConsumeInterruptInjectAndEnqueue:
//  1. SELECT the oldest pending entry
//  2. UPDATE it to leased status in the same transaction
//  3. Return the leased entry
//
// The leasedBy and leaseExpiresAt are set to track who owns the entry and
// when it expires (allowing recovery from crashed pump instances).
func (s *service) LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, leaseTTL time.Duration) (*RunQueueEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// Step 1: Find the oldest pending entry for this session
	candidate, err := qtx.GetOldestPendingRunQueueEntryForSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // No pending entries
		}
		return nil, err
	}

	// Step 2: Lease it (atomic claim)
	acquiredAt := time.Now()
	now := acquiredAt.Unix()
	leasedAt := now
	// P0-3 fix: lease_expires_at is an INTEGER (whole Unix seconds) column,
	// but the true deadline (acquiredAt + leaseTTL) is sub-second precision.
	// The previous `now + int64(leaseTTL.Seconds())` computation FLOORED
	// twice: once by truncating acquiredAt down to `now` (losing up to 1s),
	// and again by truncating leaseTTL itself to whole seconds (for any
	// leaseTTL under 1 full second — e.g. the 100-300ms TTLs this package's
	// own test suite relies on for speed — int64(leaseTTL.Seconds()) is
	// exactly 0, recording the lease as ALREADY EXPIRED at the moment it is
	// created).
	//
	// A SINGLE floor (compute the full-precision deadline once, then
	// `.Unix()`) fixes the double-truncation bug but is still not safe
	// enough on its own: it can still lose up to ~1s depending on the
	// arbitrary sub-second wall-clock phase the lease happened to be
	// acquired at, and that loss is directly load-bearing for the
	// executeEntry watchdog (which seeds its deadline from this exact
	// persisted value, see run_queue_pump.go's P0-3 fix note) whenever the
	// safety margin is itself on the order of ~1s or less — e.g. TTL=1s
	// with margin clamped to 500ms, where "up to 1s of floor loss" can
	// exceed the ENTIRE margin on an unlucky wall-clock phase, causing the
	// watchdog to fire almost immediately (confirmed empirically: this
	// flips a real test between reliably passing and reliably failing
	// depending purely on which fraction of a second the test process
	// happened to start at — a genuinely nondeterministic regression, not
	// hypothetical).
	//
	// Round the true deadline UP (ceiling) to the next whole second
	// instead: this can only make the persisted deadline later than (or
	// equal to) the true intended deadline, NEVER earlier, so every
	// consumer of this column (CleanupExpiredLeases' expiry check, and the
	// executeEntry watchdog) sees a lease that is never reclaimed/treated-
	// as-expired before its true, intended lifetime — only ever up to ~1s
	// later, which is the safe direction for a "how long can this lease
	// still be considered valid" deadline. The production TTL (30s) and
	// safety margin (5s) make this ~1s of extra slack negligible; a few of
	// this package's own short-TTL tests needed their timing tolerances
	// widened to account for it (see p1_1_lease_watchdog_test.go and
	// p1_1_watchdog_window_test.go's P0-3 notes) — an unavoidable
	// consequence of anchoring the watchdog to a whole-seconds DB column at
	// all, not something a smarter rounding scheme can avoid.
	leaseExpiresAt := ceilUnixSeconds(acquiredAt.Add(leaseTTL))

	leased, err := qtx.LeaseRunQueueEntryByID(ctx, db.LeaseRunQueueEntryByIDParams{
		LeasedBy:       sql.NullString{String: leasedBy, Valid: leasedBy != ""},
		LeasedAt:       sql.NullInt64{Int64: leasedAt, Valid: true},
		LeaseExpiresAt: sql.NullInt64{Int64: leaseExpiresAt, Valid: true},
		UpdatedAt:      now,
		ID:             candidate.ID,
	})
	if err != nil {
		// Another goroutine leased it between the SELECT and UPDATE
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return dbToRunQueueEntry(leased), nil
}

// RenewRunQueueLease — see the Service interface for the full contract.
func (s *service) RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error) {
	rows, err := s.q.RenewRunQueueLease(ctx, db.RenewRunQueueLeaseParams{
		LeaseExpiresAt: sql.NullInt64{Int64: newExpiresAt, Valid: true},
		ID:             id,
		LeasedBy:       sql.NullString{String: leasedBy, Valid: leasedBy != ""},
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// AckRunQueueEntry marks a leased entry as successfully completed (terminal).
// Deletes the entry from the queue, but only if it is still leased by
// leasedBy. Returns the deleted ID.
func (s *service) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	deletedID, err := s.q.AckRunQueueEntry(ctx, db.AckRunQueueEntryParams{
		ID:       id,
		LeasedBy: sql.NullString{String: leasedBy, Valid: leasedBy != ""},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("run queue entry %q not found, not in leased state, or no longer leased by %q", id, leasedBy)
	}
	return deletedID, err
}

// NackRunQueueEntry releases a leased entry back to pending state (retry
// later), but only if it is still leased by leasedBy.
// Used for transient errors where retry is safe and necessary.
// Increments attempts count and records the error message.
func (s *service) NackRunQueueEntry(ctx context.Context, id, leasedBy, lastError string) error {
	now := time.Now().Unix()
	_, err := s.q.NackRunQueueEntry(ctx, db.NackRunQueueEntryParams{
		LastError: sql.NullString{String: lastError, Valid: lastError != ""},
		UpdatedAt: now,
		ID:        id,
		LeasedBy:  sql.NullString{String: leasedBy, Valid: leasedBy != ""},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run queue entry %q not found, not in leased state, or no longer leased by %q", id, leasedBy)
	}
	return err
}

// NackRunQueueEntryNoAttemptPenalty releases a leased entry back to pending
// state WITHOUT incrementing its attempts count, but only if it is still
// leased by leasedBy. Used for SessionLockBusyError: ordinary lock
// contention from another live process is expected, routine behavior, not a
// failure of the call itself, and must never count toward
// RunQueueMaxAttempts (see run_queue_pump.go's executeEntry) — otherwise the
// durable queue would silently delete accepted work after nothing more than
// a few turns of normal contention.
func (s *service) NackRunQueueEntryNoAttemptPenalty(ctx context.Context, id, leasedBy, lastError string) error {
	now := time.Now().Unix()
	_, err := s.q.NackRunQueueEntryNoAttemptPenalty(ctx, db.NackRunQueueEntryNoAttemptPenaltyParams{
		LastError: sql.NullString{String: lastError, Valid: lastError != ""},
		UpdatedAt: now,
		ID:        id,
		LeasedBy:  sql.NullString{String: leasedBy, Valid: leasedBy != ""},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run queue entry %q not found, not in leased state, or no longer leased by %q", id, leasedBy)
	}
	return err
}

// TerminalFailRunQueueEntry marks a leased entry as terminal failure (no
// retry), but only if it is still leased by leasedBy.
// Used for ErrCallAlreadyAttempted-type errors where retry would cause duplicates.
// Deletes the entry from the queue permanently.
func (s *service) TerminalFailRunQueueEntry(ctx context.Context, id, leasedBy string) error {
	_, err := s.q.TerminalFailRunQueueEntry(ctx, db.TerminalFailRunQueueEntryParams{
		ID:       id,
		LeasedBy: sql.NullString{String: leasedBy, Valid: leasedBy != ""},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run queue entry %q not found, not in leased state, or no longer leased by %q", id, leasedBy)
	}
	return err
}

// ListPendingRunQueueEntries returns all pending entries across all sessions.
// Used by the pump to scan for work.
func (s *service) ListPendingRunQueueEntries(ctx context.Context) ([]RunQueueEntry, error) {
	rows, err := s.q.ListPendingRunQueueEntries(ctx)
	if err != nil {
		return nil, err
	}
	return dbSliceToRunQueueEntries(rows), nil
}

// GetRunQueueEntry looks up a single entry by ID, regardless of status
// (pending, leased, or -- since it is deleted on ack -- simply gone).
// Returns (nil, nil) when the row does not exist, matching
// GetOrphanOutboxEntry's convention: a caller checking whether a row it once
// owned is still around, still leased by someone else, or genuinely
// resolved must be able to tell 'gone' apart from a real error.
func (s *service) GetRunQueueEntry(ctx context.Context, id string) (*RunQueueEntry, error) {
	entry, err := s.q.GetRunQueueEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return dbToRunQueueEntry(entry), nil
}

// ListStaleLeasedRunQueueEntries returns all leased entries with expired leases.
// Used by the pump to recover entries from crashed instances.
func (s *service) ListStaleLeasedRunQueueEntries(ctx context.Context, beforeTime int64) ([]RunQueueEntry, error) {
	rows, err := s.q.ListStaleLeasedRunQueueEntries(ctx, sql.NullInt64{Int64: beforeTime, Valid: true})
	if err != nil {
		return nil, err
	}
	return dbSliceToRunQueueEntries(rows), nil
}

// CleanupExpiredLeases resets stale leased entries back to pending state.
// This is a maintenance operation that should run periodically.
func (s *service) CleanupExpiredLeases(ctx context.Context, beforeTime int64) error {
	now := time.Now().Unix()
	return s.q.CleanupExpiredLeases(ctx, db.CleanupExpiredLeasesParams{
		UpdatedAt:      now,
		LeaseExpiresAt: sql.NullInt64{Int64: beforeTime, Valid: true},
	})
}

// Helper functions to convert between DB and domain types

func dbToRunQueueEntry(entry db.SessionRunQueue) *RunQueueEntry {
	return &RunQueueEntry{
		ID:              entry.ID,
		SessionID:       entry.SessionID,
		CallData:        entry.CallData,
		Status:          entry.Status,
		LeasedBy:        entry.LeasedBy.String,
		LeasedAt:        entry.LeasedAt.Int64,
		LeaseExpiresAt:  entry.LeaseExpiresAt.Int64,
		Attempts:        entry.Attempts,
		LastError:       entry.LastError.String,
		TerminalFailure: entry.TerminalFailure == 1,
		CreatedAt:       entry.CreatedAt,
		UpdatedAt:       entry.UpdatedAt,
	}
}

func dbSliceToRunQueueEntries(entries []db.SessionRunQueue) []RunQueueEntry {
	result := make([]RunQueueEntry, len(entries))
	for i, e := range entries {
		if converted := dbToRunQueueEntry(e); converted != nil {
			result[i] = *converted
		}
	}
	return result
}
