package session

import (
	"context"
	"errors"
)

type agentFinalizationAuthoritiesKey struct{}

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
