package session

import (
	"context"
	"errors"
	"slices"

	"github.com/PHPCraftdream/rush/internal/session/service/components/sessguard"
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

// FinalizationScoped reports whether the caller carries finalization
// authorities, i.e. whether admission can mint revisions for this call.
func FinalizationScoped(ctx context.Context) bool {
	return len(CaptureAgentFinalizationContext(ctx).authorities) != 0
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
		if authority.Identity.SessionID == sessionID && authority.Identity.LogicalCallID != logicalID {
			return nil, ErrAgentFinalizationChanged
		}
		if err := authority.Revalidate(ctx); err != nil {
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
			if authority == nil || authority.Service == nil || !authority.Identity.Valid() {
				return nil, ErrAgentFinalizationChanged
			}
			owner := authority.Identity.SessionID
			if previous := byOwner[owner]; previous != nil {
				if previous.Service != authority.Service || previous.Identity != authority.Identity {
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

func BatchFinalizationIdentityFromContext(ctx context.Context, id BatchCommitIdentity) (BatchCommitIdentity, error) {
	if ctx == nil {
		return BatchCommitIdentity{}, errors.New("batch finalization authority service unavailable")
	}
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	if len(authorities) == 0 {
		return id, nil
	}
	if id.FinalizationService() == nil {
		return BatchCommitIdentity{}, errors.New("batch finalization authority service unavailable")
	}
	_, boundInner := id.FinalizationBinding()
	bound := make([]*AgentFinalizationAuthority, 0, len(boundInner))
	for _, authority := range boundInner {
		var service *service
		for _, contextual := range authorities {
			if contextual != nil && contextual.Service != nil && contextual.Service.finalizationIdentity == id.FinalizationService() {
				service = contextual.Service
				break
			}
		}
		if service == nil || authority.ServiceIdentity() != id.FinalizationService() {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		bound = append(bound, &AgentFinalizationAuthority{Service: service, DB: authority.DB, Identity: authority.Identity, inner: authority})
	}
	combined, err := mergeFinalizationAuthorities(bound, authorities)
	if err != nil {
		return BatchCommitIdentity{}, err
	}
	inner := make([]*sessguard.AgentFinalizationAuthority, 0, len(combined))
	for _, authority := range combined {
		if authority.Service == nil || authority.Service.finalizationIdentity != id.FinalizationService() {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		innerAuthority := authority.Service.sessguardAuthority(authority)
		if innerAuthority == nil {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		inner = append(inner, innerAuthority)
	}
	return serviceIdentity(id, sessguard.AttachFinalizationAuthorities(componentIdentity(id), id.FinalizationService(), inner...)), nil
}
