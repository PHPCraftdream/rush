package agentcall

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

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

// CallOptions carries one run's per-call execution policy. The zero value
// of every field means "unset — fall back to the legacy shared-state
// path", matching how RunOverrides' zero values mean "no override" today.
type CallOptions struct {
	// DiskBinding carries this immutable run's once-minted binding HANDLE
	// so copying a CallOptions value borrows (never re-mints or clears)
	// it (CallBindingHandle, call_binding.go; the concrete binding is
	// callDiskBinding, disk_provider_call.go, read through the
	// interface). It
	// is initialized by BindCallDiskProvider when the trusted call state is
	// created — before the options are shared — and only the explicit
	// Call-owner token returned there releases it.
	DiskBinding CallBindingHandle `json:"-"`

	// BuildScope is this trusted call state's closeable build-resource
	// scope (BuildResourceScope, call_binding.go): every
	// construction pin performed for the call registers its Construction
	// release here, and the scope is closed on resolution failure, call
	// rejection, queue cancellation or final call completion. Copies of
	// the options borrow the same scope.
	BuildScope *BuildResourceScope `json:"-"`

	// BuildScopeRetained marks BuildScope as the RETAINED scope of an
	// acknowledged queued call (armQueuedBuildRetention in
	// queued_call_scope.go): a fresh scope holding ONLY that call's extra
	// Construction owners, created at the moment the mailbox accepted the
	// call into its submitted queue. Every queue-exit path closes it through
	// releaseQueuedBuildOwnership; no other scope is ever marked, so a
	// marked scope always belongs to queued-call retention and closing it is
	// exactly the ownership release (close is idempotent). Internal trusted
	// runtime metadata; never serialized.
	BuildScopeRetained bool `json:"-"`

	// MaxAgentDepth is the trusted remaining delegation-edge budget. Its
	// presence is separate from the value so zero remains an enforced cap.
	MaxAgentDepth int
	AgentDepthSet bool

	// ModelRole is the named model slot driving this run (smart, fast,
	// worker, reviewer). Read by workerSubAgentActiveForCall to decide
	// worker-slot preference for sub-agent builds, replacing the
	// coordinator-wide activeModelRole field whose read used to race
	// concurrent SetActiveModelRole calls — including inside buildAgent's
	// async readyWg goroutines, where the shared field could be rewritten
	// between registration and the actual read.
	ModelRole config.SelectedModelType

	// RoleContract is the immutable custom-role snapshot selected for this
	// logical execution. Treat it as immutable after publication; adapters and
	// durable conversion clone it at trusted boundaries. ModelRole remains
	// reserved for built-in policy-driving roles.
	RoleContract *session.RoleContract

	// TimeoutExtendsOnProgress/TimeoutHardCap are this run's stream
	// watchdog policy (batch 8). Read per turn from the call instead of
	// the sessionAgent's shared fields written by SetTimeoutOptions.
	TimeoutExtendsOnProgress bool
	TimeoutHardCap           time.Duration

	// IdleTimeout overrides the per-turn stream watchdog's idle-stall
	// threshold (streamIdleTimeoutDefault / the sessionAgent's shared
	// streamIdleTimeout) for this call. Zero means "no override — fall
	// back to the shared/default path", same convention as the rest of
	// this struct.
	//
	// A positive value ALSO makes an idle-stall fire on this call
	// terminal: shouldRetryTurn/shouldContinueTurn (coordinator_run.go)
	// refuse to transparently retry/continue it, instead of silently
	// absorbing up to streamStallRetriesDefault extra attempts first.
	// This is `rush run --idle-timeout`'s backstop — once the run's own
	// graceful --timeout defaults to disabled, a genuinely stuck
	// invocation must end outright rather than retry into an unbounded
	// wait. Every other caller (web UI, interactive sessions) leaves
	// this zero and keeps today's retry-on-transient-stall behaviour.
	IdleTimeout time.Duration

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

	// TurnStallTimeout is the turn-stall detector's durable-progress
	// threshold for this call (Phase 1: stored on the armed stall clock
	// only; zero falls back to streamIdleTimeoutDefault). Wiring an abort
	// policy is a later phase — see SetTurnStallPolicy.
	TurnStallTimeout time.Duration

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

	// SupervisionDisabled turns off the periodic root-session supervision
	// check-in (design doc §7) for this run only (`rush run
	// --no-supervision`). false (the default) defers to config's
	// SupervisionEnabled.
	SupervisionDisabled bool

	// SupervisionInterval overrides the initial/reset silence interval
	// before a supervision check-in (`rush run --supervision-interval`).
	// Zero means "no override -- use config's SupervisionIntervalMinutes or
	// the built-in 5-minute default".
	SupervisionInterval time.Duration

	// BackgroundJobDefaultTimeout is the opt-in default terminate_and_wake
	// deadline for background bash/run_command calls that pass no explicit
	// timeout (`rush run --job-timeout`, config's
	// background_job_default_timeout_seconds). Zero means "off — no default
	// deadline"; the per-call timeout{} parameter always wins.
	BackgroundJobDefaultTimeout time.Duration

	// FailIfSessionBusy rejects this run instead of queueing it when the
	// session's mailbox is already owned by another turn (sdk.Client's
	// fail-fast contract, #818). Enforced AT the atomic mailbox
	// reservation (submit) so two simultaneous starters cannot both slip
	// into the queue — see mailbox_ownership.go and sessionAgent.Run.
	FailIfSessionBusy bool

	// WebTurnOptIn is an explicit marker for server-initiated web turns and
	// reruns. Unlike OriginWeb, it is not inferred from transport metadata.
	WebTurnOptIn bool

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
	// data that permission.BuildFolderScope compiles and T12 persists
	// across a durable-queue restart; a DiskProvider is arbitrary
	// in-process Go code with no serializable form. Batch work carrying
	// one executes under a PROCESS-BOUND BINDING (R12-H2,
	// disk_provider_binding.go): the batch ingress registers the provider
	// (registerDiskBinding - canonical OS disk and nil map to no binding),
	// stamps the opaque ID into Policy.DiskBindingID, and the executor
	// resolves it through the registry before any leaf side effect; a lost
	// binding fails closed (ErrDiskProviderUnavailable), never OS-disk
	// promotion. Durable queue handoff/rebuild of a provider-dependent
	// SESSION call is still refused (ErrDiskProviderNotDurable,
	// disk_provider_durable.go).
	//
	// It does NOT affect the legacy single-target file tools
	// (view/glob/grep/ls/write/edit/multiedit), bash, download, git_read,
	// agentic_fetch or MCP tools, all of which keep hitting the real disk
	// regardless of this field.
	DiskProvider tools.DiskProvider
	// DiskBindingID is the opaque required binding registered for this
	// immutable CallOptions incarnation. It is internal trusted runtime
	// metadata and is never serialized with session calls.
	DiskBindingID string
	// Requirement is the durable execution requirement envelope stamped
	// onto this trusted call state at admission (R12-H2b): the fence
	// row's identity this invocation established. Copies of the options
	// borrow the pointer; it is immutable after construction. nil for
	// ordinary calls.
	Requirement *session.SessionExecutionRequirement
}

