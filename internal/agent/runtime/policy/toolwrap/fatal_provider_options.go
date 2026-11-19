package toolwrap

import (
	"context"
	"slices"
	"sync"

	"charm.land/fantasy"
)

// This file consolidates the two per-call tool layers (DM4 chunk 2): the
// fatal-error interception wrapper and the per-call provider-options wrapper.

// Rush relies on a tool's non-nil Go error ending the turn and coming back
// from agent.Stream (ask_question, peak-hours and cancellation paths, every
// "fatal" tool failure). fantasy v0.45.3+ instead turns that error into an
// ordinary error result and keeps the loop going. fatalToolTool restores the
// contract at the one boundary Rush owns: the error is recorded, the step
// stops the turn via StopTurn, and runTurn returns the recorded error.

// FatalToolCollector keeps the first fatal tool error of one turn. This is
// the only exported concrete type of the wrapper layer: the staying turn
// wiring in package agent holds and drains it directly (field type and
// Take()).
type FatalToolCollector struct {
	mu  sync.Mutex
	err error
}

func (c *FatalToolCollector) Record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

// Take returns and clears the recorded error, so a stall-retried attempt
// starts clean.
func (c *FatalToolCollector) Take() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	c.err = nil
	return err
}

type fatalToolTool struct {
	inner     fantasy.AgentTool
	collector *FatalToolCollector
}

func (t *fatalToolTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *fatalToolTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.inner.Run(ctx, call)
	if err == nil {
		return resp, nil
	}
	t.collector.Record(err)
	stop := fantasy.NewTextErrorResponse(err.Error())
	stop.Metadata = resp.Metadata
	stop.StopTurn = true
	return stop, nil
}

func (t *fatalToolTool) ProviderOptions() fantasy.ProviderOptions { return t.inner.ProviderOptions() }

func (t *fatalToolTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

// WithFatalToolErrors returns a copy of tools whose Go errors end the turn
// and are recorded in c. The shared tool objects are never mutated.
func WithFatalToolErrors(tools []fantasy.AgentTool, c *FatalToolCollector) []fantasy.AgentTool {
	if len(tools) == 0 || c == nil {
		return tools
	}
	out := slices.Clone(tools)
	for i, tool := range out {
		out[i] = &fatalToolTool{inner: tool, collector: c}
	}
	return out
}

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

// WithProviderOptionsOnLast returns a copy of tools whose LAST element carries
// opts, leaving every shared tool object untouched. Empty/absent options and
// an empty tool list are both no-ops, so callers can hand the result straight
// to fantasy.WithTools.
//
// "Last tool" is the provider contract this implements: Anthropic caches the
// tool definitions block up to and including the last cache_control marker, so
// marking the final tool caches the whole block.
func WithProviderOptionsOnLast(tools []fantasy.AgentTool, opts fantasy.ProviderOptions) []fantasy.AgentTool {
	if len(tools) == 0 || len(opts) == 0 {
		return tools
	}
	out := slices.Clone(tools)
	last := len(out) - 1
	out[last] = &providerOptionsTool{inner: out[last], opts: opts}
	return out
}
