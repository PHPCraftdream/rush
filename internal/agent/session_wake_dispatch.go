package agent

import (
	"context"
	"errors"
)

type SessionWakeDispatcher interface {
	WakeSession(context.Context, string, bool) error
}

func DispatchSessionWake(ctx context.Context, c Coordinator, owner string) error {
	if c == nil {
		return errors.New("session wake dispatcher: nil coordinator")
	}
	d, ok := c.(SessionWakeDispatcher)
	if !ok {
		return errors.New("session wake dispatcher: unsupported coordinator type")
	}
	return d.WakeSession(ctx, owner, true)
}

func (c *coordinator) WakeSession(ctx context.Context, owner string, fact bool) error {
	return c.wakeSession(ctx, owner, fact)
}