// WebModelPolicy reports whether this call explicitly opted into web-turn
// model selection policy. The marker is carried in CallOptions so it survives
// durable queue serialization; OriginWeb alone does not imply this policy.
func WebModelPolicy(ctx context.Context) bool {
	opts := callOptionsFrom(ctx)
	return opts != nil && opts.WebTurnOptIn && opts.ModelRole != config.SelectedModelTypeReviewer
}

// inheritWebPolicyCallOptions transfers only the explicit web-policy marker
// to an independently driven child call; parent execution policy is not inherited.
func inheritWebPolicyCallOptions(ctx context.Context) *CallOptions {
	if !WebModelPolicy(ctx) {
		return nil
	}
	return &CallOptions{WebTurnOptIn: true}
}

type sessionModelPersistenceContextKey struct{}

type sessionModelPersistence struct {
	smart *session.ModelSlotUpdate
	fast  *session.ModelSlotUpdate
}

// WithSessionModelPersistence carries explicit session-model preference writes
// to the eventual SessionAgentCall without making them shared coordinator
// state. Invocation-only rush run model/effort overrides are not persisted.
// The values are copied before being attached to the context.
func WithSessionModelPersistence(ctx context.Context, smart, fast *session.ModelSlotUpdate) context.Context {
	copyUpdate := func(update *session.ModelSlotUpdate) *session.ModelSlotUpdate {
		if update == nil {
			return nil
		}
		copy := *update
		return &copy
	}
	return context.WithValue(ctx, sessionModelPersistenceContextKey{}, sessionModelPersistence{
		smart: copyUpdate(smart),
		fast:  copyUpdate(fast),
	})
}

func sessionModelPersistenceFrom(ctx context.Context) (smart, fast *session.ModelSlotUpdate) {
	persistence, _ := ctx.Value(sessionModelPersistenceContextKey{}).(sessionModelPersistence)
	return persistence.smart, persistence.fast
}

// roleEffortContextKey carries an explicit non-smart role run's --effort to
// the role-slot resolution (plan R6). Transient by construction: it rides
// the invocation's ctx, never CallOptions and never the session, so a
// replayed durable call carries no stale effort.
type roleEffortContextKey struct{}

