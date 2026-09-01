// Per-call restricted-run policy transport (review R3-4): the compiled
// RunAllowlist an ExecuteRun caller computed for THIS invocation travels
// on the call's context (WithRunAllowlist) and is stamped onto the
// SessionAgentCall by buildCall/runInternal, so the policy reaches the
// turn even when the call spends time in the mailbox queue first — the
// turn runs under the OWNER's context, not the queued caller's, so a
// context-only value would be lost across the handoff. The UNCOMPILED
// spec travels alongside it (WithRunAllowlistSpec) so the durable run
// queue can persist the caller's declared policy and a pump-driven
// restart can recompile it (review R4-1/R4-2/R4-3).
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

// runAllowlistSpecContextKey is the unexported context key carrying
// *permission.RunAllowlistSpec.
type runAllowlistSpecContextKey struct{}

// WithRunAllowlistSpec returns a context carrying the UNCOMPILED
// restricted-run policy spec for this one call, alongside the compiled
// matcher carried by WithRunAllowlist. Only ExecuteRun attaches it: its
// presence on a serialized call is the durable marker of an
// ExecuteRun-lineage call, which is what lets RebuildSessionAgentCall
// re-arm the restart's own policy and RunSessionAgentCall re-arm the
// session's auto-approve after a real process restart. nil arms nothing.
func WithRunAllowlistSpec(ctx context.Context, spec *permission.RunAllowlistSpec) context.Context {
	return context.WithValue(ctx, runAllowlistSpecContextKey{}, spec)
}

// runAllowlistSpecFrom returns the per-call restricted-run policy spec
// carried by ctx, or nil when the context has none (legacy caller).
func runAllowlistSpecFrom(ctx context.Context) *permission.RunAllowlistSpec {
	spec, _ := ctx.Value(runAllowlistSpecContextKey{}).(*permission.RunAllowlistSpec)
	return spec
}
