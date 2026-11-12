package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type finalizationIngressPayload struct {
	data        SessionAgentCallData
	bound       bool
	identities  string
	payloadHash string
}

func (s *service) prepareStoredFinalizationIngress(owner string, raw []byte) (finalizationIngressPayload, error) {
	var payload finalizationIngressPayload
	err := json.Unmarshal(raw, &payload.data)
	payload.bound = payload.data.FinalizationRequirement != nil
	if !payload.bound {
		return payload, nil
	}
	if err != nil || payload.data.SessionID != owner || payload.data.LogicalCallID == "" {
		return payload, ErrAgentFinalizationChanged
	}
	payload.identities, err = canonicalFinalizationRequirement(payload.data.FinalizationRequirement)
	if err != nil {
		return payload, err
	}
	payload.payloadHash, err = finalizationPayloadHash(payload.data)
	return payload, err
}

func (s *service) prepareFinalizationIngress(ctx context.Context, owner string, raw []byte) (finalizationIngressPayload, error) {
	var payload finalizationIngressPayload
	err := json.Unmarshal(raw, &payload.data)
	payload.bound = payload.data.FinalizationRequirement != nil
	if !payload.bound {
		if len(CaptureAgentFinalizationContext(ctx).authorities) != 0 {
			return payload, ErrAgentFinalizationChanged
		}
		return payload, nil
	}
	if err != nil || payload.data.SessionID != owner || payload.data.LogicalCallID == "" {
		return payload, ErrAgentFinalizationChanged
	}
	payload.identities, err = trustedFinalizationDeclaration(ctx, s, payload.data)
	if err != nil {
		return payload, err
	}
	payload.payloadHash, err = finalizationPayloadHash(payload.data)
	return payload, err
}

func validateFreshFinalizationIngressTx(ctx context.Context, tx *sql.Tx, owner, id string, payload finalizationIngressPayload, ancestryHash string, admission *batchFinalizationAdmission) error {
	if err := validateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}
	if exists, err := finalizationContinuationExistsTx(ctx, tx, id); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	if !payload.bound {
		return nil
	}
	ancestry, err := readFinalizationAncestry(ctx, tx, []string{owner})
	if err != nil {
		return err
	}
	if finalizationAncestryHash(ancestry) != ancestryHash {
		return ErrAgentFinalizationChanged
	}
	for _, identity := range payload.data.FinalizationRequirement.Authorities {
		if _, affected := searchFinalizationOwner(ancestry.owners, identity.SessionID); !affected {
			return ErrAgentFinalizationChanged
		}
	}
	return nil
}

type finalizationIngressQueryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func finalizationQueueRowTx(ctx context.Context, tx finalizationIngressQueryRow, id string) (owner, raw string, exists bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT session_id, call_data FROM session_run_queue WHERE id=?`, id).Scan(&owner, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return owner, raw, err == nil, err
}

func finalizationOutboxExistsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orphan_call_outbox WHERE id=?)`, id).Scan(&exists)
	return exists, err
}

