package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/format"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/charmbracelet/x/term"
)

// runExecSetup bundles the values ExecuteRun's request-to-config setup
// computes and everything after it reads. Every field here is written
// exactly once, inside prepareExecuteRun, and never reassigned afterward
// -- verified by grep across the rest of ExecuteRun before this split, so
// bundling them into a struct returned once changes nothing about when or
// how each value is produced. Split out of app_run.go's ExecuteRun when
// the 1000-line-per-function limit landed; ExecuteRun itself was 1197
// lines, of which this setup was ~300.
type runExecSetup struct {
	stdout, stderr io.Writer
	credsRunner    credentialsRunner

	smartOverride, fastOverride             *agent.ModelOverride
	modelOverrideRequested                  bool
	persistedSmartModel, persistedFastModel *session.ModelSlotUpdate

	spinner     *format.Spinner
	stopSpinner func()
	stderrTTY   bool
	progress    bool

	runSpec         permission.RunAllowlistSpec
	folderScope     *permission.FolderScope
	folderScopeSpec *permission.FolderScopeSpec

	failIfSessionBusy bool
	callOpts          *agent.CallOptions
}

// prepareExecuteRun performs ExecuteRun's request-to-config setup: context
// tagging, per-call credential validation, model-override resolution, the
// spinner, the MCP-init wait, the run-allowlist and folder-scope
// compilation, and the immutable per-call CallOptions.
//
// Returns the (possibly wrapped) ctx and its cancel func: the caller owns
// deferring cancel(), matching the single `defer cancel()` this block
// carried before the split. On error, cancel has already been fired by
// this function's own deferred cleanup -- the same "cancel exactly once,
// immediately on any early return, otherwise whenever the caller's own
// deferred call fires" behaviour context.WithCancel plus a single
// `defer cancel()` gave the block when it still lived inline.
//
// Every error return passes the real cancel through rather than nil: the
// deferred cleanup below reads the SAME named-return variable, so an
// explicit `nil` here would overwrite it before the defer runs and the
// defer would then call a nil func -- a real panic this function hit
// once, caught by the package's own -race suite. The caller has nothing
// to do with the returned cancel on the error path; it is only ever
// meant to be deferred on success.
func (app *App) prepareExecuteRun(ctx context.Context, req RunRequest) (_ context.Context, cancel context.CancelFunc, setup *runExecSetup, err error) {
	overrides := req.Overrides
	hideSpinner := req.HideSpinner
	smartModel := overrides.SmartModel
	fastModel := overrides.FastModel

	stdout := io.Writer(io.Discard)
	if req.Stdout != nil {
		stdout = req.Stdout
	}
	stderr := io.Writer(io.Discard)
	if req.Stderr != nil {
		stderr = req.Stderr
	}

	ctx, cancel = context.WithCancel(ctx)
	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()

	// Fork patch: batch 14 — mark the agent context as non-interactive.
	// cliprovider.Stream reads this and forces bypass-permissions on the
	// inner CLI sub-process (claude / codex / gemini) so it doesn't hang
	// waiting for a permission prompt that no human is there to answer.
	// See cliprovider.NonInteractiveContextKey.
	ctx = context.WithValue(ctx, cliprovider.NonInteractiveContextKey, true)
	// Tag the entry-channel origin on the turn context: resolveSession
	// reads it to persist the origin on a freshly created session, and
	// buildCall stamps it onto the call so createUserMessage persists it
	// on the user message. Empty/unspecified leaves both untouched.
	ctx = agent.WithCallOrigin(ctx, req.Origin)

	// Per-call credentials (sdk.Client.RunWithCredentials): validate the
	// bundle before any session work so a malformed set fails fast, and
	// keep the credentials-capable coordinator for the run handoff below.
	var credsRunner credentialsRunner
	if req.Credentials != nil {
		if err := req.Credentials.Validate(); err != nil {
			return nil, cancel, nil, fmt.Errorf("invalid per-call credentials: %w", err)
		}
		cr, ok := app.AgentCoordinator.(credentialsRunner)
		if !ok {
			return nil, cancel, nil, fmt.Errorf("per-call credentials are not supported by this coordinator")
		}
		credsRunner = cr
	}

	smartOverride, fastOverride, err := app.resolveModelOverridesForNonInteractive(smartModel, fastModel)
	if err != nil {
		return nil, cancel, nil, fmt.Errorf("failed to override models: %w", err)
	}
	modelOverrideRequested := smartOverride != nil || fastOverride != nil
	var persistedSmartModel, persistedFastModel *session.ModelSlotUpdate
	if smartOverride != nil {
		persistedSmartModel = &session.ModelSlotUpdate{Provider: smartOverride.Provider, Model: smartOverride.Model}
	}
	if fastOverride != nil {
		persistedFastModel = &session.ModelSlotUpdate{Provider: fastOverride.Provider, Model: fastOverride.Model}
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		spinner = format.NewSpinner()
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for MCP initialization to complete before reading MCP tools. A
	// library-mode App deliberately has no MCP owner, so it must not observe
	// or wait on the application mode's process-wide initialization barrier.
	if app.mcpOwner != nil {
		if err := app.mcpOwner.WaitForInit(ctx); err != nil { // bound to this App's own owner, not the installed owner's barrier
			return nil, cancel, nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
		}
	}

	// T10 (SDK folder scopes): the restricted-run spec merge is assembled
	// HERE, before the per-call CallOptions below is frozen, because the
	// folder scope's KeepCommandTools decision and the fs_* AllowTools
	// append both derive from it. This is the same pure config+overrides
	// merge that used to sit just above BuildRunAllowlist further down;
	// everything with side effects (BuildRunAllowlist, the session
	// baseline pin, the WithRunAllowlist ctx attachments) stays at its
	// original spot because it needs the resolved session and the final
	// ctx.
	runSpec := runAllowlistSpecFromConfig(app.config.Config().Permissions)
	if overrides.RestrictedRun {
		runSpec.Restrict = true
	}
	runSpec.AllowBash = append(runSpec.AllowBash, overrides.AllowBash...)
	runSpec.AllowTools = append(runSpec.AllowTools, overrides.AllowTools...)
	if runSpec.Restrict {
		// Delegation is the parent turn's policy-carrying mechanism; the
		// child turn is separately gated with the inherited allowlist.
		// Keep delegation tools dispatchable so their child permission
		// requests can produce the authoritative decided notification.
		for _, name := range subAgentToolNames {
			if !slices.Contains(runSpec.AllowTools, name) {
				runSpec.AllowTools = append(runSpec.AllowTools, name)
			}
		}
	}

	// R14-2 (P0, SDK review round 14): a config with no real workspace
	// (sdk.ModeLibrary with no WorkingDir) must never resolve per-call
	// FolderScopes against the real OS disk. The R6-1 sentinel guard
	// (tools.rejectRealDiskUnderLibraryVirtualRoot) is lexical and only
	// catches paths under the sentinel root: an ABSOLUTE scope Dir
	// pointing at any real host directory is joined through SmartJoin
	// unchanged and canonicalizes straight through OSDisk. Enforce the
	// README's precondition HERE, before any canonicalization or
	// provider traffic: on such a config, FolderScopes require a custom
	// (non-OSDisk) DiskProvider, full stop. The opposite-direction
	// checks (DiskProvider without FolderScopes, and DiskProvider with
	// a command-keeping scope) are kept unchanged below.
	if cfgOpts := app.config.Config().Options; cfgOpts != nil && cfgOpts.NoRealWorkspace &&
		len(overrides.FolderScopes) > 0 &&
		!tools.IsCustomDiskProvider(overrides.DiskProvider) {
		return nil, cancel, nil, errors.New(
			"folder scopes on a session with no real working directory require a custom DiskProvider: " +
				"RunOverrides.FolderScopes is set but DiskProvider is nil or the real OS disk, and an " +
				"ephemeral session has no real filesystem to resolve the scope against")
	}

	// T10: a non-empty FolderScopes scopes this call's filesystem
	// toolset. A malformed entry is a HARD error, deliberately unlike the
	// allowErr handling further down: dropping a bad run-allowlist
	// pattern only narrows access (safe to log and skip), but a
	// folder-scope entry can be a deny carve-out, and silently dropping
	// one would WIDEN access — BuildFolderScope refuses the whole spec
	// rather than guess, and this run fails before any session work or
	// provider traffic.
	var folderScope *permission.FolderScope
	var folderScopeSpec *permission.FolderScopeSpec
	if len(overrides.FolderScopes) > 0 {
		spec := permission.FolderScopeSpec{
			WorkingDir: app.config.WorkingDir(),
			Entries:    overrides.FolderScopes,
			// Command-executing tools stay in a scoped toolset only when
			// the restricted-run gate is armed AND this run grants actual
			// command patterns: keeping them is a deliberate grant (their
			// commands stay subject to those patterns), not a default.
			KeepCommandTools: runSpec.Restrict && len(runSpec.AllowBash) > 0,
		}
		// R5-2 (P0 security review): canonicalize every entry (and
		// WorkingDir itself) through the SAME longest-existing-prefix +
		// EvalSymlinks algorithm and the SAME DiskProvider used to resolve
		// every REQUESTED item path, before compiling the matcher. Without
		// this a scope's roots were matched in lexical form while
		// requested paths were matched after symlink resolution, so a
		// nested deny carve-out could stop matching while its broader
		// parent grant kept matching — see
		// tools.CanonicalizeFolderScopeSpec's doc comment. A
		// canonicalization failure is a hard error here, exactly like the
		// BuildFolderScope compile failure below: the run never starts, so
		// there is nothing to fail open into.
		canonSpec, err := tools.CanonicalizeFolderScopeSpec(ctx, overrides.DiskProvider, spec)
		if err != nil {
			return nil, cancel, nil, fmt.Errorf("invalid folder scopes: %w", err)
		}
		compiledScope, err := permission.BuildFolderScope(canonSpec)
		if err != nil {
			return nil, cancel, nil, fmt.Errorf("invalid folder scopes: %w", err)
		}
		folderScope = &compiledScope
		// T12: keep the exact RAW (pre-canonicalization) spec the compiled
		// scope came from so it can travel on the context below and be
		// persisted on any durable run-queue row this call produces. A
		// durable restart re-canonicalizes it against the real disk (see
		// RebuildSessionAgentCall) — a DiskProvider is never persisted, so
		// persisting the already-canonicalized form here would buy
		// nothing and would go stale if the on-disk symlink structure
		// changes before a restart.
		folderScopeSpec = &spec
		// Mandatory restricted-run companion: under RestrictedRun an
		// EMPTY AllowTools table denies every plain (non-command) tool
		// (RunAllowlist.toolAllowed), so "scoped + restricted" would deny
		// the very first fs_* write at the permission gate and silently
		// make the two features incompatible. Append exactly the fs_*
		// names this scope grants so the run gate and the scoped toolset
		// agree; the fs_* tools still judge every item against the scope
		// themselves. The command tools need no entry: their requests are
		// command-shaped and governed by AllowBash alone.
		runSpec.AllowTools = append(runSpec.AllowTools, fsToolsForScope(folderScope)...)
	}

	// #859: two hard errors for RunOverrides.DiskProvider, both before
	// any session work or provider traffic, mirroring "invalid folder
	// scopes" above.
	if tools.HasDiskProvider(overrides.DiskProvider) {
		// (1) A DiskProvider without a FolderScope is a footgun: the
		// legacy single-target file tools (view/write/edit/multiedit/
		// glob/grep/ls) stay in the toolset when there is no scope to
		// strip them (applyCallFolderScope only runs when
		// CallOptions.FolderScope != nil), so the model could read the
		// virtual file with fs_read and overwrite the REAL one with
		// write in the same turn.
		if folderScope == nil {
			return nil, cancel, nil, errors.New("disk provider requires at least one folder scope: " +
				"RunOverrides.DiskProvider is set but RunOverrides.FolderScopes is empty")
		}
		// (2) A DiskProvider combined with a scope that keeps
		// command-executing tools means bash sees the REAL disk while
		// fs_* sees the virtual one in the same turn — an invisible
		// split the model has no way to know about (the prompt does not
		// change for a disk-provider call, see sdk/README.md).
		if folderScope.KeepsCommandTools() {
			return nil, cancel, nil, errors.New("disk provider cannot be combined with a folder scope that keeps command tools: " +
				"bash would see the real disk while the fs_* tools see the virtual one")
		}
	}

	// #859 (Layer 3, §7): a DiskProvider-carrying run never queues behind
	// another turn, even if the caller didn't ask for FailIfSessionBusy.
	// Queueing is what creates the orphan-restart risk Layers 1/2 refuse
	// outright — forcing fail-fast here means a provider-carrying call
	// never reaches that path in the first place.
	failIfSessionBusy := req.FailIfSessionBusy || tools.HasDiskProvider(overrides.DiskProvider)

	// R1-1 (P0): build this run's IMMUTABLE per-call execution context
	// and attach it to the run's context. Everything below that used to
	// pin per-invocation settings onto SHARED coordinator/permission
	// state — SetActiveModelRole, SetAgentTimeoutOptions, SetRunLimits,
	// SetAllowPeakHours, the published-config DisableSubAgents
	// mutation, and the process-wide SetRunAllowlist — now travels in
	// this one value instead. On one *App (web server, sdk.Client) two
	// overlapping runs each carry their own policy: previously they
	// raced for every one of those shared fields, and a run could
	// execute under another run's role, caps, bypass, allowlist or
	// stripped toolset. The coordinator's Set* methods remain the
	// fallback path for legacy (non-ExecuteRun) callers and are
	// deliberately untouched.
	//
	// Attached before session resolution on purpose: the coordinator's
	// per-call toolset build (resolveSessionModels -> buildTools) reads
	// the DisableSubAgents filter and ModelRole from this context, and
	// buildAgent captures the role synchronously at registration time.
	callOpts := &agent.CallOptions{
		ModelRole:                overrides.ModelRole,
		TimeoutExtendsOnProgress: overrides.TimeoutExtendsOnProgress,
		TimeoutHardCap:           overrides.TimeoutHardCap,
		// R3-6: RunOverrides cannot distinguish "flag not passed" from
		// "flag passed as false/0" (plain bool/duration fields; the CLI
		// reads them with GetBool/GetString defaults), so presence is
		// still derived from the same predicate the turn-time check
		// used, and CLI behaviour is unchanged. F3 (2026-09-01 SDK
		// review): an in-process caller can additionally set
		// Overrides.TimeoutOptionsSet to make even an all-zero policy
		// deliberate instead of an inheritance of the sessionAgent's
		// shared fields. The derived predicate stays in the OR because
		// removing it would silently break the CLI's
		// --timeout-extends-on-progress / --timeout-hard-cap flags,
		// whose only presence signal is this predicate. See
		// CallOptions.TimeoutOptionsSet.
		TimeoutOptionsSet: overrides.TimeoutOptionsSet || overrides.TimeoutExtendsOnProgress || overrides.TimeoutHardCap > 0,
		MaxCost:           overrides.MaxCost,
		MaxTokens:         overrides.MaxTokens,
		AllowPeakHours:    overrides.AllowPeakHours,
		DisableSubAgents:  overrides.DisableSubAgents,
		// T10: the compiled folder scope for THIS call (nil = unscoped).
		// The coordinator's applyCallFolderScope rebuilds the toolset
		// from it per call; rejectScopedCallOnCLIProvider (T9) refuses
		// the call outright when the resolved provider is a CLI provider.
		FolderScope: folderScope,
		// #859: the caller-supplied filesystem for THIS call's fs_* tools
		// (nil = real disk). Never persisted: a durably-queued call
		// carrying one is refused outright (agent.ErrDiskProviderNotDurable),
		// never rebuilt with a nil fallback onto the real disk.
		DiskProvider:      overrides.DiskProvider,
		FailIfSessionBusy: failIfSessionBusy,
	}
	ctx = agent.WithCallOptions(ctx, callOpts)
	if modelOverrideRequested {
		ctx = agent.WithSessionModelPersistence(ctx, persistedSmartModel, persistedFastModel)
		ctx = agent.WithModelOverrides(ctx, smartOverride, fastOverride)
	}

	// Fork patch (orchestrator UX): --agents single. The agent /
	// agentic_fetch tools are stripped from the coder's toolset for THIS
	// run only, via CallOptions.DisableSubAgents consumed inside the
	// coordinator's per-build toolset filter (applyCallDisableSubAgents).
	// The former implementation mutated the PUBLISHED coder AllowedTools
	// and restored it via defer (the R3 fix): on one *App that write raced
	// every concurrent run's buildTools for the whole duration of the
	// mutating run, so a DisableSubAgents:false call could observe the
	// delegating call's stripped toolset until the restore fired. Nothing
	// shared is touched anymore — there is nothing to snapshot or restore
	// (shouldBypassSubAgentBan/disableToolsInConfig stay in
	// app_run_gates.go for their direct tests and as the config-level
	// gate utility).
	//
	// Plan phase 2 exception is preserved with identical semantics: when
	// this run is --role smart with a Worker model configured, the
	// filter restores ONLY the `agent` tool — a configured worker means
	// delegation is the intended workflow, even when --agents single was
	// passed explicitly. `agentic_fetch` stays stripped either way.

	// R3-1: the former per-run UpdateModels(ctx) call is GONE. It was the
	// publisher that leaked THIS run's per-call tool filter onto shared
	// state: it rebuilt the coder toolset with this ctx's CallOptions and
	// SetTools'd the result onto the ONE shared currentAgent — before
	// session resolution and before ReserveExclusive — so a same-session
	// fail-fast loser clobbered the winner's live toolset, and calls with
	// opposite DisableSubAgents raced each other's in-flight turns (which
	// re-read the shared slice at every PrepareStep). Per-call toolsets are
	// now built and pinned inside the coordinator's resolveSessionModels;
	// UpdateModels remains only as a global config/MCP refresh and no
	// longer consumes per-call state at all.

	ok = true
	return ctx, cancel, &runExecSetup{
		stdout:                 stdout,
		stderr:                 stderr,
		credsRunner:            credsRunner,
		smartOverride:          smartOverride,
		fastOverride:           fastOverride,
		modelOverrideRequested: modelOverrideRequested,
		persistedSmartModel:    persistedSmartModel,
		persistedFastModel:     persistedFastModel,
		spinner:                spinner,
		stopSpinner:            stopSpinner,
		stderrTTY:              stderrTTY,
		progress:               progress,
		runSpec:                runSpec,
		folderScope:            folderScope,
		folderScopeSpec:        folderScopeSpec,
		failIfSessionBusy:      failIfSessionBusy,
		callOpts:               callOpts,
	}, nil
}