// WithRoleReasoningEffort attaches the explicit role-run effort to ctx.
func WithRoleReasoningEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, roleEffortContextKey{}, effort)
}

// roleReasoningEffortFrom returns the attached role-run effort, "" when none.
func roleReasoningEffortFrom(ctx context.Context) string {
	effort, _ := ctx.Value(roleEffortContextKey{}).(string)
	return effort
}

type modelOverridesContextKey struct{}

type modelOverridesContextValue struct {
	smart *ModelOverride
	fast  *ModelOverride
}

// WithModelOverrides carries resolved model choices through credentialed
// ExecuteRun calls without touching coordinator-wide model state.
func WithModelOverrides(ctx context.Context, smart, fast *ModelOverride) context.Context {
	copyOverride := func(override *ModelOverride) *ModelOverride {
		if override == nil {
			return nil
		}
		copy := *override
		return &copy
	}
	return context.WithValue(ctx, modelOverridesContextKey{}, modelOverridesContextValue{
		smart: copyOverride(smart),
		fast:  copyOverride(fast),
	})
}

func modelOverridesFrom(ctx context.Context) (smart, fast *ModelOverride) {
	overrides, _ := ctx.Value(modelOverridesContextKey{}).(modelOverridesContextValue)
	return overrides.smart, overrides.fast
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
	if o != nil && o.BuildScope == nil {
		EnsureCallBuildScope(o)
	}
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

// CallOptionsFrom is the exported read side of callOptionsFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func CallOptionsFrom(ctx context.Context) *CallOptions { return callOptionsFrom(ctx) }

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

// FolderScopeSpecFrom is the exported read side of folderScopeSpecFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func FolderScopeSpecFrom(ctx context.Context) *permission.FolderScopeSpec {
	return folderScopeSpecFrom(ctx)
}

// ModelOverridesFrom is the exported read side of modelOverridesFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func ModelOverridesFrom(ctx context.Context) (smart, fast *ModelOverride) {
	return modelOverridesFrom(ctx)
}

// SessionModelPersistenceFrom is the exported read side of sessionModelPersistenceFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func SessionModelPersistenceFrom(ctx context.Context) (smart, fast *session.ModelSlotUpdate) {
	return sessionModelPersistenceFrom(ctx)
}

// RoleReasoningEffortFrom is the exported read side of roleReasoningEffortFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func RoleReasoningEffortFrom(ctx context.Context) string { return roleReasoningEffortFrom(ctx) }

// InheritWebPolicyCallOptions is the exported read side of inheritWebPolicyCallOptions: staying
// package-agent callers delegate here (design §3.1 contract reads).
func InheritWebPolicyCallOptions(ctx context.Context) *CallOptions {
	return inheritWebPolicyCallOptions(ctx)
}

// CallOptionsContextKey is the exported alias of callOptionsContextKey: staying
// package-agent callers delegate here (design §3.1 contract reads).
type CallOptionsContextKey = callOptionsContextKey

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

// RunAllowlistFrom is the exported read side of runAllowlistFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func RunAllowlistFrom(ctx context.Context) *permission.RunAllowlist { return runAllowlistFrom(ctx) }

// RunAllowlistSpecFrom is the exported read side of runAllowlistSpecFrom: staying
// package-agent callers delegate here (design §3.1 contract reads).
func RunAllowlistSpecFrom(ctx context.Context) *permission.RunAllowlistSpec {
	return runAllowlistSpecFrom(ctx)
}

// Run-scoped, CLI-origin-gated rate-limit diagnostic sink: replaces the
// app-wide process-stderr callback with a context-carried writer attached
// per run in app.prepareExecuteRun.

// rateLimitNotifySinkKey is the context key for the per-run diagnostic
// sink. Unexported: only WithRateLimitNotifySink can set it.
type rateLimitNotifySinkKey struct{}

// WithRateLimitNotifySink attaches the run's diagnostic writer to the
// turn context. A nil writer is a no-op so callers can gate on their own
// predicate without a second branch here.
func WithRateLimitNotifySink(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitNotifySinkKey{}, w)
}

// RateLimitNotifySinkFrom returns the run-scoped rate-limit diagnostic
// sink, or nil when this run carries none (non-CLI origins, library
// callers, direct coordinator use).
func RateLimitNotifySinkFrom(ctx context.Context) io.Writer {
	if ctx == nil {
		return nil
	}
	w, _ := ctx.Value(rateLimitNotifySinkKey{}).(io.Writer)
	return w
}

// readyGateContextKey names the context value used to scope asynchronous
// construction tasks to one rebuild or per-call tool build.
type readyGateContextKey struct{}

