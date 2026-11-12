package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// persistedFinalizationCandidate is a committed candidate and the step identity
// it was recorded under; publication must present both unchanged.
type persistedFinalizationCandidate struct {
	finalizationCandidate
	identity message.FinalizationIdentity
}

// persistFinalizationCandidate commits the provisional message and its durable
// identity in one writer transaction. Nothing is published before the commit.
// ErrFinalizationScopeChanged means the candidate is stale (older step, moved
// revision, newer checkpoint) and nothing was written.
func persistFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope finalizationPublicationScope, identity message.FinalizationIdentity, candidate message.Message) (persistedFinalizationCandidate, error) {
	if !scope.matches(identity) {
		return persistedFinalizationCandidate{}, ErrFinalizationScopeChanged
	}
	writer, supported := sessions.(session.FinalizationTransaction)
	if !supported {
		return persistedFinalizationCandidate{}, fmt.Errorf("%w: finalization transaction unavailable", ErrFinalizationFactsUnavailable)
	}
	storer, supported := messages.(message.FinalizationWriter)
	if !supported {
		return persistedFinalizationCandidate{}, fmt.Errorf("%w: transactional candidate writer unavailable", ErrFinalizationFactsUnavailable)
	}
	var stored message.Message
	var postCommit func()
	err := writer.WithFinalizationTx(ctx, scope.identity.SessionID, func(tx *sql.Tx) (err error) {
		stored, postCommit, err = scope.store.StoreCandidateTx(ctx, tx, scope.identity, storer, candidate, identity)
		return err
	})
	if err != nil {
		if errors.Is(err, message.ErrFinalizationCandidateChanged) || errors.Is(err, session.ErrAgentFinalizationChanged) {
			return persistedFinalizationCandidate{}, fmt.Errorf("%w: %w", ErrFinalizationScopeChanged, err)
		}
		return persistedFinalizationCandidate{}, err
	}
	hash, err := stored.FinalizationContentHash()
	if err != nil {
		return persistedFinalizationCandidate{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return persistedFinalizationCandidate{finalizationCandidate: finalizationCandidate{messageID: stored.ID, hash: hash}, identity: identity}, nil
}

// recoverFinalizationCandidate re-adopts or fails the candidate a previous
// process recorded, then resumes the gate's step counter above the durable
// high-water mark so a restarted owner is never fenced by its own history.
func recoverFinalizationCandidate(ctx context.Context, messages message.Service, scope finalizationPublicationScope, gate *finalizationGate) (session.AgentFinalizationRecovery, error) {
	reader, supported := messages.(message.FinalizationReader)
	if !supported || scope.store == nil || gate == nil {
		return session.AgentFinalizationNoCandidate, fmt.Errorf("%w: candidate recovery unavailable", ErrFinalizationFactsUnavailable)
	}
	verdict, record, err := scope.store.RecoverCandidate(ctx, scope.identity, reader)
	if err != nil {
		return session.AgentFinalizationNoCandidate, err
	}
	gate.resumeCandidateSerial(uint64(record.CandidateSerial))
	return verdict, nil
}

// resumeCandidateSerial raises, never lowers, the step counter.
func (g *finalizationGate) resumeCandidateSerial(serial uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if serial > g.candidateSerial {
		g.candidateSerial = serial
	}
}
