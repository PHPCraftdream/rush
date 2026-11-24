package agent

import (
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// bindFinalizationIdentity changes only identity transport, not frozen policy,
// credentials, tools, budgets, model settings or construction owners.
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
		return a.coord.subAgentDrivers.BindFinalizationIdentity(a, call)
	}
	return nil
}
