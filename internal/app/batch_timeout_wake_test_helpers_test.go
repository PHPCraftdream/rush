package app

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent"
)

type wakeDispatchTestCoordinator struct {
	agent.Coordinator
	wake func(context.Context, string, bool) error
}

func (c wakeDispatchTestCoordinator) WakeSession(ctx context.Context, owner string, fact bool) error {
	return c.wake(ctx, owner, fact)
}

var _ agent.SessionWakeDispatcher = wakeDispatchTestCoordinator{}
