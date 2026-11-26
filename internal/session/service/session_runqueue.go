package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/service/components/finqueue"
	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

// Durable run queue (session_run_queue) for orphaned/detached agent
// calls: entry and payload types, enqueue, lease/renew/ack/nack
// lifecycle, stale-lease recovery, and the DB-to-domain converters.

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

	FinalizationLease *finqueue.Lease
}

// RunAllowlistSpec is a JSON-serializable mirror of
// permission.RunAllowlistSpec (the uncompiled restricted-run policy). It
// exists for the same reason ModelCfg mirrors config.SelectedModel: the
// session package cannot import internal/permission (session → permission
// → shell → session would be an import cycle). The agent package converts
// between the two at the durable-serialization boundary
// (call_data_conversion.go); keep the fields in sync with
// permission.RunAllowlistSpec.
type RunAllowlistSpec = sesstypes.RunAllowlistSpec

// FileOp is the session-mirror of permission.FileOp: one filesystem
// operation a folder scope can grant. The string values are identical to
// the permission package's constants; the type is duplicated for the
// same reason the rest of this mirror exists (session cannot import
// permission).
type FileOp = sesstypes.FileOp

// FolderScopeEntry is the session-mirror of permission.FolderScopeEntry:
// one directory subtree plus the operations granted inside it. Dir is
// absolute or relative to the spec's WorkingDir. Ops empty means a deny
// carve-out, exactly as in the permission package.
type FolderScopeEntry = sesstypes.FolderScopeEntry

// FolderScopeSpec is a JSON-serializable mirror of
// permission.FolderScopeSpec (the uncompiled folder-scope set). It
// exists for the same reason RunAllowlistSpec above mirrors
// permission.RunAllowlistSpec: the session package cannot import
// internal/permission (session → permission → shell → session would be
// an import cycle). The agent package converts between the two at the
// durable-serialization boundary (call_data_conversion.go); keep the
// fields in sync with permission.FolderScopeSpec.
type FolderScopeSpec = sesstypes.FolderScopeSpec

// CallOptionsSpecVersion is the current schema version. Version 1 predates
// WebTurnOptIn, version 2 adds it, and version 3 adds the fail-closed
// custom-role contract snapshot.
const CallOptionsSpecVersion = sesstypes.CallOptionsSpecVersion

