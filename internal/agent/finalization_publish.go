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

type finalizationPublicationScope struct {
	store    *session.AgentFinalizationStore
	identity session.AgentFinalizationIdentity
}

// matches reports whether a step identity belongs to this scope's logical
// call and generation.
func (s finalizationPublicationScope) matches(identity message.FinalizationIdentity) bool {
	return s.store != nil && identity.Key == s.identity.LogicalCallID && s.identity.Generation > 0 &&
		identity.Generation == uint64(s.identity.Generation) && identity.Revision <= uint64(1<<63-1)
}

// publishFinalizationCandidate runs only while the owner gate and session are held.
func publishFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope finalizationPublicationScope, identity message.FinalizationIdentity, candidate finalizationCandidate) (message.Message, error) {
	owner := scope.identity.SessionID
	if !scope.matches(identity) {
		return message.Message{}, ErrFinalizationScopeChanged
	}
	sealed, ok := session.CandidateOf(candidate.messageID, candidate.hash, identity)
	if !ok {
		return message.Message{}, ErrFinalizationScopeChanged
	}
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
		alreadyAccepted := errors.Is(err, message.ErrFinalizationAlreadyAccepted)
		if err != nil && !alreadyAccepted {
			return err
		}
		sealErr := scope.store.SealTx(ctx, tx, scope.identity, sealed)
		if alreadyAccepted {
			if !errors.Is(sealErr, session.ErrAgentFinalizationAccepted) {
				if sealErr != nil {
					return sealErr
				}
				return session.ErrAgentFinalizationChanged
			}
			accepted = current
			return nil // Both durable receipts must agree; do not publish again.
		}
		if sealErr != nil {
			if errors.Is(sealErr, session.ErrAgentFinalizationAccepted) {
				return session.ErrAgentFinalizationChanged
			}
			return sealErr
		}
		postCommit, err = messages.UpdateTx(ctx, tx, current)
		if err != nil {
			return err
		}
		accepted = current
		return nil
	})
	if err != nil {
		if errors.Is(err, message.ErrFinalizationCandidateChanged) || errors.Is(err, session.ErrAgentFinalizationChanged) {
			return message.Message{}, fmt.Errorf("%w: %w", ErrFinalizationScopeChanged, err)
		}
		return message.Message{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return accepted, nil
}
