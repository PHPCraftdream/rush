package agent

import (
	"context"
	"slices"

	"github.com/PHPCraftdream/rush/internal/session"
)

func (c *coordinator) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
	accepted, _ := ctx.Value(finalizationQueueAcceptedKey{}).(*finalizationQueueAccepted)
	if accepted != nil {
		payload, err := finalizationQueuePayload(data)
		if err != nil {
			return session.AgentFinalizationContext{}, err
		}
		if accepted.rowID != rowID || accepted.payload != payload || accepted.sessionID != data.SessionID || accepted.logicalID != data.LogicalCallID {
			return session.AgentFinalizationContext{}, session.ErrAgentFinalizationChanged
		}
		return accepted.snapshot, nil
	}
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
	driver, _, err := c.selectFinalizationQueueDriver(call.SessionID, call.LogicalCallID, call.finalizationAuthorities.Requirement(), call.finalizationAuthorities.RequiresManagedDriver())
	return driver, err
}

func (c *coordinator) selectFinalizationQueueDriver(sessionID, logicalID string, requirement *session.AgentFinalizationRequirement, managed bool) (SessionAgent, uint64, error) {
	if requirement == nil {
		return c.currentAgent, 0, nil
	}
	if driver, ok := c.subAgentDrivers.get(sessionID); ok && driver.agent != nil {
		stored := driver.finalizationAuthorities.Requirement()
		if driver.call.SessionID != sessionID || driver.call.LogicalCallID != logicalID || stored == nil ||
			stored.Version != requirement.Version || !slices.Equal(stored.Authorities, requirement.Authorities) {
			return nil, 0, session.ErrAgentFinalizationDriverUnavailable
		}
		return driver.agent, driver.generation, nil
	}
	if managed || len(requirement.Authorities) != 1 {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	identity := requirement.Authorities[0]
	if identity.SessionID != sessionID || identity.LogicalCallID != logicalID || identity.NodeID != "" {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	return c.currentAgent, 0, nil
}

func enqueueFinalizationCall(ctx context.Context, sessions session.Service, rowID string, call SessionAgentCall, raw []byte) error {
	restored, err := call.finalizationAuthorities.Restore(ctx)
	if err != nil {
		return err
	}
	return sessions.EnqueueRunQueueEntry(restored, rowID, call.SessionID, raw)
}
