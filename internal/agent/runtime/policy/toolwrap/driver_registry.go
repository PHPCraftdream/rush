package toolwrap

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// Driver is a frozen snapshot of the SessionAgentCall shape
// runSubAgent used to start a delegated child's turn, plus the SessionAgent
// it ran on. CallFor reuses both for a later wake: the SAME agent (so the
// wake queues into, or restarts, the correct mailbox) and the SAME
// model/provider/sampling shape the delegation was launched with -- only the
// prompt changes.
type Driver struct {
	Agent agentcall.SessionAgent
	Call  agentcall.SessionAgentCall
	// Frozen at delegation; wakes never bind or infer replacement capabilities.
	FinalizationAuthorities session.AgentFinalizationContext
	PctReleases             []func() // pct: retained child construction refs released at confirmed closure.
	// ParentSessionID is the delegating session that originally armed this
	// child's restricted-run allowlist baseline. Wakes re-inherit from it
	// on every wake, not just the first turn.
	ParentSessionID string
	// SlotRole records live config-slot provenance from the original build.
	SlotRole config.SelectedModelType
	// Generation guards a driver record; a stale fire for a prior delegation can
	// never cancel or release a later resume of the same child session.
	Generation uint64
}

// CallFor returns a copy of the driver's call template with a new prompt.
func (d Driver) CallFor(prompt string) agentcall.SessionAgentCall {
	call := agentcall.CallFromActive(d.Call)
	call.Prompt = prompt
	call.FinalizationAuthorities = d.FinalizationAuthorities
	call.DriverGeneration = d.Generation
	if call.CallOptions != nil {
		copy := *call.CallOptions
		call.CallOptions = &copy
	}
	return call
}

// Registry stores the current driver for each delegated child session.
type Registry struct {
	mu      sync.Mutex
	byChild map[string]Driver
	nextGen uint64
}

// NewRegistry creates a driver registry.
func NewRegistry() *Registry { return &Registry{byChild: make(map[string]Driver)} }

// Register records or replaces a child's driver and returns its generation.
func (r *Registry) Register(childSessionID string, driver Driver) uint64 {
	if r == nil || childSessionID == "" {
		ReleaseScopeRefs(driver.PctReleases)
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextGen++
	driver.Generation = r.nextGen
	old := r.byChild[childSessionID]
	if len(old.PctReleases) != 0 {
		driver.PctReleases = append(driver.PctReleases, old.PctReleases...)
	}
	r.byChild[childSessionID] = driver
	return driver.Generation
}

// Get returns the driver's snapshot for a child session.
func (r *Registry) Get(childSessionID string) (Driver, bool) {
	if r == nil || childSessionID == "" {
		return Driver{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byChild[childSessionID]
	return d, ok
}

// ChildSessions lists registered child session IDs.
func (r *Registry) ChildSessions() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byChild))
	for id := range r.byChild {
		out = append(out, id)
	}
	return out
}

// ReleaseIfCurrent removes a driver only if its generation is still current.
func (r *Registry) ReleaseIfCurrent(childSessionID string, g uint64) bool {
	if r == nil || childSessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.byChild[childSessionID]; ok && current.Generation == g {
		delete(r.byChild, childSessionID)
		ReleaseScopeRefs(current.PctReleases)
		return true
	}
	return false
}

// AllDriverAgents returns distinct agents currently registered as drivers.
func (r *Registry) AllDriverAgents() []agentcall.SessionAgent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[agentcall.SessionAgent]struct{}, len(r.byChild))
	out := make([]agentcall.SessionAgent, 0, len(r.byChild))
	for _, d := range r.byChild {
		if d.Agent == nil {
			continue
		}
		if _, dup := seen[d.Agent]; dup {
			continue
		}
		seen[d.Agent] = struct{}{}
		var allReleases []func()
		for _, sameAgent := range r.byChild {
			if sameAgent.Agent == d.Agent {
				allReleases = append(allReleases, sameAgent.PctReleases...)
			}
		}
		driverAgent := d.Agent
		if len(allReleases) != 0 {
			driverAgent = NewDriverDisposer(d.Agent, allReleases)
		}
		out = append(out, driverAgent)
	}
	return out
}

// BindFinalizationIdentity changes only identity transport, not frozen policy,
// credentials, tools, budgets, model settings or construction owners.
func (r *Registry) BindFinalizationIdentity(agent agentcall.SessionAgent, call agentcall.SessionAgentCall) error {
	if r == nil || call.DriverGeneration == 0 {
		return session.ErrAgentFinalizationDriverUnavailable
	}
	requirement := call.FinalizationAuthorities.Requirement()
	if requirement == nil {
		return session.ErrAgentFinalizationChanged
	}
	incoming := make(map[string]session.AgentFinalizationIdentity, len(requirement.Authorities))
	for _, identity := range requirement.Authorities {
		incoming[identity.SessionID] = identity
	}
	own, exists := incoming[call.SessionID]
	if !exists || own.LogicalCallID != call.LogicalCallID {
		return session.ErrAgentFinalizationChanged
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	driver, exists := r.byChild[call.SessionID]
	if !exists || driver.Agent != agent || driver.Call.SessionID != call.SessionID || driver.Generation != call.DriverGeneration ||
		driver.Call.LogicalCallID != "" && driver.Call.LogicalCallID != call.LogicalCallID {
		return session.ErrAgentFinalizationDriverUnavailable
	}
	previous := make(map[string]session.AgentFinalizationIdentity)
	if stored := driver.FinalizationAuthorities.Requirement(); stored != nil {
		for _, identity := range stored.Authorities {
			previous[identity.SessionID] = identity
			if incoming[identity.SessionID] != identity {
				return session.ErrAgentFinalizationChanged
			}
		}
	}
	for owner := range incoming {
		if owner != call.SessionID {
			if _, inherited := previous[owner]; !inherited {
				return session.ErrAgentFinalizationChanged
			}
		}
	}
	driver.Call.LogicalCallID = call.LogicalCallID
	driver.Call.DriverGeneration = call.DriverGeneration
	driver.FinalizationAuthorities = call.FinalizationAuthorities
	r.byChild[call.SessionID] = driver
	return nil
}
