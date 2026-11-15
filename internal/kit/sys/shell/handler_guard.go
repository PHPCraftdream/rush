package shell

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"mvdan.cc/sh/v3/interp"
)

// panicGuardHandler returns middleware that converts a panic escaping any
// downstream exec handler into one failed command instead of a process-killing
// panic. It must be the outermost layer of the chain: exec handlers run inside
// pipeline stage goroutines, so a panic there cannot be recovered by the
// caller of [Run] or [Shell.execCommon] and would take the whole process down.
func panicGuardHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) (err error) {
			name := "<unknown>"
			if len(args) > 0 {
				name = args[0]
			}
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				fmt.Fprintf(interp.HandlerCtx(ctx).Stderr, "%s: internal error: %v\n", name, r)
				slog.Error("Recovered panic in exec handler",
					"command", name,
					"panic", r,
					"stack", string(debug.Stack()))
				err = interp.ExitStatus(2)
			}()
			return next(ctx, args)
		}
	}
}