// CallOptionsSpec is a JSON-serializable mirror of the replay-relevant
// subset of agent.CallOptions (R5-3, P0 security review). It exists for
// the same reason RunAllowlistSpec/FolderScopeSpec above mirror their
// agent/permission-side originals: the session package cannot import
// internal/agent (agent already imports session in
// call_data_conversion.go, so the reverse import would cycle). The agent
// package converts between the two at the durable-serialization boundary
// (call_data_conversion.go); keep the fields in sync with
// agent.CallOptions.
//
// Deliberately NOT a full mirror of agent.CallOptions:
//   - MaxCost/MaxTokens are already persisted directly on
//     SessionAgentCallData below (not routed through CallOptions) and
//     round-trip correctly today.
//   - FolderScope has its own dedicated spec (FolderScopeSpec above); it is
//     compiled through tools.CanonicalizeFolderScopeSpec and
//     permission.BuildFolderScope, and kept separate rather than folded
//     in here.
//   - AllowPeakHours and FailIfSessionBusy are one-shot admission-time
//     decisions consumed before/at the original mailbox submission; they
//     have no meaning for a call being replayed after the fact.
//   - DiskProvider has no serializable form at all, and a call carrying
//     one is refused before it ever reaches the durable queue (see
//     HostDiskProvider below).
type CallOptionsSpec = sesstypes.CallOptionsSpec

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
	LogicalCallID        string
	MaxOutputTokens      int64
	NonInteractive       bool
	RequestedModelRole   string `json:"requested_model_role,omitempty"`
	RequestedModelSource string `json:"requested_model_source,omitempty"`
	// Declaration only; executable authority requires trusted queue provenance.
	FinalizationRequirement *AgentFinalizationRequirement `json:"finalization_requirement,omitempty"`
	// RunAllowlistSpec carries the caller's declared restricted-run policy
	// SPEC (the uncompiled mirror of permission.RunAllowlistSpec), so a
	// call rebuilt from this row by the run-queue pump can recompile and
	// re-arm its own policy instead of falling back to a session-wide or
	// process-wide gate. The compiled matcher itself is NOT serialized —
	// it holds compiled regexps and is process-local. Pointer, so "no
	// policy declared" (rows persisted before this field existed, and
	// calls queued by non-ExecuteRun callers) stays distinguishable from
	// "policy present but inert" (Restrict: false). nil spec must never be
	// read as "unrestricted on purpose"; see RebuildSessionAgentCall for
	// how a nil spec is judged.
	RunAllowlistSpec *RunAllowlistSpec
	// FolderScopeSpec carries the caller's declared folder-scope SPEC
	// (the uncompiled mirror of permission.FolderScopeSpec), so a call
	// rebuilt from this row by the run-queue pump can recompile the
	// scope and rebind its scoped filesystem toolset instead of
	// silently restarting with the shared unscoped toolset. The
	// compiled matcher itself is NOT serialized — it is process-local.
	// Pointer, so "no scope declared" (rows persisted before this
	// field existed, and unscoped calls — which are the overwhelming
	// majority) stays distinguishable from "scope present but empty"
	// (an empty Entries list compiles to a deny-everything scope, the
	// fail-closed direction). nil means unscoped or legacy row; see
	// RebuildSessionAgentCall for how a nil spec is judged.
	FolderScopeSpec *FolderScopeSpec
	// CallOptionsSpec carries the caller's declared replay-relevant
	// agent.CallOptions execution policy — DisableSubAgents, ModelRole,
	// and the timeout-watchdog policy — so a call rebuilt from this row
	// by the run-queue pump reconstructs ALL of them together, not just
	// the folder scope (R5-3, P0 security review: RebuildSessionAgentCall
	// used to construct the rebuilt CallOptions with ONLY FolderScope
	// set, silently dropping DisableSubAgents/ModelRole/timeout policy on
	// every durable restart — most seriously, a call originally declared
	// single-agent could silently regain its delegation tools after a
	// restart). nil means "the originating call carried no CallOptions"
	// (legacy in-process callers, and durable rows persisted before this
	// field existed) — such rows keep the historical fallback to the
	// shared toolset/watchdog policy, exactly as before this fix.
	CallOptionsSpec *CallOptionsSpec
	RoleContract    *RoleContract `json:"role_contract,omitempty"`
	// HostDiskProvider records that the call this row was built from
	// carried a caller-supplied filesystem (agent.CallOptions.DiskProvider).
	// The provider itself is arbitrary in-process Go code (typically a
	// closure over host state: an open DB handle, an HTTP client, an
	// in-memory map) and cannot be serialized in any form, so a row with
	// this flag set must NEVER be executed: rebuilding it would silently
	// restart the turn on the REAL disk instead of the host's, replaying
	// the model's writes onto the operator's filesystem. Producers
	// (agent.restartOrphanedWithRetry, coordinator.startDetachedRun,
	// coordinator.handleInterruptTick) refuse to enqueue such a call at
	// all; this flag exists so that a producer added later, or a row
	// written by an older/newer binary, still fails CLOSED at the
	// consumer (agent.RebuildSessionAgentCall) instead of failing open.
	HostDiskProvider bool
	// DiskRequirement is the call's process-bound execution requirement
	// (R12-H2b): the opaque binding identity and policy baseline every
	// continuation must restore (same process, binding still live) or
	// fail closed on (restart, binding gone). nil for ordinary OS-disk
	// calls. Kept alongside HostDiskProvider: the flag marks "cannot
	// replay", the envelope names WHAT must be restored.
	DiskRequirement *SessionExecutionRequirement `json:"disk_requirement,omitempty"`
	// SystemPromptOverride, if non-empty, replaces the agent's global system prompt
	SystemPromptOverride string
	// MaxCost aborts the run if total session cost exceeds this value (0 = no cap)
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this value
	MaxTokens int64
	// ExistingMessageID, when non-empty, marks this call as referencing a
	// user message that already exists in the DB (created by another process)
	ExistingMessageID string
	// Origin marks the entry channel the original request arrived
	// through; preserved across the durable run queue so a call queued
	// by one process (e.g. the CLI) still stamps its origin when
	// drained by another.
	Origin              message.Origin
	AutoResumed         bool
	BackgroundJobNotice bool
	// InjectID, when non-empty, is the ID of a pending_injects row that
	// must be deleted AFTER successful OS lock acquisition (P0-2 fix)
	InjectID string
	// Model configuration overrides (task #265, P0-1) - we serialize ModelCfg
	// because it's the per-session pinned snapshot. The actual Model (with
	// live fantasy.LanguageModel and CatwalkCfg) is reconstructed by the
	// coordinator during pump execution (ROUND 3).
	// Pointers, so "explicitly set" is distinguishable from "zero value"
	SmartModel *ModelCfg
	FastModel  *ModelCfg
	// PersistSmartModel/PersistFastModel carry explicit ExecuteRun model-slot
	// requests through durable replay. Nil preserves the slot on admission.
	PersistSmartModel  *ModelSlotUpdate `json:"persist_smart_model,omitempty"`
	PersistFastModel   *ModelSlotUpdate `json:"persist_fast_model,omitempty"`
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
// already does. Finalization-bound rows must retry with the exact recorded
// payload, and a key already owned by another session is refused.
func (s *service) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	return s.enqueueFinalizationAware(ctx, idempotencyKey, sessionID, callData)
}

