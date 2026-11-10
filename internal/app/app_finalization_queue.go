package app

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
)

func runMarkedFinalizationQueue(ctx context.Context, coordinator agent.Coordinator, data session.SessionAgentCallData) (bool, *any, error) {
	if data.FinalizationRequirement == nil {
		return false, nil, nil
	}
	combined, ok := coordinator.(agent.FinalizationQueueCoordinator)
	if !ok {
		return false, nil, nil
	}
	result, err := combined.RunFinalizationQueuedCall(ctx, session.RunQueueRowIDFrom(ctx), data)
	if result == nil && err == nil {
		return true, nil, session.ErrCallQueuedNotExecuted
	}
	var value any
	if result != nil {
		value = result
	}
	return true, &value, err
}
