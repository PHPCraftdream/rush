// Package batchexec owns accepted batch-leaf execution lifetimes and binding leases.
package batchexec

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Runner is the run-only view of an already resolved policy leaf.
type Runner interface {
	Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

// Output supplies the host's result envelope mapping and optional cache handoff.
// Store runs synchronously before report; the host chooses the cursor contract.
type Output struct {
	Result func(fantasy.ToolResponse, error, bool) jobbatch.Result
	Store  func(jobbatch.OutputChunk)
}
