package config

import "sync"

// ConfigStore owns uncertainty raised by a durable MCP mutation. The state is
// deliberately outside the replaceable config snapshot so reloads cannot lose
// it before their successful publication boundary.
type mcpUncertaintyState struct {
	mu       sync.RWMutex
	next     uint64
	versions map[string]uint64
}

// MarkMCPUncertain raises the fail-closed fence for name and returns its
// version. A subsequent successful reload of this exact store clears it.
func (s *ConfigStore) MarkMCPUncertain(name string) uint64 {
	if s == nil {
		return 0
	}
	s.mcpUncertainty.mu.Lock()
	defer s.mcpUncertainty.mu.Unlock()
	if s.mcpUncertainty.versions == nil {
		s.mcpUncertainty.versions = make(map[string]uint64)
	}
	s.mcpUncertainty.next++
	s.mcpUncertainty.versions[name] = s.mcpUncertainty.next
	return s.mcpUncertainty.next
}

// MCPUncertaintyVersion reports the current fence version for name.
func (s *ConfigStore) MCPUncertaintyVersion(name string) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.mcpUncertainty.mu.RLock()
	version, ok := s.mcpUncertainty.versions[name]
	s.mcpUncertainty.mu.RUnlock()
	return version, ok
}

// MCPUncertaintyVersions returns a snapshot of the requested fences. With no
// names it returns every fence.
func (s *ConfigStore) MCPUncertaintyVersions(names ...string) map[string]uint64 {
	result := make(map[string]uint64)
	if s == nil {
		return result
	}
	s.mcpUncertainty.mu.RLock()
	defer s.mcpUncertainty.mu.RUnlock()
	if len(names) == 0 {
		for name, version := range s.mcpUncertainty.versions {
			result[name] = version
		}
		return result
	}
	for _, name := range names {
		if version, ok := s.mcpUncertainty.versions[name]; ok {
			result[name] = version
		}
	}
	return result
}

// clearMCPUncertainty clears only fences observed before the reload began.
// Fences raised while a reload is in flight remain for the next reconciliation.
func (s *ConfigStore) clearMCPUncertainty(expected map[string]uint64) {
	if s == nil {
		return
	}
	s.mcpUncertainty.mu.Lock()
	for name, version := range expected {
		if current, ok := s.mcpUncertainty.versions[name]; ok && current == version {
			delete(s.mcpUncertainty.versions, name)
		}
	}
	s.mcpUncertainty.mu.Unlock()
}
