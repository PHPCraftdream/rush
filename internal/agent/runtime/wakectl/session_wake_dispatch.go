package wakectl

import (
	"context"
	"errors"
)

type SessionWakeDispatcher interface {
	WakeSession(context.Context, string, bool) error
}

func DispatchSessionWake(ctx context.Context, candidate any, owner string) error {
	if candidate == nil {
		return errors.New("session wake dispatcher: nil coordinator")
	}
	d, ok := candidate.(SessionWakeDispatcher)
	if !ok {
		return errors.New("session wake dispatcher: unsupported coordinator type")
	}
	return d.WakeSession(ctx, owner, true)
}