// WithReadyGate installs gate as the context-local asynchronous build group.
func WithReadyGate(ctx context.Context, gate *ReadyGate) context.Context {
	return context.WithValue(ctx, readyGateContextKey{}, gate)
}

// ReadyGateFor returns the context-local build group when present, otherwise
// the coordinator's initialization gate.
func ReadyGateFor(ctx context.Context, fallback *ReadyGate) *ReadyGate {
	if gate, ok := ctx.Value(readyGateContextKey{}).(*ReadyGate); ok && gate != nil {
		return gate
	}
	return fallback
}

// ReadyGate is the coordinator's "agent build still in flight" barrier: every
// asynchronous piece of agent construction (buildAgent's system-prompt build
// and its tool build) registers here, and every entry point that is about to
// USE the built agent — Run, RunWithOverrides, RunWithCredentials,
// RunWithReservedOwnership, the interrupt paths — waits on it first.
//
// It used to be an errgroup.Group, and that was a real, reportable data race.
// errgroup documents both "A Group should not be reused for different tasks"
// and "The first call to Go must happen before a Wait", and this coordinator
// violates both by construction:
//
//   - the SAME gate is registered on every time an agent is (re)built —
//     once from NewCoordinator, and then again from every UpdateModels, via
//     buildTools -> agentTool -> buildAgent;
//   - every run entry point calls Wait, and internal/app's ExecuteRun calls
//     UpdateModels on its own goroutine right before starting the turn.
//
// One *App / one sdk.Client may have several runs in flight at once (that is
// the whole point of sdk.Client.RunWithCredentials' multi-tenant story), so
// run A's Wait can be parked as the FIRST waiter on the group's WaitGroup
// while run B's UpdateModels drives buildAgent into a fresh registration
// after the counter has already drained back to zero. sync.WaitGroup models
// exactly that pair as a write of &wg.sema (Wait, first waiter) against a
// read of &wg.sema (Add, on the 0 -> N transition) precisely so the detector
// catches it: -race reports a genuine DATA RACE between
// errgroup.(*Group).Wait and errgroup.(*Group).Go. In a build without -race
// the same interleaving can instead trip WaitGroup's own
// "sync: WaitGroup misuse: Add called concurrently with Wait" panic, so this
// is a production defect, not a test artifact. errgroup.Wait's unsynchronized
// `return g.err` racing a later task's `g.err = err` is the same bug again in
// a second field.
//
// A counter plus a sync.Cond has none of those constraints: registrations and
// waits may overlap freely and repeatedly, and — importantly — a task may
// register MORE work while it is itself running (buildAgent's tool task calls
// agentTool, which builds the sub-agent and registers two further tasks on
// this same gate) without deadlocking a concurrent waiter, because Cond.Wait
// releases the mutex while it blocks. A plain mutex around Go/Wait would
// deadlock on exactly that re-entrant registration.
//
// The zero value is ready to use. Semantics deliberately match the errgroup
// this replaces: Wait blocks until every task registered so far has returned,
// returns the FIRST non-nil error, and keeps returning it on later calls;
// panics inside a task are not recovered (they crash the process, as before),
// but the pending count is released first so a concurrent Wait cannot be
// wedged forever by one.
type ReadyGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending int
	err     error
}

// condLocked returns the gate's condition variable, creating it on first use.
// Caller must hold mu — which is also the Locker the Cond is built on, so a
// waiter releases it for the duration of Cond.Wait.
func (g *ReadyGate) condLocked() *sync.Cond {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
	return g.cond
}

// Go runs f in a new goroutine and counts it as pending until it returns.
// Safe to call at any time, including concurrently with Wait and from inside
// another task already registered on this gate.
func (g *ReadyGate) Go(f func() error) {
	g.mu.Lock()
	g.pending++
	g.mu.Unlock()

	go func() {
		var err error
		// Deferred so a panicking task releases its slot before unwinding;
		// the panic itself still propagates, exactly like errgroup.
		defer func() {
			g.mu.Lock()
			g.pending--
			if err != nil && g.err == nil {
				g.err = err
			}
			if g.pending == 0 {
				g.condLocked().Broadcast()
			}
			g.mu.Unlock()
		}()
		err = f()
	}()
}

// PendingCount reports how many tasks are currently registered and not yet
// returned. Test-support accessor only (e.g. asserting that exactly the
// held task remains pending before cancelling under it); production code
// must rely on Wait, not on polling this counter.
func (g *ReadyGate) PendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

// Wait blocks until no task is pending and returns the first error any task
// has reported so far (sticky, as with errgroup).
func (g *ReadyGate) Wait() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.pending > 0 {
		g.condLocked().Wait()
	}
	return g.err
}
