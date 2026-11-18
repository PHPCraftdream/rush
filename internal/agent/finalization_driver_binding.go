package agent

import (
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// finalizationLogicalIdentity reads an already trusted capability, never a
// payload declaration. It does not bind a scope or change an existing call ID.
func finalizationLogicalIdentity(snapshot session.AgentFinalizationContext, sessionID, logicalID string) (string, error) {
	requirement := snapshot.Requirement()
	if requirement == nil {
		return logicalID, nil
	}
	for _, identity := range requirement.Authorities {
		if identity.SessionID != sessionID {
			continue
		}
		if logicalID != "" && logicalID != identity.LogicalCallID {
			return "", session.ErrAgentFinalizationChanged
		}
		return identity.LogicalCallID, nil
	}
	return logicalID, nil
}

// bindFinalizationIdentity changes only identity transport, not frozen policy,
// credentials, tools, budgets, model settings or construction owners.
func (r *subAgentDriverRegistry) bindFinalizationIdentity(agent SessionAgent, call SessionAgentCall) error {
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
	if !exists || driver.agent != agent || driver.call.SessionID != call.SessionID || driver.generation != call.DriverGeneration ||
		driver.call.LogicalCallID != "" && driver.call.LogicalCallID != call.LogicalCallID {
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
	driver.call.LogicalCallID = call.LogicalCallID
	driver.call.DriverGeneration = call.DriverGeneration
	driver.FinalizationAuthorities = call.FinalizationAuthorities
	r.byChild[call.SessionID] = driver
	return nil
}

func (a *sessionAgent) bindFinalizationDriverCall(call SessionAgentCall) error {
	requirement := call.FinalizationAuthorities.Requirement()
	if requirement == nil {
		return nil
	}
	for _, identity := range requirement.Authorities {
		if identity.SessionID != call.SessionID {
			continue
		}
		if call.DriverGeneration == 0 {
			if identity.NodeID != "" || call.FinalizationAuthorities.RequiresManagedDriver() || a.isSubAgent {
				return session.ErrAgentFinalizationDriverUnavailable
			}
			return nil
		}
		if a.coord == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		return a.coord.subAgentDrivers.bindFinalizationIdentity(a, call)
	}
	return nil
}
