// R11F shared flag parser for the inert batch runtime (RUSH_BATCH_RUNTIME).
// Lives in the lowest-level config package so both internal/app (host
// lifecycle install) and internal/server (web surface) read ONE flag
// definition. Same idiom as the keepalive cache flag
// (internal/agent/agent_cache_keepalive.go): unset or unparseable means OFF.
package config

import (
	"os"
	"strconv"
)

// BatchRuntimeEnabled reports whether the batch runtime is switched on.
func BatchRuntimeEnabled() bool {
	on, err := strconv.ParseBool(os.Getenv("RUSH_BATCH_RUNTIME"))
	return err == nil && on
}
