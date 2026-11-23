package shexec

import (
	"context"
	"os/exec"
)

// RetainedLauncher is the retained-process launch path. The background-job
// layer installs it on the execution context with WithRetainedLauncher; the
// exec handlers and the shebang fallback consult it per command instead of
// naming the retained stop types, which live above this package and must not
// be imported from here.
type RetainedLauncher interface {
	Launch(ctx context.Context, cmd *exec.Cmd, kill func() error, register func(int), afterStart func(*exec.Cmd) func()) (bool, error)
}

type retainedLauncherKey struct{}

// WithRetainedLauncher returns a context that carries launcher for every
// command executed under it.
func WithRetainedLauncher(ctx context.Context, launcher RetainedLauncher) context.Context {
	return context.WithValue(ctx, retainedLauncherKey{}, launcher)
}

// RetainedLauncherFrom returns the launcher installed by
// WithRetainedLauncher, or nil when the command runs on the ordinary launch
// path.
func RetainedLauncherFrom(ctx context.Context) RetainedLauncher {
	launcher, _ := ctx.Value(retainedLauncherKey{}).(RetainedLauncher)
	return launcher
}
