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

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
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

	// TimeoutOptionsSet marks the two timeout fields above as THIS
	// call's deliberate policy (R3-6). Without it they are
	// indistinguishable from "unset": a non-nil CallOptions with both
	// fields zero could mean "no extension, no cap, on purpose" or
	// "caller never configured timeouts", and the turn loop could not
	// tell them apart — it fell back to the SHARED SetTimeoutOptions
	// fields, so a call asking for no watchdog policy could execute
	// under another run's. When the bit is set, the two fields are
	// authoritative even when both are zero; when it is clear (and for
	// a nil CallOptions) the legacy shared fields apply, exactly as
	// before. ExecuteRun derives the bit from its overrides because
	// RunOverrides' zero values mean "no override" and cannot express a
	// deliberate zero; constructors that need that (in-process callers,
	// tests) set it explicitly.
	TimeoutOptionsSet bool

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

	// FolderScope scopes this call's filesystem toolset. When non-nil,
	// applyCallFolderScope rebuilds AllowedTools from the scope: the
	// legacy single-target file tools and the escape-hatch tools are
	// stripped, only the fs_* tools whose operation the scope grants
	// survive, the command tools follow FolderScope.KeepsCommandTools,
	// and MCP tools are excluded. The pointed-to value is compiled by
	// permission.BuildFolderScope and immutable; like the rest of this
	// struct it must not be mutated after WithCallOptions. nil (the
	// default) is an unscoped call: existing callers keep today's
	// toolset.
	FolderScope *permission.FolderScope

	// DiskProvider redirects THIS call's fs_* filesystem I/O to a
	// caller-supplied implementation instead of the real disk: every
	// stat, symlink resolution, read, write, delete, directory listing,
	// name search and content search the eight fs_* tools perform goes
	// through it, including the path resolution the folder-scope check
	// runs on. nil (the default) is the real filesystem, so every
	// existing caller is unchanged.
	//
	// Deliberately NOT symmetrical with FolderScope above: FolderScope is
	// data that permission.BuildFolderScope compiles and a later task
	// persists across a durable-queue restart; a DiskProvider is
	// arbitrary in-process Go code with no serializable form. A call
	// carrying one must therefore never reach the durable queue — that
	// refusal is a LATER task, not implemented here.
	//
	// It does NOT affect the legacy single-target file tools
	// (view/glob/grep/ls/write/edit/multiedit), bash, download, git_read,
	// agentic_fetch or MCP tools, all of which keep hitting the real disk
	// regardless of this field.
	DiskProvider tools.DiskProvider
}

// callOptionsContextKey is the unexported context key carrying *CallOptions.
type callOptionsContextKey struct{}

// WithCallOptions returns a context carrying o. o must not be mutated
// afterwards: it is read concurrently by the run's goroutines.
//
// It also mirrors the one prompt-relevant fact of o — whether the call is
// folder-scoped — under the prompt package's own context key: prompt
// cannot read callOptionsContextKey (unexported here; it would import
// agent, which imports prompt), and deriving the flag at the prompt side
// is impossible. Doing it HERE means every present and future attach
// point gets the same answer, and a scoped call's coder prompt always
// matches the scoped toolset built from the same CallOptions.
func WithCallOptions(ctx context.Context, o *CallOptions) context.Context {
	scoped := o != nil && o.FolderScope != nil
	return context.WithValue(
		prompt.WithFolderScoped(ctx, scoped),
		callOptionsContextKey{}, o,
	)
}

// callOptionsFrom returns the per-call options carried by ctx, or nil when
// the context has none (legacy caller — every consumer then falls back to
// the shared Set*-state path).
func callOptionsFrom(ctx context.Context) *CallOptions {
	o, _ := ctx.Value(callOptionsContextKey{}).(*CallOptions)
	return o
}

// watchdogTimeoutPolicyForCall resolves the stream watchdog's
// (extendsOnProgress, hardCap) policy for one call: the call's own values
// when it carries a TimeoutOptionsSet CallOptions (even all-zero — a
// deliberate policy, R3-6), otherwise the sessionAgent's shared
// SetTimeoutOptions fields for legacy callers.
func (a *sessionAgent) watchdogTimeoutPolicyForCall(call *CallOptions) (extendsOnProgress bool, hardCap time.Duration) {
	if call != nil && call.TimeoutOptionsSet {
		return call.TimeoutExtendsOnProgress, call.TimeoutHardCap
	}
	return a.timeoutExtendsOnProgress, a.timeoutHardCap
}

// folderScopeSpecContextKey is the unexported context key carrying
// *permission.FolderScopeSpec.
type folderScopeSpecContextKey struct{}

// WithFolderScopeSpec returns a context carrying the UNCOMPILED
// folder-scope spec for this one call, alongside the compiled matcher
// inside the CallOptions carried by WithCallOptions. Only ExecuteRun
// attaches it: its presence on a serialized call is what lets
// RebuildSessionAgentCall recompile the scope for a durable restart
// (T12), since CallOptions itself is json:"-" and never persists. nil
// arms nothing.
func WithFolderScopeSpec(ctx context.Context, spec *permission.FolderScopeSpec) context.Context {
	return context.WithValue(ctx, folderScopeSpecContextKey{}, spec)
}

// folderScopeSpecFrom returns the per-call folder-scope spec carried by
// ctx, or nil when the context has none (legacy caller, unscoped call).
func folderScopeSpecFrom(ctx context.Context) *permission.FolderScopeSpec {
	spec, _ := ctx.Value(folderScopeSpecContextKey{}).(*permission.FolderScopeSpec)
	return spec
}
