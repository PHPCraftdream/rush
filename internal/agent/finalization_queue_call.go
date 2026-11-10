package agent

import (
	"context"
	"slices"

	"github.com/PHPCraftdream/rush/internal/session"
)

func (c *coordinator) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
	store, ok := c.sessions.(session.AgentFinalizationQueueService)
	if !ok {
		if data.FinalizationRequirement != nil {
			return session.AgentFinalizationContext{}, session.ErrAgentFinalizationChanged
		}
		return session.AgentFinalizationContext{}, nil
	}
	guardCtx, cancel := context.WithTimeout(ctx, sessionPreambleMaxDurationDefault)
	defer cancel()
	return store.RestoreAgentFinalizationQueue(guardCtx, rowID, data)
}

// A durable child continuation cannot silently become a root-agent run.
func (c *coordinator) finalizationQueueDriver(call SessionAgentCall) (SessionAgent, error) {
	requirement := call.finalizationAuthorities.Requirement()
	if requirement == nil {
		return c.currentAgent, nil
	}
	if driver, ok := c.subAgentDrivers.get(call.SessionID); ok && driver.agent != nil {
		stored := driver.finalizationAuthorities.Requirement()
		if driver.call.SessionID != call.SessionID || driver.call.LogicalCallID != call.LogicalCallID || stored == nil ||
			stored.Version != requirement.Version || !slices.Equal(stored.Authorities, requirement.Authorities) {
			return nil, session.ErrAgentFinalizationDriverUnavailable
		}
		return driver.agent, nil
	}
	if call.finalizationAuthorities.RequiresManagedDriver() || len(requirement.Authorities) != 1 {
		return nil, session.ErrAgentFinalizationDriverUnavailable
	}
	identity := requirement.Authorities[0]
	if identity.SessionID != call.SessionID || identity.LogicalCallID != call.LogicalCallID || identity.NodeID != "" {
		return nil, session.ErrAgentFinalizationDriverUnavailable
	}
	return c.currentAgent, nil
}

func enqueueFinalizationCall(ctx context.Context, sessions session.Service, rowID string, call SessionAgentCall, raw []byte) error {
	restored, err := call.finalizationAuthorities.Restore(ctx)
	if err != nil {
		return err
	}
	return sessions.EnqueueRunQueueEntry(restored, rowID, call.SessionID, raw)
}
