package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// AgentFinalizationQueueService restores only a trusted, currently leased row.
type AgentFinalizationQueueService interface {
	RestoreAgentFinalizationQueue(context.Context, string, SessionAgentCallData) (AgentFinalizationContext, error)
}

// RestoreAgentFinalizationQueue consumes a continuation before model creation.
// A crash after consumption leaves the durable obligation unresolved, not ready
// for an automatic replay. Consumed receipts survive queue-row deletion.
func (s *service) RestoreAgentFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData) (AgentFinalizationContext, error) {
	return s.restoreFinalizationQueue(ctx, rowID, data, true)
}

func (s *service) restoreFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData, consume bool) (AgentFinalizationContext, error) {
	if err := ctx.Err(); err != nil {
		return AgentFinalizationContext{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	defer tx.Rollback()
	var owner, logical, payloadHash, identities, ancestryHash, token string
	var consumed int64
	err = tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, lease_token, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, rowID).Scan(&owner, &logical, &payloadHash, &identities, &ancestryHash, &token, &consumed)
	if errors.Is(err, sql.ErrNoRows) && data.FinalizationRequirement == nil {
		var queuedOwner string
		queueErr := tx.QueryRowContext(ctx, `SELECT session_id FROM session_run_queue WHERE id=?`, rowID).Scan(&queuedOwner)
		if queueErr == nil && queuedOwner != data.SessionID {
			return AgentFinalizationContext{}, ErrAgentFinalizationChanged
		}
		if queueErr != nil && !errors.Is(queueErr, sql.ErrNoRows) {
			return AgentFinalizationContext{}, queueErr
		}
		if data.SessionID == "" {
			return AgentFinalizationContext{}, nil
		}
		// Legacy calls remain legacy only while their entire managed ancestry
		// is truly unbound. A missing declaration cannot downgrade a scope.
		if err := validateBatchFinalizationTx(ctx, tx, []string{data.SessionID}, nil); err != nil {
			return AgentFinalizationContext{}, err
		}
		return AgentFinalizationContext{}, nil
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentFinalizationContext{}, ErrAgentFinalizationChanged
		}
		return AgentFinalizationContext{}, err
	}
	declaration, err := canonicalFinalizationRequirement(data.FinalizationRequirement)
	if err != nil || identities != declaration || owner != data.SessionID || logical != data.LogicalCallID || consumed != 0 {
		return AgentFinalizationContext{}, ErrAgentFinalizationChanged
	}
	expectedHash, err := finalizationPayloadHash(data)
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	if payloadHash != expectedHash {
		return AgentFinalizationContext{}, ErrAgentFinalizationChanged
	}
	proof, _ := ctx.Value(agentFinalizationQueueLeaseKey{}).(*agentFinalizationQueueLease)
	if proof == nil || proof.writer != s.db || proof.rowID != rowID || proof.token == "" {
		return AgentFinalizationContext{}, ErrAgentFinalizationChanged
	}
	if proof.token != token {
		return AgentFinalizationContext{}, finalizationQueueLeaseChanged()
	}
	var actualOwner, raw, status string
	var leasedBy sql.NullString
	var leasedAt, expires sql.NullInt64
	var attempts int64
	err = tx.QueryRowContext(ctx, `SELECT session_id, call_data, status, leased_by, leased_at, lease_expires_at, attempts
		FROM session_run_queue WHERE id=?`, rowID).Scan(&actualOwner, &raw, &status, &leasedBy, &leasedAt, &expires, &attempts)
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	if actualOwner != owner || status != RunQueueStatusLeased || !leasedBy.Valid || leasedBy.String != proof.leasedBy ||
		!leasedAt.Valid || leasedAt.Int64 != proof.leasedAt || attempts != proof.attempts || !expires.Valid || expires.Int64 <= time.Now().Unix() {
		return AgentFinalizationContext{}, finalizationQueueLeaseChanged()
	}
	var actual SessionAgentCallData
	if err := json.Unmarshal([]byte(raw), &actual); err != nil {
		return AgentFinalizationContext{}, err
	}
	actualHash, err := finalizationPayloadHash(actual)
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	if actualHash != payloadHash {
		return AgentFinalizationContext{}, ErrAgentFinalizationChanged
	}
	ancestry, err := readFinalizationAncestry(ctx, tx, []string{owner})
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	if finalizationAncestryHash(ancestry) != ancestryHash {
		return AgentFinalizationContext{}, ErrAgentFinalizationChanged
	}
	// Construct from the trusted stored declaration only after all provenance
	// checks. Payload IDs, public row-ID context and fake leases are not proof.
	var stored AgentFinalizationRequirement
	if err := json.Unmarshal([]byte(identities), &stored); err != nil {
		return AgentFinalizationContext{}, err
	}
	snapshot := AgentFinalizationContext{managedContinuation: len(ancestry.edges) != 0}
	byOwner := make(map[string]bool, len(stored.Authorities))
	for _, identity := range stored.Authorities {
		if _, affected := slices.BinarySearch(ancestry.owners, identity.SessionID); !affected {
			return AgentFinalizationContext{}, ErrAgentFinalizationChanged
		}
		record, err := readAgentFinalization(ctx, tx, identity)
		if err != nil {
			return AgentFinalizationContext{}, err
		}
		if record.Cancelled {
			return AgentFinalizationContext{}, context.Canceled
		}
		if !record.active() {
			return AgentFinalizationContext{}, ErrAgentFinalizationChanged
		}
		byOwner[identity.SessionID] = true
		snapshot.authorities = append(snapshot.authorities, &AgentFinalizationAuthority{service: s, identity: identity})
	}
	for _, affected := range ancestry.owners {
		if byOwner[affected] {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, affected).Scan(&exists); err != nil {
			return AgentFinalizationContext{}, err
		}
		if exists {
			return AgentFinalizationContext{}, ErrAgentFinalizationChanged
		}
	}
	if !consume {
		return snapshot, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalization_continuations SET consumed=1, failure_code=''
		WHERE row_id=? AND lease_token=? AND consumed=0`, rowID, proof.token)
	if err != nil {
		return AgentFinalizationContext{}, err
	}
	if err := batchRequireRow(result.RowsAffected()); err != nil {
		return AgentFinalizationContext{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentFinalizationContext{}, err
	}
	return snapshot, nil
}

var _ AgentFinalizationQueueService = (*service)(nil)
