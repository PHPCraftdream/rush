package agent

import (
	"context"
	"slices"

	"charm.land/fantasy"
)

// providerOptionsTool attaches a per-CALL fantasy.ProviderOptions value to a
// tool without touching the tool object itself.
//
// Why this exists: a sessionAgent's tool slice (a.tools) is shared by every
// session and every concurrent turn running on that agent — csync.Slice makes
// the SLICE safe to read concurrently, but the elements are pointers to ONE
// set of tool objects. runTurn used to attach the Anthropic cache-control
// marker by calling SetProviderOptions on the last of those shared objects,
// which writes fantasy's funcToolWrapper.providerOptions field in place. With
// two turns in flight on one agent — an ordinary web server with two busy
// sessions, or two sdk.Client.RunWithCredentials calls — both turns pick the
// same last tool (the slice is name-sorted, so both see the same element) and
// both write that same field. -race reports it as a genuine DATA RACE
// (confirmed: two `funcToolWrapper.SetProviderOptions` writes from two
// `sessionAgent.runTurn` goroutines at the same address). Even though the
// value happens to be equivalent for every turn, each call builds a fresh map
// with fresh pointers, so this is an unsynchronized write of a new map header,
// not a benign idempotent store.
//
// Wrapping instead of mutating also removes a second, quieter problem: the old
// code depended on the mutation PERSISTING on the shared object so that
// PrepareStep's fresh a.tools.Copy() would still carry the marker. That made
// a per-turn decision into shared agent state that any later turn could
// change underneath an in-flight one.
type providerOptionsTool struct {
	inner fantasy.AgentTool
	opts  fantasy.ProviderOptions
}

func (t *providerOptionsTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *providerOptionsTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.inner.Run(ctx, call)
}

func (t *providerOptionsTool) ProviderOptions() fantasy.ProviderOptions { return t.opts }

// SetProviderOptions writes to the wrapper, never to the wrapped tool — that
// is the entire point of this type. The wrapper itself is built fresh per
// call/step and never shared, so this write has a single owner.
func (t *providerOptionsTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.opts = opts }

// withProviderOptionsOnLast returns a copy of tools whose LAST element carries
// opts, leaving every shared tool object untouched. Empty/absent options and
// an empty tool list are both no-ops, so callers can hand the result straight
// to fantasy.WithTools.
//
// "Last tool" is the provider contract this implements: Anthropic caches the
// tool definitions block up to and including the last cache_control marker, so
// marking the final tool caches the whole block.
func withProviderOptionsOnLast(tools []fantasy.AgentTool, opts fantasy.ProviderOptions) []fantasy.AgentTool {
	if len(tools) == 0 || len(opts) == 0 {
		return tools
	}
	out := slices.Clone(tools)
	last := len(out) - 1
	out[last] = &providerOptionsTool{inner: out[last], opts: opts}
	return out
}
