package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

var ErrFinalizationScopeChanged = errors.New("finalization_scope_changed")

// publishFinalizationCandidate runs only while the owner gate and session are held.
func publishFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, owner string, identity message.FinalizationIdentity, candidate finalizationCandidate) (message.Message, error) {
	writer, supported := sessions.(session.FinalizationTransaction)
	if !supported {
		return message.Message{}, fmt.Errorf("%w: finalization transaction unavailable", ErrFinalizationFactsUnavailable)
	}
	reader, supported := messages.(message.FinalizationReader)
	if !supported {
		return message.Message{}, fmt.Errorf("%w: transactional candidate reader unavailable", ErrFinalizationFactsUnavailable)
	}
	var accepted message.Message
	var postCommit func()
	err := writer.WithFinalizationTx(ctx, owner, func(tx *sql.Tx) error {
		current, err := reader.GetFinalizationCandidateTx(ctx, tx, candidate.messageID)
		if err != nil {
			return err
		}
		if current.SessionID != owner {
			return message.ErrFinalizationCandidateChanged
		}
		err = current.AcceptFinalizationCandidate(identity, candidate.hash)
		if errors.Is(err, message.ErrFinalizationAlreadyAccepted) {
			accepted = current
			return nil // A verified receipt is not another terminal publication.
		}
		if err != nil {
			return err
		}
		postCommit, err = messages.UpdateTx(ctx, tx, current)
		if err != nil {
			return err
		}
		accepted = current
		return nil
	})
	if err != nil {
		if errors.Is(err, message.ErrFinalizationCandidateChanged) {
			return message.Message{}, fmt.Errorf("%w: %w", ErrFinalizationScopeChanged, err)
		}
		return message.Message{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return accepted, nil
}
