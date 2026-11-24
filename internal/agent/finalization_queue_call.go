package agent

import (
	"context"
	"slices"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func (c *coordinator) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
	return newFinalizationQueueComponent(c).restoreFinalizationQueuedCall(ctx, rowID, data)
}

func (c *coordinator) finalizationQueueDriver(call SessionAgentCall) (SessionAgent, error) {
	return newFinalizationQueueComponent(c).finalizationQueueDriver(call)
}

func (c *coordinator) selectFinalizationQueueDriver(sessionID, logicalID string, requirement *session.AgentFinalizationRequirement, managed bool) (SessionAgent, uint64, error) {
	return newFinalizationQueueComponent(c).selectFinalizationQueueDriver(sessionID, logicalID, requirement, managed)
}

// A durable child continuation cannot silently become a root-agent run.
func (p finalizationQueueComponent) finalizationQueueDriver(call SessionAgentCall) (SessionAgent, error) {
	driver, _, err := p.selectFinalizationQueueDriver(call.SessionID, call.LogicalCallID, call.FinalizationAuthorities.Requirement(), call.FinalizationAuthorities.RequiresManagedDriver())
	return driver, err
}

func (p finalizationQueueComponent) selectFinalizationQueueDriver(sessionID, logicalID string, requirement *session.AgentFinalizationRequirement, managed bool) (SessionAgent, uint64, error) {
	if requirement == nil {
		return p.host.currentAgent(), 0, nil
	}
	if driver, ok := p.host.subAgentDrivers().Get(sessionID); ok && driver.Agent != nil {
		stored := driver.FinalizationAuthorities.Requirement()
		if driver.Call.SessionID != sessionID || driver.Call.LogicalCallID != logicalID || stored == nil ||
			stored.Version != requirement.Version || !slices.Equal(stored.Authorities, requirement.Authorities) {
			return nil, 0, session.ErrAgentFinalizationDriverUnavailable
		}
		return driver.Agent, driver.Generation, nil
	}
	if managed || len(requirement.Authorities) != 1 {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	identity := requirement.Authorities[0]
	if identity.SessionID != sessionID || identity.LogicalCallID != logicalID || identity.NodeID != "" {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	return p.host.currentAgent(), 0, nil
}

func (p finalizationQueueComponent) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
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
	store, ok := p.host.sessionsService().(session.AgentFinalizationQueueService)
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

type finalizationQueueComponentHost interface {
	sessionsService() session.Service
	currentAgent() SessionAgent
	subAgentDrivers() *toolwrap.Registry
	readyWait() error
	rebuildSessionAgentCall() func(context.Context, string, session.SessionAgentCallData) (SessionAgentCall, error)
	runSessionAgentCall() func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
}

func newFinalizationQueueComponent(c *coordinator) finalizationQueueComponent {
	return finalizationQueueComponent{host: finalizationQueueComponentHostAdapter{coordinator: c}}
}

func enqueueFinalizationCall(ctx context.Context, sessions session.Service, rowID string, call SessionAgentCall, raw []byte) error {
	restored, err := call.FinalizationAuthorities.Restore(ctx)
	if err != nil {
		return err
	}
	return sessions.EnqueueRunQueueEntry(restored, rowID, call.SessionID, raw)
}
