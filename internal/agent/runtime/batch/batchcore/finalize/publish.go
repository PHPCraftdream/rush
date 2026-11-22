package finalize

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	gate "github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	sessionfacade "github.com/PHPCraftdream/rush/internal/session"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// FinalizationPublicationScope is the durable scope a candidate is persisted
// and sealed against.
type FinalizationPublicationScope struct {
	Store    *session.AgentFinalizationStore
	Identity session.AgentFinalizationIdentity
}

// Matches reports whether a step identity belongs to this scope's logical
// call and generation (finalization_publish.go:22 verbatim).
func (s FinalizationPublicationScope) matches(identity message.FinalizationIdentity) bool {
	return s.Store != nil && identity.Key == s.Identity.LogicalCallID && s.Identity.Generation > 0 &&
		identity.Generation == uint64(s.Identity.Generation) && identity.Revision <= uint64(1<<63-1)
}

// PersistedFinalizationCandidate is a committed candidate and the step
// identity it was recorded under; publication must present both unchanged.
type PersistedFinalizationCandidate struct {
	Candidate gate.Candidate
	Identity  message.FinalizationIdentity
}

// PersistFinalizationCandidate commits the provisional message and its durable
// identity in one writer transaction. Nothing is published before the commit.
// ErrFinalizationScopeChanged means the candidate is stale (older step, moved
// revision, newer checkpoint) and nothing was written.
// (finalization_candidate.go:24 verbatim.)
func PersistFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope FinalizationPublicationScope, identity message.FinalizationIdentity, candidate message.Message) (PersistedFinalizationCandidate, error) {
	if !scope.matches(identity) {
		return PersistedFinalizationCandidate{}, gate.ErrFinalizationScopeChanged
	}
	writer, supported := sessions.(session.FinalizationTransaction)
	if !supported {
		return PersistedFinalizationCandidate{}, fmt.Errorf("%w: finalization transaction unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	storer, supported := messages.(message.FinalizationWriter)
	if !supported {
		return PersistedFinalizationCandidate{}, fmt.Errorf("%w: transactional candidate writer unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	var stored message.Message
	var postCommit func()
	err := writer.WithFinalizationTx(ctx, scope.Identity.SessionID, func(tx *sql.Tx) (err error) {
		stored, postCommit, err = scope.Store.StoreCandidateTx(ctx, tx, scope.Identity, storer, candidate, identity)
		return err
	})
	if err != nil {
		if errors.Is(err, message.ErrFinalizationCandidateChanged) || errors.Is(err, session.ErrAgentFinalizationChanged) {
			return PersistedFinalizationCandidate{}, fmt.Errorf("%w: %w", gate.ErrFinalizationScopeChanged, err)
		}
		return PersistedFinalizationCandidate{}, err
	}
	hash, err := stored.FinalizationContentHash()
	if err != nil {
		return PersistedFinalizationCandidate{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return PersistedFinalizationCandidate{Candidate: gate.Candidate{MessageID: stored.ID, Hash: hash}, Identity: identity}, nil
}

// RecoverFinalizationCandidate re-adopts or fails the candidate a previous
// process recorded, then resumes the gate's step counter above the durable
// high-water mark so a restarted owner is never fenced by its own history.
// (finalization_candidate.go:61 verbatim.)
func recoverFinalizationCandidate(ctx context.Context, messages message.Service, scope FinalizationPublicationScope, g *gate.Gate) (session.AgentFinalizationRecovery, error) {
	reader, supported := messages.(message.FinalizationReader)
	if !supported || scope.Store == nil || g == nil {
		return session.AgentFinalizationNoCandidate, fmt.Errorf("%w: candidate recovery unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	verdict, record, err := scope.Store.RecoverCandidate(ctx, scope.Identity, reader)
	if err != nil {
		return session.AgentFinalizationNoCandidate, err
	}
	g.ResumeCandidateSerial(uint64(record.CandidateSerial))
	return verdict, nil
}

// PublishFinalizationCandidate runs only while the owner gate and session are
// held (finalization_publish.go:28 verbatim).
func PublishFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope FinalizationPublicationScope, identity message.FinalizationIdentity, candidate gate.Candidate) (message.Message, error) {
	owner := scope.Identity.SessionID
	if !scope.matches(identity) {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	sealed, ok := session.CandidateOf(candidate.MessageID, candidate.Hash, identity)
	if !ok {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	writer, supported := sessions.(session.FinalizationTransaction)
	if !supported {
		return message.Message{}, fmt.Errorf("%w: finalization transaction unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	reader, supported := messages.(message.FinalizationReader)
	if !supported {
		return message.Message{}, fmt.Errorf("%w: transactional candidate reader unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	var accepted message.Message
	var postCommit func()
	err := writer.WithFinalizationTx(ctx, owner, func(tx *sql.Tx) error {
		current, err := reader.GetFinalizationCandidateTx(ctx, tx, candidate.MessageID)
		if err != nil {
			return err
		}
		if current.SessionID != owner {
			return message.ErrFinalizationCandidateChanged
		}
		err = current.AcceptFinalizationCandidate(identity, candidate.Hash)
		alreadyAccepted := errors.Is(err, message.ErrFinalizationAlreadyAccepted)
		if err != nil && !alreadyAccepted {
			return err
		}
		sealErr := scope.Store.SealTx(ctx, tx, scope.Identity, sealed)
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
			return message.Message{}, fmt.Errorf("%w: %w", gate.ErrFinalizationScopeChanged, err)
		}
		return message.Message{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return accepted, nil
}

// ErrFinalizationSettlerUnavailable refuses to wait without a debt settler:
// without one an open debt would never be settled and the seal would refuse
// forever, so fail-closed beats a silent endless wait
// (finalization_settle.go:15 verbatim).
var ErrFinalizationSettlerUnavailable = errors.New("finalization debt settler unavailable")

// SettleFinalizationDebts settles the owner's reaction debts once per wait
// entry. A settlement error is logged and swallowed: the settler is
// idempotent, the retry happens on the next wait entry, and an unsettled debt
// keeps refusing the seal anyway; only the missing settler is fatal.
// (finalization_settle.go:21 verbatim.)
func SettleFinalizationDebts(ctx context.Context, settler sessionfacade.AgentFinalizationDebtSettler, ownerSessionID string) error {
	if settler == nil {
		return fmt.Errorf("%w: no debt settler is installed", ErrFinalizationSettlerUnavailable)
	}
	if _, err := settler.SettleFinalizationDebts(ctx, ownerSessionID); err != nil {
		slog.Warn("Finalization debt settlement failed", "error", err)
	}
	return nil
}
