// Per-call execution options (review R1-1, P0): the immutable per-call
// counterpart of the coordinator's shared Set*-state.
//
// Historically `rush run` pinned per-invocation settings (model role,
// cost/token caps, peak-hours bypass, timeout policy, restricted-run
// policy, sub-agent ban) onto SHARED coordinator/service fields via the
// Set* methods immediately before Run. That was sound for a one-shot
// single-run process, but this coordinator drives N concurrent runs (web
// server, sdk.Client): two overlapping Run/RunWithCredentials calls raced
// for those fields, and a call could execute under another call's policy.
//
// CallOptions flips the default for ExecuteRun: the caller builds ONE
// immutable value, attaches it to the run's context with WithCallOptions,
// and every consumer reads its OWN copy — the shared Set* fields remain
// as the fallback path for legacy callers (they are still fully
// functional; see runInternal and workerSubAgentActive for the
// precedence rules).
//
// The value must be treated as immutable after construction: it is read
// from several goroutines (turn loop, async agent builds) that may
// outlive the ExecuteRun call frame.
package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

// CallOptions carries one run's per-call execution policy. The zero value
// of every field means "unset — fall back to the legacy shared-state
// path", matching how RunOverrides' zero values mean "no override" today.
type CallOptions struct {
	// ModelRole is the named model slot driving this run (smart, fast,
	// worker, reviewer). Read by workerSubAgentActiveForCall to decide
	// worker-slot preference for sub-agent builds, replacing the
	// coordinator-wide activeModelRole field whose read used to race
	// concurrent SetActiveModelRole calls — including inside buildAgent's
	// async readyWg goroutines, where the shared field could be rewritten
	// between registration and the actual read.
	ModelRole config.SelectedModelType

	// TimeoutExtendsOnProgress/TimeoutHardCap are this run's stream
	// watchdog policy (batch 8). Read per turn from the call instead of
	// the sessionAgent's shared fields written by SetTimeoutOptions.
	TimeoutExtendsOnProgress bool
	TimeoutHardCap           time.Duration

	// MaxCost/MaxTokens abort this run when the session exceeds them
	// (batch 30). Threaded onto SessionAgentCall exactly like the legacy
	// SetRunLimits path does, but sourced per call.
	MaxCost   float64
	MaxTokens int64

	// AllowPeakHours bypasses this provider's peak_hours refusal for this
	// one run (the --allow-peak-hours flag). Consumed by runInternal
	// before checkPeakHours; the shared one-shot flag is untouched.
	AllowPeakHours bool

	// DisableSubAgents strips the delegation tools (agent, agentic_fetch)
	// from the TOP-LEVEL coder toolset for this run only
	// (--agents single). Replaces the published-config mutation that used
	// to race a concurrent run's toolset build.
	DisableSubAgents bool

	// FailIfSessionBusy rejects this run instead of queueing it when the
	// session's mailbox is already owned by another turn (sdk.Client's
	// fail-fast contract, #818). Enforced AT the atomic mailbox
	// reservation (submit) so two simultaneous starters cannot both slip
	// into the queue — see mailbox_ownership.go and sessionAgent.Run.
	FailIfSessionBusy bool
}

// callOptionsContextKey is the unexported context key carrying *CallOptions.
type callOptionsContextKey struct{}

// WithCallOptions returns a context carrying o. o must not be mutated
// afterwards: it is read concurrently by the run's goroutines.
func WithCallOptions(ctx context.Context, o *CallOptions) context.Context {
	return context.WithValue(ctx, callOptionsContextKey{}, o)
}

// callOptionsFrom returns the per-call options carried by ctx, or nil when
// the context has none (legacy caller — every consumer then falls back to
// the shared Set*-state path).
func callOptionsFrom(ctx context.Context) *CallOptions {
	o, _ := ctx.Value(callOptionsContextKey{}).(*CallOptions)
	return o
}
