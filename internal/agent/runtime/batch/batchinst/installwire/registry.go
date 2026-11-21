package installwire

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// RegistryStore owns the pinned index and its shared escrow. The escrow policy
// and leaf wrapper are supplied by the install; the index never imports them.
type RegistryStore[E any] struct {
	mu          sync.RWMutex
	registries  map[string]*LeafRegistry
	credentials *E
}

func NewRegistryStore[E any](newEscrow func() *E) RegistryStore[E] {
	return RegistryStore[E]{registries: make(map[string]*LeafRegistry), credentials: newEscrow()}
}

func (s *RegistryStore[E]) credentialEscrowLocked(newEscrow func() *E) *E {
	if s.credentials == nil {
		s.credentials = newEscrow()
	}
	return s.credentials
}

func (s *RegistryStore[E]) CredentialEscrow(newEscrow func() *E) *E {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentialEscrowLocked(newEscrow)
}

// Register refuses replacement and transforms a copy of the registry under
// the same lock that protects initialization of the shared escrow.
func (s *RegistryStore[E]) Register(id string, registry *LeafRegistry, newEscrow func() *E, wrap func(fantasy.AgentTool, *E) fantasy.AgentTool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("batch leaf registry: empty registry identity")
	}
	if registry == nil {
		return errors.New("batch leaf registry: nil registry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registries[id]; exists {
		return fmt.Errorf("batch leaf registry: %q is already pinned", id)
	}
	copyRegistry := registry.CloneLeaves(func(name string, tool fantasy.AgentTool) fantasy.AgentTool {
		if IsDelegation(name) {
			return wrap(tool, s.credentialEscrowLocked(newEscrow))
		}
		return tool
	})
	s.registries[id] = copyRegistry
	return nil
}

func (s *RegistryStore[E]) Resolve(id string) (*LeafRegistry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registry, ok := s.registries[id]
	if !ok {
		return nil, fmt.Errorf("batch leaf registry: unknown pinned registry identity %q", id)
	}
	return registry, nil
}

// Delete removes only the identity index entry, not an ownership record.
func (s *RegistryStore[E]) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registries, id)
}

func (s *RegistryStore[E]) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registries)
}
