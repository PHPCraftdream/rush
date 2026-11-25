// Durable session execution fence (R12-H2b, design 2.1): the
// provider-bound execution requirement of one session, persisted as a
// single non-executable metadata row in session_run_queue. H2 interim
// adapter only — H4 replaces the storage with a
// sessions.execution_requirement column behind the same
// SessionExecutionRequirementStore interface.

package requirements

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
)

// SessionExecutionRequirementKind is the envelope kind stamped on every
// persisted fence row. The pump guard and the loader both match on it so
// the fence can never be mistaken for a serialized session call.
const SessionExecutionRequirementKind = "session_execution_requirement"

// SessionExecutionRequirementVersion is the schema version of the
// envelope. The loader fails closed on any other version rather than
// guessing at an unknown field set.
const SessionExecutionRequirementVersion = 1

type SessionExecutionRequirement = sesstypes.SessionExecutionRequirement

// sessionExecutionRequirementEnvelope is the wire form persisted in the
// row's call_data column: kind and version wrap the requirement so a
// future schema change is detectable at load time instead of silently
// misdecoded.
type sessionExecutionRequirementEnvelope struct {
	Kind        string                                `json:"kind"`
	Version     int                                   `json:"version"`
	Requirement sesstypes.SessionExecutionRequirement `json:"requirement"`
}

// ErrSessionExecutionRequirementMalformed is returned when a stored fence
// payload cannot be trusted. The loader fails closed: an unreadable
// requirement must block execution, never fall through to a zero value
// that could look like "no binding".
var ErrSessionExecutionRequirementMalformed = errors.New("session: stored session execution requirement is malformed")

// SessionExecutionRequirementStore is the storage seam for the durable
// execution fence. The H2 interim adapter stores the fence as one
// non-executable service row in session_run_queue; H4 replaces the
// implementation with a sessions.execution_requirement column behind this
// same interface.
type SessionExecutionRequirementStore interface {
	StoreSessionExecutionRequirement(ctx context.Context, sessionID string, req sesstypes.SessionExecutionRequirement) error
	LoadSessionExecutionRequirement(ctx context.Context, sessionID string) (*sesstypes.SessionExecutionRequirement, error)
	// DeleteSessionExecutionRequirement removes sessionID's fence. An
	// explicit OS-provider run supersedes the session's fence (R12-H2b
	// new-epoch semantics); deleting an absent fence is a no-op.
	DeleteSessionExecutionRequirement(ctx context.Context, sessionID string) error
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
func StoreSessionExecutionRequirement(ctx context.Context, database *sql.DB, sessionID string, req sesstypes.SessionExecutionRequirement) error {
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
	_, err = database.ExecContext(ctx,
		`INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, terminal_failure, created_at, updated_at)
		 VALUES (?, ?, ?, 'acked', 0, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET call_data = excluded.call_data, updated_at = excluded.updated_at`,
		sesstypes.SessionExecutionRequirementRowID(sessionID), sessionID, string(payload), now, now,
	)
	return err
}

// DeleteSessionExecutionRequirement removes sessionID's fence row. A
// missing fence is a no-op (nil): the explicit OS-provider epoch the
// caller is establishing may legally follow an unfenced session.
func DeleteSessionExecutionRequirement(ctx context.Context, database *sql.DB, sessionID string) error {
	_, err := database.ExecContext(ctx,
		`DELETE FROM session_run_queue WHERE id = ? AND session_id = ?`,
		sesstypes.SessionExecutionRequirementRowID(sessionID), sessionID,
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
func LoadSessionExecutionRequirement(ctx context.Context, database *sql.DB, sessionID string) (*sesstypes.SessionExecutionRequirement, error) {
	var payload string
	err := database.QueryRowContext(ctx,
		`SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		sesstypes.SessionExecutionRequirementRowID(sessionID), sessionID,
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
