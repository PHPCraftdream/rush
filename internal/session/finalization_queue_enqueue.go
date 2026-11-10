package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

func (s *service) enqueueFinalizationAware(ctx context.Context, rowID, owner string, raw []byte) (retErr error) {
	var data SessionAgentCallData
	parseErr := json.Unmarshal(raw, &data)
	bound := data.FinalizationRequirement != nil
	if !bound && len(CaptureAgentFinalizationContext(ctx).authorities) == 0 {
		// Preserve the historical unmarked enqueue contract. Reconstruction
		// still refuses unmarked rows whose managed scope became bound.
		now := time.Now().Unix()
		_, err := s.q.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
			ID: rowID, SessionID: owner, CallData: string(raw), CreatedAt: now, UpdatedAt: now,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var identities, payloadHash string
	if bound {
		if parseErr != nil || data.SessionID != owner || data.LogicalCallID == "" {
			return ErrAgentFinalizationChanged
		}
		var err error
		identities, err = trustedFinalizationDeclaration(ctx, s, data)
		if err != nil {
			return err
		}
		payloadHash, err = finalizationPayloadHash(data)
		if err != nil {
			return err
		}
	} else if len(CaptureAgentFinalizationContext(ctx).authorities) != 0 {
		return ErrAgentFinalizationChanged
	}
	release, admission, err := admitFinalizationMutation(ctx, s.db, owner)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	var ancestryHash string
	if bound {
		if admission == nil {
			return ErrAgentFinalizationChanged
		}
		for _, identity := range data.FinalizationRequirement.Authorities {
			if _, affected := slices.BinarySearch(admission.ancestry.owners, identity.SessionID); !affected {
				return ErrAgentFinalizationChanged
			}
		}
		ancestryHash = finalizationAncestryHash(admission.ancestry)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}
	var oldHash, oldIdentities, oldAncestry string
	var consumed int64
	err = tx.QueryRowContext(ctx, `SELECT payload_hash, identities, ancestry, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, rowID).Scan(&oldHash, &oldIdentities, &oldAncestry, &consumed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if !bound || oldHash != payloadHash || oldIdentities != identities || oldAncestry != ancestryHash {
			return ErrAgentFinalizationChanged
		}
		if consumed != 0 {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_run_queue WHERE id=?)`, rowID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrAgentFinalizationChanged
			}
		}
	}
	now := time.Now().Unix()
	qtx := s.q.WithTx(tx)
	_, err = qtx.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID: rowID, SessionID: owner, CallData: string(raw), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if bound {
		var actualRaw string
		if err := tx.QueryRowContext(ctx, `SELECT call_data FROM session_run_queue WHERE id=? AND session_id=?`, rowID, owner).Scan(&actualRaw); err != nil {
			return err
		}
		var actual SessionAgentCallData
		if err := json.Unmarshal([]byte(actualRaw), &actual); err != nil {
			return err
		}
		actualHash, err := finalizationPayloadHash(actual)
		if err != nil {
			return err
		}
		if actualHash != payloadHash {
			return ErrAgentFinalizationChanged
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_agent_finalization_continuations
			(row_id, session_id, logical_call_id, payload_hash, identities, ancestry) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(row_id) DO NOTHING`, rowID, owner, data.LogicalCallID, payloadHash, identities, ancestryHash)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
