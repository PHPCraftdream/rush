package session

import (
	"context"
	"errors"
	"slices"
)

// AgentFinalizationContext is an opaque, in-process capability snapshot.
// It is not a serialized identity or permission to bind another incarnation.
type AgentFinalizationContext struct {
	authorities         []*AgentFinalizationAuthority
	managedContinuation bool
}

// RequiresManagedDriver reports lineage verified by the queue writer snapshot.
func (snapshot AgentFinalizationContext) RequiresManagedDriver() bool {
	return snapshot.managedContinuation
}

func CaptureAgentFinalizationContext(ctx context.Context) AgentFinalizationContext {
	if ctx == nil {
		return AgentFinalizationContext{}
	}
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	managed, _ := ctx.Value(agentFinalizationManagedContinuationKey{}).(bool)
	return AgentFinalizationContext{authorities: slices.Clone(authorities), managedContinuation: managed}
}

// Restore merges without replacing any conflicting authority already present.
func (snapshot AgentFinalizationContext) Restore(ctx context.Context) (context.Context, error) {
	restored, err := WithAgentFinalizationContext(ctx, snapshot.authorities...)
	if err != nil {
		return nil, err
	}
	if snapshot.managedContinuation {
		restored = context.WithValue(restored, agentFinalizationManagedContinuationKey{}, true)
	}
	return restored, nil
}

// RestoreCall refuses a carried owner capability for another logical call.
func (snapshot AgentFinalizationContext) RestoreCall(ctx context.Context, sessionID, logicalID string) (context.Context, error) {
	restored, err := snapshot.Restore(ctx)
	if err != nil {
		return nil, err
	}
	authorities, _ := restored.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	for _, authority := range authorities {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if authority.identity.SessionID == sessionID && authority.identity.LogicalCallID != logicalID {
			return nil, ErrAgentFinalizationChanged
		}
		if err := authority.revalidate(ctx); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

type (
	agentFinalizationAuthoritiesKey         struct{}
	agentFinalizationManagedContinuationKey struct{}
)

// WithAgentFinalizationContext carries bound capabilities, never raw model IDs.
// Descendant calls keep ancestor capabilities until their physical work closes.
func WithAgentFinalizationContext(ctx context.Context, authorities ...*AgentFinalizationAuthority) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("finalization authority context unavailable")
	}
	inherited, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	combined, err := mergeFinalizationAuthorities(inherited, authorities)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, agentFinalizationAuthoritiesKey{}, combined), nil
}

func mergeFinalizationAuthorities(first, second []*AgentFinalizationAuthority) ([]*AgentFinalizationAuthority, error) {
	combined := make([]*AgentFinalizationAuthority, 0, len(first)+len(second))
	byOwner := make(map[string]*AgentFinalizationAuthority)
	for _, set := range [][]*AgentFinalizationAuthority{first, second} {
		for _, authority := range set {
			if authority == nil || authority.service == nil || !authority.identity.valid() {
				return nil, ErrAgentFinalizationChanged
			}
			owner := authority.identity.SessionID
			if previous := byOwner[owner]; previous != nil {
				if previous.service != authority.service || previous.identity != authority.identity {
					return nil, ErrAgentFinalizationChanged
				}
				continue
			}
			byOwner[owner] = authority
			combined = append(combined, authority)
		}
	}
	return combined, nil
}

func batchFinalizationIdentityFromContext(ctx context.Context, id BatchCommitIdentity) (BatchCommitIdentity, error) {
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	if len(authorities) == 0 {
		return id, nil
	}
	if id.finalizationService == nil {
		return BatchCommitIdentity{}, errors.New("batch finalization authority service unavailable")
	}
	combined, err := mergeFinalizationAuthorities(id.finalizationAuthorities, authorities)
	if err != nil {
		return BatchCommitIdentity{}, err
	}
	for _, authority := range combined {
		if authority.service != id.finalizationService {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
	}
	id.finalizationAuthorities = combined
	return id, nil
}
