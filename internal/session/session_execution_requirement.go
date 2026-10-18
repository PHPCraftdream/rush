// Durable session execution fence (R12-H2b, design 2.1): the
// provider-bound execution requirement of one session, persisted as a
// single non-executable metadata row in session_run_queue. H2 interim
// adapter only — H4 replaces the storage with a
// sessions.execution_requirement column behind the same
// SessionExecutionRequirementStore interface.

package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionExecutionRequirementKind is the envelope kind stamped on every
// persisted fence row. The pump guard and the loader both match on it so
// the fence can never be mistaken for a serialized session call.
const SessionExecutionRequirementKind = "session_execution_requirement"

// SessionExecutionRequirementVersion is the schema version of the
// envelope. The loader fails closed on any other version rather than
// guessing at an unknown field set.
const SessionExecutionRequirementVersion = 1

// SessionExecutionRequirement is the durable, serializable execution
// requirement of one session's provider-bound work (R12-H2b): the opaque
// process binding ID a continuation must resolve, plus the serializable
// policy baseline the originating invocation admitted under.
type SessionExecutionRequirement struct {
	BindingID        string            `json:"binding_id"`
	Owner            string            `json:"owner,omitempty"`
	Epoch            int64             `json:"epoch,omitempty"`
	FolderScopeSpec  *FolderScopeSpec  `json:"folder_scope_spec,omitempty"`
	RunAllowlistSpec *RunAllowlistSpec `json:"run_allowlist_spec,omitempty"`
	CallPolicy       *CallOptionsSpec  `json:"call_policy,omitempty"`
}

// sessionExecutionRequirementEnvelope is the wire form persisted in the
// row's call_data column: kind and version wrap the requirement so a
// future schema change is detectable at load time instead of silently
// misdecoded.
type sessionExecutionRequirementEnvelope struct {
	Kind        string                      `json:"kind"`
	Version     int                         `json:"version"`
	Requirement SessionExecutionRequirement `json:"requirement"`
}

// ErrSessionExecutionRequirementMalformed is returned when a stored fence
// payload cannot be trusted. The loader fails closed: an unreadable
// requirement must block execution, never fall through to a zero value
// that could look like "no binding".
var ErrSessionExecutionRequirementMalformed = errors.New("session: stored session execution requirement is malformed")

// SessionExecutionRequirementRowID returns the deterministic reserved row
// ID of the fence row for sessionID. The `:` separator cannot occur in a
// run-queue idempotency key (`<sessionID>-<uuid>` forms built with "-"),
// so the reserved row can never collide with a logical-call row.
func SessionExecutionRequirementRowID(sessionID string) string {
	return "session-execution-requirement:" + sessionID
}

// SessionExecutionRequirementStore is the storage seam for the durable
// execution fence. The H2 interim adapter stores the fence as one
// non-executable service row in session_run_queue; H4 replaces the
// implementation with a sessions.execution_requirement column behind this
// same interface.
type SessionExecutionRequirementStore interface {
	StoreSessionExecutionRequirement(ctx context.Context, sessionID string, req SessionExecutionRequirement) error
	LoadSessionExecutionRequirement(ctx context.Context, sessionID string) (*SessionExecutionRequirement, error)
}

// StoreSessionExecutionRequirement upserts the fence row for sessionID.
// The row is written with status 'acked' so it is invisible to pending
// selection and to outstanding-work checks by construction (run_queue.sql
// filters status='pending' and status IN ('pending','leased')), and with
// terminal_failure=1 so even a future reader that forgot the kind guard
// treats it as never-retryable. It is deliberately NOT a real call: the
// pump never decodes it (guarded in run_queue_entry_exec.go) and it only
// leaves the table when the session itself is deleted (FK ON DELETE
// CASCADE).
func (s *service) StoreSessionExecutionRequirement(ctx context.Context, sessionID string, req SessionExecutionRequirement) error {
	if req.BindingID == "" {
		return fmt.Errorf("%w: session execution requirement needs a binding id (session %s)",
			ErrSessionExecutionRequirementMalformed, sessionID)
	}
	payload, err := json.Marshal(sessionExecutionRequirementEnvelope{
		Kind:        SessionExecutionRequirementKind,
		Version:     SessionExecutionRequirementVersion,
		Requirement: req,
	})
	if err != nil {
		return fmt.Errorf("%w: session %s: %s", ErrSessionExecutionRequirementMalformed, sessionID, err)
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, terminal_failure, created_at, updated_at)
		 VALUES (?, ?, ?, 'acked', 0, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET call_data = excluded.call_data, updated_at = excluded.updated_at`,
		SessionExecutionRequirementRowID(sessionID), sessionID, string(payload), now, now,
	)
	return err
}

// LoadSessionExecutionRequirement returns the fence of sessionID, or
// (nil, nil) when the session has none — an absent fence is a normal
// state (sessions created before the fence existed, or never bound), not
// an error. A present-but-unreadable payload fails closed with
// ErrSessionExecutionRequirementMalformed rather than degrading to "no
// requirement", because continuing under a lost binding is exactly the
// cross-process provider confusion the fence exists to prevent.
func (s *service) LoadSessionExecutionRequirement(ctx context.Context, sessionID string) (*SessionExecutionRequirement, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		SessionExecutionRequirementRowID(sessionID), sessionID,
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var envelope sessionExecutionRequirementEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return nil, fmt.Errorf("%w: session %s: cannot decode envelope: %s",
			ErrSessionExecutionRequirementMalformed, sessionID, err)
	}
	if envelope.Kind != SessionExecutionRequirementKind {
		return nil, fmt.Errorf("%w: session %s: wrong kind %q", ErrSessionExecutionRequirementMalformed, sessionID, envelope.Kind)
	}
	if envelope.Version != SessionExecutionRequirementVersion {
		return nil, fmt.Errorf("%w: session %s: unsupported version %d", ErrSessionExecutionRequirementMalformed, sessionID, envelope.Version)
	}
	if envelope.Requirement.BindingID == "" {
		return nil, fmt.Errorf("%w: session %s: empty binding id", ErrSessionExecutionRequirementMalformed, sessionID)
	}
	req := envelope.Requirement
	return &req, nil
}

// IsSessionExecutionRequirementCallData reports whether callData is a
// fence envelope row rather than a serialized session call. Used by the
// pump to refuse executing the metadata row; malformed JSON returns false
// because the pump's own parse-error path already terminal-fails those.
func IsSessionExecutionRequirementCallData(callData string) bool {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(callData), &probe); err != nil {
		return false
	}
	return probe.Kind == SessionExecutionRequirementKind
}
