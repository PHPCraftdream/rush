package tools

import (
	"context"

	"charm.land/fantasy"
)

type commandSpawnProbeKey struct{}

// WithCommandSpawnProbe installs a test-only spawn substitute in a context.
// Production callers should not use this; absent the value, execution is unchanged.
func WithCommandSpawnProbe(ctx context.Context, spawn func(context.Context, string, []string) (fantasy.ToolResponse, error)) context.Context {
	return context.WithValue(ctx, commandSpawnProbeKey{}, spawn)
}

func commandSpawnFromContext(ctx context.Context) func(context.Context, string, []string) (fantasy.ToolResponse, error) {
	spawn, _ := ctx.Value(commandSpawnProbeKey{}).(func(context.Context, string, []string) (fantasy.ToolResponse, error))
	return spawn
}

// testCommandSpawn is a test-only seam at the post-guard, post-permission
// execution boundary. Production leaves it nil and follows normal spawning.
var testCommandSpawn func(context.Context, string, []string) (fantasy.ToolResponse, error)
