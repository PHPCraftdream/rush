package app

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

func runMarkedFinalizationQueue(ctx context.Context, coordinator agent.Coordinator, data coresession.SessionAgentCallData) (bool, *any, error) {
	if data.FinalizationRequirement == nil {
		return false, nil, nil
	}
	combined, ok := coordinator.(agent.FinalizationQueueCoordinator)
	if !ok {
		return false, nil, nil
	}
	result, err := combined.RunFinalizationQueuedCall(ctx, session.RunQueueRowIDFrom(ctx), data)
	if result == nil && err == nil {
		return true, nil, coresession.ErrCallQueuedNotExecuted
	}
	var value any
	if result != nil {
		value = result
	}
	return true, &value, err
}
