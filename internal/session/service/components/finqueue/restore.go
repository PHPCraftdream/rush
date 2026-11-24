package finqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type leasedCall struct {
	owner string
	raw   string
}

func readLeasedCall(ctx context.Context, tx *sql.Tx, rowID, owner, leasedStatus string, proof *Lease, changed func() error) (leasedCall, error) {
	var call leasedCall
	var actualOwner, status string
	var leasedBy sql.NullString
	var leasedAt, expires sql.NullInt64
	var attempts int64
	err := tx.QueryRowContext(ctx, `SELECT session_id, call_data, status, leased_by, leased_at, lease_expires_at, attempts
		FROM session_run_queue WHERE id=?`, rowID).Scan(&actualOwner, &call.raw, &status, &leasedBy, &leasedAt, &expires, &attempts)
	if err != nil {
		return leasedCall{}, err
	}
	if proof == nil || actualOwner != owner || status != leasedStatus || !leasedBy.Valid || leasedBy.String != proof.LeasedBy() ||
		!leasedAt.Valid || leasedAt.Int64 != proof.LeasedAt() || attempts != proof.Attempts() || !expires.Valid || expires.Int64 <= time.Now().Unix() {
		return leasedCall{}, changed()
	}
	call.owner = actualOwner
	return call, nil
}

// RestorePort contains the narrow service-owned operations required to restore
// a trusted continuation without coupling finqueue to service types.
type RestorePort[TData, TContext, TAncestry any] struct {
	HasRequirement func(TData) bool
	SessionID      func(TData) string
	LogicalCallID  func(TData) string
	Canonical      func(TData) (string, error)
	PayloadHash    func(TData) (string, error)
	Decode         func([]byte) (TData, error)
	ValidateLegacy func(context.Context, *sql.Tx, string) error
	Lease          func(context.Context) *Lease
	LeaseChanged   func() error
	Changed        error
	LeasedStatus   string
	Ancestry       func(context.Context, *sql.Tx, string) (TAncestry, string, error)
	BuildSnapshot  func(context.Context, *sql.Tx, []byte, TAncestry) (TContext, error)
	Consume        func(*sql.Tx, string, string) (sql.Result, error)
	RequireRows    func(int64, error) error
}

// Restore consumes a continuation only after its payload, lease, ancestry, and
// identity provenance have been verified in the same transaction.
func Restore[TData, TContext, TAncestry any](ctx context.Context, db *sql.DB, rowID string, data TData, consume bool, port RestorePort[TData, TContext, TAncestry]) (TContext, error) {
	var zero TContext
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var owner, logical, payloadHash, identities, ancestryHash, token string
	var consumed int64
	err = tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, lease_token, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, rowID).Scan(&owner, &logical, &payloadHash, &identities, &ancestryHash, &token, &consumed)
	if errors.Is(err, sql.ErrNoRows) && !port.HasRequirement(data) {
		var queuedOwner string
		queueErr := tx.QueryRowContext(ctx, `SELECT session_id FROM session_run_queue WHERE id=?`, rowID).Scan(&queuedOwner)
		if queueErr == nil && queuedOwner != port.SessionID(data) {
			return zero, port.Changed
		}
		if queueErr != nil && !errors.Is(queueErr, sql.ErrNoRows) {
			return zero, queueErr
		}
		if port.SessionID(data) == "" {
			return zero, nil
		}
		// Legacy calls remain legacy only while their entire managed ancestry
		// is truly unbound. A missing declaration cannot downgrade a scope.
		if err := port.ValidateLegacy(ctx, tx, port.SessionID(data)); err != nil {
			return zero, err
		}
		return zero, nil
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return zero, port.Changed
		}
		return zero, err
	}
	declaration, err := port.Canonical(data)
	if err != nil || identities != declaration || owner != port.SessionID(data) || logical != port.LogicalCallID(data) || consumed != 0 {
		return zero, port.Changed
	}
	expectedHash, err := port.PayloadHash(data)
	if err != nil {
		return zero, err
	}
	if payloadHash != expectedHash {
		return zero, port.Changed
	}
	proof := port.Lease(ctx)
	if proof == nil || proof.Writer() != db || proof.RowID() != rowID || proof.Token == "" {
		return zero, port.Changed
	}
	if proof.Token != token {
		return zero, port.LeaseChanged()
	}
	call, err := readLeasedCall(ctx, tx, rowID, owner, port.LeasedStatus, proof, port.LeaseChanged)
	if err != nil {
		return zero, err
	}
	actual, err := port.Decode([]byte(call.raw))
	if err != nil {
		return zero, err
	}
	actualHash, err := port.PayloadHash(actual)
	if err != nil {
		return zero, err
	}
	if actualHash != payloadHash {
		return zero, port.Changed
	}
	ancestry, actualAncestryHash, err := port.Ancestry(ctx, tx, owner)
	if err != nil {
		return zero, err
	}
	if actualAncestryHash != ancestryHash {
		return zero, port.Changed
	}
	var stored json.RawMessage = []byte(identities)
	snapshot, err := port.BuildSnapshot(ctx, tx, stored, ancestry)
	if err != nil {
		return zero, err
	}
	if !consume {
		return snapshot, nil
	}
	result, err := port.Consume(tx, rowID, proof.Token)
	if err != nil {
		return zero, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return zero, err
	}
	if err := port.RequireRows(rows, err); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return snapshot, nil
}