func validateFinalizationIngressTx(ctx context.Context, tx *sql.Tx, owner, id, raw string, payload finalizationIngressPayload) (bool, error) {
	var storedOwner, logical, payloadHash, identities, ancestryHash string
	var consumed int64
	err := tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, id).Scan(&storedOwner, &logical, &payloadHash, &identities, &ancestryHash, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		if payload.bound {
			return false, ErrAgentFinalizationChanged
		}
		if err := validateBatchFinalizationTx(ctx, tx, []string{owner}, nil); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !payload.bound || storedOwner != owner || logical != payload.data.LogicalCallID || payloadHash != payload.payloadHash || identities != payload.identities || consumed != 0 {
		return false, ErrAgentFinalizationChanged
	}
	if raw != "" {
		var actual SessionAgentCallData
		if err := json.Unmarshal([]byte(raw), &actual); err != nil {
			return false, err
		}
		actualHash, err := finalizationPayloadHash(actual)
		if err != nil {
			return false, err
		}
		declaration, err := canonicalFinalizationRequirement(actual.FinalizationRequirement)
		if err != nil || actual.SessionID != owner || actual.LogicalCallID != logical || actualHash != payloadHash || declaration != identities {
			return false, ErrAgentFinalizationChanged
		}
	}
	declaration, err := canonicalFinalizationRequirement(payload.data.FinalizationRequirement)
	if err != nil || declaration != identities {
		return false, ErrAgentFinalizationChanged
	}
	ancestry, err := readFinalizationAncestry(ctx, tx, []string{owner})
	if err != nil {
		return false, err
	}
	if finalizationAncestryHash(ancestry) != ancestryHash {
		return false, ErrAgentFinalizationChanged
	}
	var storedRequirement AgentFinalizationRequirement
	if err := json.Unmarshal([]byte(identities), &storedRequirement); err != nil {
		return false, err
	}
	byOwner := make(map[string]bool, len(storedRequirement.Authorities))
	for _, identity := range storedRequirement.Authorities {
		if _, affected := searchFinalizationOwner(ancestry.owners, identity.SessionID); !affected {
			return false, ErrAgentFinalizationChanged
		}
		record, err := readAgentFinalization(ctx, tx, identity)
		if err != nil {
			return false, err
		}
		if record.Cancelled {
			return false, context.Canceled
		}
		if !record.active() {
			return false, ErrAgentFinalizationChanged
		}
		var conflicting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations
			WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing')
			AND (logical_call_id<>? OR generation<>?))`, identity.SessionID, identity.LogicalCallID, identity.Generation).Scan(&conflicting); err != nil {
			return false, err
		}
		if conflicting {
			return false, ErrAgentFinalizationChanged
		}
		byOwner[identity.SessionID] = true
	}
	for _, affected := range ancestry.owners {
		if byOwner[affected] {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, affected).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			return false, ErrAgentFinalizationChanged
		}
	}
	return true, nil
}

func searchFinalizationOwner(owners []string, owner string) (int, bool) {
	for i, candidate := range owners {
		if candidate == owner {
			return i, true
		}
	}
	return 0, false
}

func verifyQueueCollisionTx(ctx context.Context, tx *sql.Tx, id, owner string, payload finalizationIngressPayload) (bool, error) {
	queueOwner, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, id)
	if err != nil || !exists {
		return false, err
	}
	if queueOwner != owner {
		return false, ErrAgentFinalizationChanged
	}
	if payload.bound {
		var actual SessionAgentCallData
		if err := json.Unmarshal([]byte(existingRaw), &actual); err != nil {
			return false, err
		}
		actualHash, err := finalizationPayloadHash(actual)
		if err != nil {
			return false, err
		}
		if actualHash != payload.payloadHash {
			return false, ErrAgentFinalizationChanged
		}
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, owner, id, existingRaw, payload); err != nil {
		return false, err
	}
	return true, nil
}

func persistFinalizationIngressTx(ctx context.Context, tx *sql.Tx, id, owner string, payload finalizationIngressPayload, ancestryHash string) error {
	if !payload.bound {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO session_agent_finalization_continuations
		(row_id, session_id, logical_call_id, payload_hash, identities, ancestry) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(row_id) DO NOTHING`, id, owner, payload.data.LogicalCallID, payload.payloadHash, payload.identities, ancestryHash)
	if err != nil {
		return err
	}
	var storedOwner, logical, hash, identities, ancestry string
	var consumed int64
	if err := tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, id).Scan(&storedOwner, &logical, &hash, &identities, &ancestry, &consumed); err != nil {
		return err
	}
	if storedOwner != owner || logical != payload.data.LogicalCallID || hash != payload.payloadHash || identities != payload.identities || ancestry != ancestryHash || consumed != 0 {
		return ErrAgentFinalizationChanged
	}
	return nil
}

func finalizationIngressAncestryHash(payload finalizationIngressPayload, admission *batchFinalizationAdmission) (string, error) {
	if !payload.bound {
		return "", nil
	}
	if admission == nil {
		return "", ErrAgentFinalizationChanged
	}
	for _, identity := range payload.data.FinalizationRequirement.Authorities {
		if _, affected := searchFinalizationOwner(admission.ancestry.owners, identity.SessionID); !affected {
			return "", fmt.Errorf("%w: authority outside managed ancestry", ErrAgentFinalizationChanged)
		}
	}
	return finalizationAncestryHash(admission.ancestry), nil
}

// finalizationFenceError marks an admission or validation failure as a lost
// finalization fence while keeping the cause (including context.Canceled)
// inspectable. Ordinary writer errors must not pass through it.
func finalizationFenceError(err error) error {
	if err == nil || errors.Is(err, ErrAgentFinalizationChanged) || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.Join(ErrAgentFinalizationChanged, err)
}

// finalizationScoped reports whether the caller carries finalization
// authorities, i.e. whether admission can mint revisions for this call.
func finalizationScoped(ctx context.Context) bool {
	return len(CaptureAgentFinalizationContext(ctx).authorities) != 0
}
