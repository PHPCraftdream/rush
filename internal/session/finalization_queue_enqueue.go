package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

func (s *service) enqueueFinalizationAware(ctx context.Context, rowID, owner string, raw []byte) (retErr error) {
	payload, err := s.prepareFinalizationIngress(ctx, owner, raw)
	if err != nil {
		return err
	}
	if handled, err := s.existingFinalizationQueueEnqueue(ctx, rowID, owner, string(raw), payload); handled || err != nil {
		return err
	}
	release, admission, err := admitFinalizationMutation(ctx, s.db, owner)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	ancestryHash, err := finalizationIngressAncestryHash(payload, admission)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateFreshFinalizationIngressTx(ctx, tx, owner, rowID, payload, ancestryHash, admission); err != nil {
		return err
	}
	if exists, err := finalizationOutboxExistsTx(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	if exists, err := finalizationContinuationExistsTx(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	if _, _, exists, err := finalizationQueueRowTx(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	now := time.Now().Unix()
	_, err = s.q.WithTx(tx).EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID: rowID, SessionID: owner, CallData: string(raw), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	actualOwner, actualRaw, exists, err := finalizationQueueRowTx(ctx, tx, rowID)
	if err != nil {
		return err
	}
	if !exists || actualOwner != owner || actualRaw != string(raw) {
		return ErrAgentFinalizationChanged
	}
	if err := validateFreshFinalizationIngressTx(ctx, tx, owner, rowID, payload, ancestryHash, admission); err != nil {
		return err
	}
	if err := persistFinalizationIngressTx(ctx, tx, rowID, owner, payload, ancestryHash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *service) existingFinalizationQueueEnqueue(ctx context.Context, rowID, owner, raw string, payload finalizationIngressPayload) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	queueOwner, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, rowID)
	if err != nil || !exists {
		return false, err
	}
	if queueOwner != owner {
		return true, ErrAgentFinalizationChanged
	}
	if exists, err := finalizationOutboxExistsTx(ctx, tx, rowID); err != nil {
		return true, err
	} else if exists {
		return true, ErrAgentFinalizationChanged
	}
	if !payload.bound && existingRaw != raw {
		if _, err := validateFinalizationIngressTx(ctx, tx, owner, rowID, existingRaw, payload); err != nil {
			return true, err
		}
		return true, tx.Commit()
	}
	if _, err := verifyQueueCollisionTx(ctx, tx, rowID, owner, payload); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

func finalizationContinuationExistsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_continuations WHERE row_id=?)`, id).Scan(&exists)
	return exists, err
}
