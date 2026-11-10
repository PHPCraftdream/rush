package session

import (
	"context"
	"errors"
	"fmt"
)

// AgentFinalizationService is an optional trusted API implemented by the
// concrete service; the broad Service interface remains unchanged.
type AgentFinalizationService interface {
	BindAgentFinalization(context.Context, AgentFinalizationIdentity) (*AgentFinalizationAuthority, error)
	WithAgentFinalizationAuthorities(BatchCommitIdentity, ...*AgentFinalizationAuthority) (BatchCommitIdentity, error)
}

// AgentFinalizationAuthority is an immutable capability for one named owner
// identity, bound to the service instance that persisted and revalidates it.
type AgentFinalizationAuthority struct {
	service  *service
	identity AgentFinalizationIdentity
}

func (s *service) BindAgentFinalization(ctx context.Context, identity AgentFinalizationIdentity) (*AgentFinalizationAuthority, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("finalization authority service unavailable")
	}
	store := NewAgentFinalizationStore(s.db)
	if err := store.Bind(ctx, identity); err != nil {
		return nil, fmt.Errorf("bind finalization identity: %w", err)
	}
	authority := &AgentFinalizationAuthority{service: s, identity: identity}
	if err := authority.revalidate(ctx); err != nil {
		return nil, err
	}
	return authority, nil
}

func (a *AgentFinalizationAuthority) revalidate(ctx context.Context) error {
	if a == nil || a.service == nil || a.service.db == nil || !a.identity.valid() {
		return ErrAgentFinalizationChanged
	}
	store := NewAgentFinalizationStore(a.service.db)
	record, err := store.Snapshot(ctx, a.identity)
	if err != nil {
		return fmt.Errorf("read finalization authority: %w", err)
	}
	if !record.active() {
		return ErrAgentFinalizationChanged
	}
	if record.Cancelled {
		return context.Canceled
	}
	var conflicting int64
	err = a.service.db.QueryRowContext(ctx, `SELECT count(*) FROM session_agent_finalizations
		WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing')
		AND (logical_call_id<>? OR generation<>?)`,
		a.identity.SessionID, a.identity.LogicalCallID, a.identity.Generation).Scan(&conflicting)
	if err != nil {
		return fmt.Errorf("inspect active finalization incarnation: %w", err)
	}
	if conflicting != 0 {
		return ErrAgentFinalizationChanged
	}
	return nil
}

// WithAgentFinalizationAuthorities attaches only the explicitly named affected
// owners; it never expands parent links to ancestors. Callers bind scopes before
// constructing the hook. Unbound legacy owners are not cross-process locked.
func (s *service) WithAgentFinalizationAuthorities(identity BatchCommitIdentity, authorities ...*AgentFinalizationAuthority) (BatchCommitIdentity, error) {
	if s == nil || s.db == nil {
		return BatchCommitIdentity{}, errors.New("finalization authority service unavailable")
	}
	if identity.finalizationService != nil {
		return BatchCommitIdentity{}, errors.New("batch identity already has a finalization authority binding")
	}
	byOwner := make(map[string]*AgentFinalizationAuthority, len(authorities))
	for _, authority := range authorities {
		if authority == nil || authority.service != s || authority.service.db != s.db || !authority.identity.valid() {
			return BatchCommitIdentity{}, errors.New("finalization authority belongs to another service")
		}
		owner := authority.identity.SessionID
		if previous, exists := byOwner[owner]; exists {
			if previous.identity != authority.identity {
				return BatchCommitIdentity{}, fmt.Errorf("conflicting finalization authorities for owner %q", owner)
			}
			return BatchCommitIdentity{}, fmt.Errorf("duplicate finalization authority for owner %q", owner)
		}
		byOwner[owner] = authority
	}
	identity.finalizationService = s
	identity.finalizationAuthorities = append([]*AgentFinalizationAuthority(nil), authorities...)
	return identity, nil
}

func (s *service) hasFinalizationScope(ctx context.Context, owner string) (bool, error) {
	reader := s.readDB
	if reader == nil {
		reader = s.db
	}
	if reader != s.db {
		var exists int
		err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("inspect persisted finalization scope: %w", err)
		}
		if exists != 0 {
			return true, nil
		}
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect persisted finalization scope: %w", err)
	}
	return exists != 0, nil
}

var _ AgentFinalizationService = (*service)(nil)