// CeilUnixSeconds converts t to a whole-Unix-seconds timestamp, rounding UP
// (ceiling) rather than truncating down. Used for lease_expires_at (an
// INTEGER/whole-seconds DB column): rounding up guarantees the persisted
// deadline is never earlier than the true, sub-second-precision intended
// deadline (only ever up to ~1s later), which is the safe direction for a
// "how long is this lease still valid" column — see the P0-3 fix note at
// LeaseRunQueueEntry's call site for the full rationale.
func CeilUnixSeconds(t time.Time) int64 {
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
	tx, err := s.DB.BeginTx(ctx, nil)
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
	leaseExpiresAt := CeilUnixSeconds(acquiredAt.Add(leaseTTL))

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

	entry := dbToRunQueueEntry(leased)
	if err := s.bindFinalizationQueueLeaseTx(ctx, tx, entry); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return entry, nil
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

// ListPendingRunQueueEntries returns the pending entries across all sessions
// this process owns -- the pump's work list. Used by the pump to scan for
// work.
//
// WS-1 (#1142 step C): the scan is filtered by ownership (the SQL joins
// sessions and applies the same predicate as Owns), so a foreign session's
// durable row is never leased here, its attempts never grow, and its owner
// process picks it up instead.
func (s *service) ListPendingRunQueueEntries(ctx context.Context) ([]RunQueueEntry, error) {
	workspaceRoot, home := s.ownsArgs()
	rows, err := s.q.ListPendingRunQueueEntries(ctx, db.ListPendingRunQueueEntriesParams{
		WorkspaceRoot: workspaceRoot,
		Home:          home,
	})
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

// HasOutstandingRunQueueEntriesForSession reports whether sessionID has any
// durable run-queue row left, in EITHER status (pending or leased) -- see
// the Service interface for the full rationale (task #610).
func (s *service) HasOutstandingRunQueueEntriesForSession(ctx context.Context, sessionID string) (bool, error) {
	has, err := s.q.HasOutstandingRunQueueEntryForSession(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return has != 0, nil
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

// Orphan call outbox (orphan_call_outbox, P0-3): durable fallback for
// calls that could not reach the main run queue, drained back into it
// atomically by whichever pump wins the race.

// WriteToOrphanOutbox durably records a call that could not be enqueued to the
// main run queue (P0-3 fix). This provides a minimal durability guarantee:
// even when the main run queue is unavailable, we have a durable record that
// can be recovered later. Returns error if even the outbox write fails.
func (s *service) WriteToOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) (retErr error) {
	if id == "" {
		id = uuid.NewString()
	}
	payload, err := s.prepareFinalizationIngress(ctx, sessionID, callData)
	if err != nil {
		return err
	}
	if !payload.bound {
		return s.writeUnboundOrphanOutbox(ctx, id, sessionID, callData)
	}
	if handled, err := s.existingBoundOutboxWrite(ctx, id, sessionID, payload); handled || err != nil {
		return err
	}
	release, admission, err := AdmitFinalizationMutation(ctx, s.DB, sessionID)
	if release != nil {
		defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	ancestryHash, err := finalizationIngressAncestryHash(payload, admission)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateFreshFinalizationIngressTx(ctx, tx, sessionID, id, payload, ancestryHash, admission); err != nil {
		return err
	}
	if _, _, exists, err := finalizationQueueRowTx(ctx, tx, id); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	if exists, err := finalizationOutboxExistsTx(ctx, tx, id); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	now := time.Now().Unix()
	_, err = s.q.WithTx(tx).WriteToOrphanOutbox(ctx, db.WriteToOrphanOutboxParams{
		ID: id, SessionID: sessionID, CallData: string(callData), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	if err := validateFreshFinalizationIngressTx(ctx, tx, sessionID, id, payload, ancestryHash, admission); err != nil {
		return err
	}
	if err := persistFinalizationIngressTx(ctx, tx, id, sessionID, payload, ancestryHash); err != nil {
		return err
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, sessionID, id, string(callData), payload); err != nil {
		return err
	}
	return tx.Commit()
}

// writeUnboundOrphanOutbox keeps the historical plain insert (a duplicate id
// is still the writer's own error); the only addition is the fence refusing
// an owner whose ancestry gained a finalization scope.
func (s *service) writeUnboundOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	_, err = s.q.WithTx(tx).WriteToOrphanOutbox(ctx, db.WriteToOrphanOutboxParams{
		ID:        id,
		SessionID: sessionID,
		CallData:  string(callData),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// existingBoundOutboxWrite makes a retry of a bound outbox write idempotent
// only for the exact recorded provenance; anything else is a lost fence.
func (s *service) existingBoundOutboxWrite(ctx context.Context, id, owner string, payload finalizationIngressPayload) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	entry, err := s.q.WithTx(tx).GetOrphanOutboxEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if entry.SessionID != owner || entry.Status != "pending" {
		return true, ErrAgentFinalizationChanged
	}
	if _, _, queueExists, err := finalizationQueueRowTx(ctx, tx, id); err != nil {
		return true, err
	} else if queueExists {
		return true, ErrAgentFinalizationChanged
	}
	if exists, err := finalizationContinuationExistsTx(ctx, tx, id); err != nil {
		return true, err
	} else if !exists {
		return true, ErrAgentFinalizationChanged
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, owner, id, entry.CallData, payload); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

// ListPendingOrphanOutboxEntries returns all pending outbox entries for
// recovery (scanned by pump).
func (s *service) ListPendingOrphanOutboxEntries(ctx context.Context) ([]db.OrphanCallOutbox, error) {
	return s.q.ListPendingOrphanOutboxEntries(ctx)
}

// GetOrphanOutboxEntry retrieves a single outbox entry by ID.
func (s *service) GetOrphanOutboxEntry(ctx context.Context, id string) (*db.OrphanCallOutbox, error) {
	entry, err := s.q.GetOrphanOutboxEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &entry, err
}

// DrainOrphanOutboxEntry atomically moves an entry from orphan_call_outbox to
// session_run_queue in a single database transaction. This eliminates the
// vulnerable 'processing' intermediate state that could leave entries stranded
// after process crashes (P0-4 fix).
//
// The operation is idempotent and safe for concurrent pump instances:
// 1. Check if entry exists and is still pending
// 2. INSERT into session_run_queue (idempotent via ON CONFLICT DO NOTHING)
// 3. DELETE from orphan_call_outbox (only if status='pending')
// 4. Both operations happen in one transaction, so there's no window for crashes
//
// Returns true if the entry was successfully drained (moved to main queue),
// false if the entry was already drained by another pump or not in pending state.
func (s *service) DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// First, check if the entry exists and is still pending
	entry, err := qtx.GetOrphanOutboxEntry(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Entry doesn't exist - already drained
			return false, nil
		}
		return false, err
	}

	if entry.Status != "pending" {
		// Entry is not pending - already claimed/processed by another pump
		return false, nil
	}

	payload, err := s.prepareStoredFinalizationIngress(entry.SessionID, []byte(entry.CallData))
	if err != nil {
		return false, err
	}
	if payload.bound {
		if _, err := validateFinalizationIngressTx(ctx, tx, entry.SessionID, entry.ID, entry.CallData, payload); err != nil {
			return false, err
		}
	} else if exists, err := finalizationContinuationExistsTx(ctx, tx, entry.ID); err != nil {
		return false, err
	} else if exists {
		return false, ErrAgentFinalizationChanged
	}

	now := time.Now().Unix()

	// Insert into session_run_queue (idempotent via ON CONFLICT DO NOTHING)
	_, err = qtx.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID:        entry.ID,
		SessionID: entry.SessionID,
		CallData:  entry.CallData,
		CreatedAt: entry.CreatedAt,
		UpdatedAt: now,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// sql.ErrNoRows means it was already inserted (idempotent success)
		// Any other error is a real failure
		return false, fmt.Errorf("enqueueing to main queue: %w", err)
	}

	// The moved row must be exactly the recorded one: a bound row keeps its
	// provenance, an unbound row only refuses a newly bound owner scope.
	if payload.bound {
		if ok, err := verifyQueueCollisionTx(ctx, tx, entry.ID, entry.SessionID, payload); err != nil {
			return false, err
		} else if !ok {
			return false, ErrAgentFinalizationChanged
		}
	} else if err := ValidateBatchFinalizationTx(ctx, tx, []string{entry.SessionID}, nil); err != nil {
		return false, err
	}

	// Delete from orphan_call_outbox (only if still pending)
	rowsDeleted, err := qtx.DeleteOrphanOutboxEntryIfPending(ctx, entry.ID)
	if err != nil {
		return false, fmt.Errorf("deleting from orphan outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing transaction: %w", err)
	}

	// Return whether we actually deleted a row (true = we drained it, false = another pump did)
	return rowsDeleted > 0, nil
}

// OrphanOutboxFailureOutcome reports what one failed drain attempt did to an
// entry's retry budget.
type OrphanOutboxFailureOutcome struct {
	// Attempts is the entry's failure count after this attempt.
	Attempts int64
	// MaxAttempts is the budget it is counted against.
	MaxAttempts int64
	// Quarantined is true when this attempt exhausted the budget and moved
	// the entry to the terminal 'failed' state, so it will no longer be
	// scanned by ListPendingOrphanOutboxEntries.
	Quarantined bool
	// AlreadyTerminal is true when the row was not in 'pending' state at
	// all -- someone else quarantined or drained it first. Nothing was
	// counted; there is nothing for the caller to do.
	AlreadyTerminal bool
}

// RecordOrphanOutboxFailure counts one failed drain attempt against an entry
// and quarantines it once it has used up max_attempts.
//
// Deliberately NOT part of DrainOrphanOutboxEntry's transaction. That
// transaction is an atomic insert-to-main-queue plus delete-from-outbox and
// must stay that way; this runs only after it has already failed and rolled
// back. The point of the 2026-08-18 review finding was that dropping the old
// claim/mark-failed model also dropped the retry budget, so a genuinely
// malformed row was logged and retried every 15 seconds forever. This
// restores the budget without restoring the claim.
//
// It takes no ownership: nothing waits on this write, and a crash between the
// failed drain and this call simply means the attempt was not counted.
func (s *service) RecordOrphanOutboxFailure(ctx context.Context, id, lastError string) (OrphanOutboxFailureOutcome, error) {
	row, err := s.q.RecordOrphanOutboxFailure(ctx, db.RecordOrphanOutboxFailureParams{
		ID:        id,
		LastError: sql.NullString{String: lastError, Valid: lastError != ""},
		UpdatedAt: time.Now().Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// The UPDATE is scoped to status = 'pending', so no rows means the
		// entry was drained or quarantined by someone else in the meantime.
		// Not an error: there is simply nothing left to count.
		return OrphanOutboxFailureOutcome{AlreadyTerminal: true}, nil
	}
	if err != nil {
		return OrphanOutboxFailureOutcome{}, err
	}
	return OrphanOutboxFailureOutcome{
		Attempts:    row.Attempts,
		MaxAttempts: row.MaxAttempts,
		Quarantined: row.Status != "pending",
	}, nil
}
