// Per-call restricted-run policy transport (review R3-4): the compiled
// RunAllowlist an ExecuteRun caller computed for THIS invocation travels
// on the call's context (WithRunAllowlist) and is stamped onto the
// SessionAgentCall by buildCall/runInternal, so the policy reaches the
// turn even when the call spends time in the mailbox queue first — the
// turn runs under the OWNER's context, not the queued caller's, so a
// context-only value would be lost across the handoff.
//
// The value must be treated as immutable after construction.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/permission"
)

// runAllowlistContextKey is the unexported context key carrying
// *permission.RunAllowlist.
type runAllowlistContextKey struct{}

// WithRunAllowlist returns a context carrying the compiled restricted-run
// policy for this one call. nil (the zero case) arms nothing: the turn
// keeps the historical fallback behavior.
func WithRunAllowlist(ctx context.Context, a *permission.RunAllowlist) context.Context {
	return context.WithValue(ctx, runAllowlistContextKey{}, a)
}

// runAllowlistFrom returns the per-call restricted-run policy carried by
// ctx, or nil when the context has none (legacy caller).
func runAllowlistFrom(ctx context.Context) *permission.RunAllowlist {
	a, _ := ctx.Value(runAllowlistContextKey{}).(*permission.RunAllowlist)
	return a
}
