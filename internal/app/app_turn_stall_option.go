package app

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
)

// turnStallCallTimeout resolves the call's turn-stall threshold: the
// configured Options.TurnStallTimeoutSeconds, or the 30-minute default when
// unset (0).
func turnStallCallTimeout(opts *config.Options) time.Duration {
	if opts == nil || opts.TurnStallTimeoutSeconds <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(opts.TurnStallTimeoutSeconds) * time.Second
}

// applyTurnStallCallTimeout pins the resolved threshold onto CallOptions.
func applyTurnStallCallTimeout(call *agent.CallOptions, opts *config.Options) {
	call.TurnStallTimeout = turnStallCallTimeout(opts)
}
