// Durable session credential marker: records that a session encountered
// credentials, without persisting any credential material.
package requirements

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
)

const (
	SessionCredentialRequirementKind    = "session_credential_requirement"
	SessionCredentialRequirementVersion = 1
)

// SessionCredentialRequirementStore is an optional capability for persisting
// whether a session requires credentials. Marker payloads contain no secrets.
type SessionCredentialRequirementStore interface {
	StoreSessionCredentialRequirement(ctx context.Context, sessionID string) error
	HasSessionCredentialRequirement(ctx context.Context, sessionID string) (bool, error)
}

type sessionCredentialRequirementEnvelope struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}

// StoreSessionCredentialRequirement persists a secret-free, terminal metadata row.
func StoreSessionCredentialRequirement(ctx context.Context, database *sql.DB, sessionID string) error {
	payload, err := json.Marshal(sessionCredentialRequirementEnvelope{
		Kind: SessionCredentialRequirementKind, Version: SessionCredentialRequirementVersion,
	})
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = database.ExecContext(ctx,
		`INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, terminal_failure, created_at, updated_at)
		 VALUES (?, ?, ?, 'acked', 0, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET call_data = excluded.call_data, updated_at = excluded.updated_at`,
		sesstypes.SessionCredentialMarkerRowID(sessionID), sessionID, string(payload), now, now)
	return err
}

// HasSessionCredentialRequirement reports whether the marker exists; malformed
// present data is an error rather than an absent marker.
func HasSessionCredentialRequirement(ctx context.Context, database *sql.DB, sessionID string) (bool, error) {
	var payload string
	err := database.QueryRowContext(ctx,
		`SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		sesstypes.SessionCredentialMarkerRowID(sessionID), sessionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var envelope sessionCredentialRequirementEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return false, err
	}
	if envelope.Kind != SessionCredentialRequirementKind || envelope.Version != SessionCredentialRequirementVersion {
		return false, errors.New("session: malformed credential requirement marker")
	}
	return true, nil
}

// IsSessionCredentialRequirementCallData recognizes the non-executable marker.
func IsSessionCredentialRequirementCallData(callData string) bool {
	var probe struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal([]byte(callData), &probe) != nil {
		return false
	}
	return probe.Kind == SessionCredentialRequirementKind
}
