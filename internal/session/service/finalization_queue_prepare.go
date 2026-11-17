package session

import (
	"context"
	"encoding/json"
)

// AgentFinalizationQueuePreparer validates without consuming a continuation.
type AgentFinalizationQueuePreparer interface {
	PrepareAgentFinalizationQueue(context.Context, string, SessionAgentCallData) (*AgentFinalizationQueuePreparation, error)
}

// AgentFinalizationQueuePreparation exposes only verified identity declarations
// until the existing driver owns the mailbox era and consumes the real lease.
type AgentFinalizationQueuePreparation struct {
	store    *service
	rowID    string
	frozen   []byte
	snapshot AgentFinalizationContext
}

func (s *service) PrepareAgentFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData) (*AgentFinalizationQueuePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rowID == "" || data.FinalizationRequirement == nil {
		return nil, ErrAgentFinalizationChanged
	}
	frozen, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var copy SessionAgentCallData
	if err := json.Unmarshal(frozen, &copy); err != nil {
		return nil, err
	}
	snapshot, err := s.restoreFinalizationQueue(ctx, rowID, copy, false)
	if err != nil {
		return nil, err
	}
	return &AgentFinalizationQueuePreparation{store: s, rowID: rowID, frozen: frozen, snapshot: snapshot}, nil
}

func (p *AgentFinalizationQueuePreparation) Requirement() *AgentFinalizationRequirement {
	if p == nil {
		return nil
	}
	return p.snapshot.Requirement()
}

func (p *AgentFinalizationQueuePreparation) RequiresManagedDriver() bool {
	return p != nil && p.snapshot.RequiresManagedDriver()
}

// Consume revalidates the full lease and provenance in the same CAS transaction.
// It returns detached frozen call data, never the caller's mutable pointers.
func (p *AgentFinalizationQueuePreparation) Consume(ctx context.Context) (AgentFinalizationContext, SessionAgentCallData, error) {
	if err := ctx.Err(); err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	if p == nil || p.store == nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, ErrAgentFinalizationChanged
	}
	var data SessionAgentCallData
	if err := json.Unmarshal(p.frozen, &data); err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	snapshot, err := p.store.RestoreAgentFinalizationQueue(ctx, p.rowID, data)
	if err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	return snapshot, data, nil
}

var _ AgentFinalizationQueuePreparer = (*service)(nil)
