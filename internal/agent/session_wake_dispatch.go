package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/wakectl"
)

type SessionWakeDispatcher interface {
	WakeSession(context.Context, string, bool) error
}

func DispatchSessionWake(ctx context.Context, c Coordinator, owner string) error {
	return wakectl.DispatchSessionWake(ctx, c, owner)
}

func (c *coordinator) WakeSession(ctx context.Context, owner string, fact bool) error {
	return c.wakeSession(ctx, owner, fact)
}
