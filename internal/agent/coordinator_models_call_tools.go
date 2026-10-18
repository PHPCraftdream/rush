package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
)

// ErrScopedCallToolsUnavailable is returned by resolveSessionModelsInternal/
// applyModelOverrides/resolveCredentialsModels/RunSessionAgentCall when
// pinCallTools fails to build a distinct toolset for a call whose
// CallOptions carries FolderScope, DiskProvider, DisableSubAgents, or a
// tool-shaping ModelRole (R5-1, P0; widened R6-3, P1). Every one of those
// options exists specifically to change the shared toolset's composition —
// FolderScope/DiskProvider strip the legacy view/write/edit/glob/grep/ls
// tools, MCP tools, and (usually) sub-agent tools down to a
// restricted/redirected set; DisableSubAgents strips the delegation tools
// (agent, agentic_fetch); a non-empty ModelRole decides worker-tool
// layering/orchestrator-tool stripping via workerSubAgentActiveForCall. A
// build failure for any of them means the CALLER-REQUESTED composition
// could not be built, and the shared toolset is never guaranteed to match
// it (it is built from the coordinator-wide activeModelRole/UpdateModels
// state, not this call's policy). Since an SDK run auto-approves every tool
// call, nothing downstream would catch the mismatched toolset before the
// model could use it — so the call must fail here instead of silently
// falling back, mirroring RunSessionAgentCall's existing "refuse the row"
// pattern (coordinator_interrupt.go, T12) for the durable-restart path.
var ErrScopedCallToolsUnavailable = errors.New(
	"agent: failed to build this call's tool-shaping toolset (folder-scope/disk-provider/disable-sub-agents/model-role); refusing to fall back to the shared toolset")

// scopedCallToolsRequired reports whether ctx carries per-call options that
// make a failed pinCallTools build unsafe to silently fall back from (R5-1;
// widened R6-3): FolderScope and DiskProvider mean the caller specifically
// asked for a restricted/redirected toolset; DisableSubAgents and a
// non-empty ModelRole ALSO decide this call's tool composition (see
// applyCallDisableSubAgents and workerSubAgentActiveForCall/
// buildToolsAgentConfigForCall) — a build failure for either must fail the
// call closed instead of widening/narrowing it to whatever the shared
// toolset happens to be. A legacy call (no CallOptions, or one with none of
// these fields set) keeps today's fallback behavior. Mirrors the trigger
// predicate RunSessionAgentCall uses for the durable-replay rebind
// (coordinator_interrupt.go) — kept as a single shared definition via
// scopedCallOptionsRequireDistinctTools so the two can never drift apart
// again.
func scopedCallToolsRequired(ctx context.Context) bool {
	return scopedCallOptionsRequireDistinctTools(callOptionsFrom(ctx))
}

// scopedCallOptionsRequireDistinctTools is scopedCallToolsRequired's
// struct-based form, usable before a CallOptions has been attached to a
// context (RunSessionAgentCall inspects the durable call's CallOptions
// directly, ahead of calling WithCallOptions).
func scopedCallOptionsRequireDistinctTools(opts *CallOptions) bool {
	return opts != nil && (opts.FolderScope != nil || tools.HasDiskProvider(opts.DiskProvider) ||
		opts.DisableSubAgents || opts.ModelRole != "")
}

// pinCallToolsReadyGateSeam is a test-only hook: fires immediately after the
// buildTools call below, before the readyWg.Wait() drain, letting a test
// register a task on c.readyWg — simulating a concurrent agent build (or
// this call's own async sub-agent build) failing — at exactly the window
// this call's OWN Wait() is about to observe, deterministically and on this
// same goroutine, without a real race. Mirrors runTurnToolsSnapshotSeam's
// pattern (agent_turn.go). nil (a no-op) in every production path.
var pinCallToolsReadyGateSeam func()

// pinCallTools builds the per-call coder toolset (R3-1) from the caller's
// already-pinned cfg snapshot and THIS call's context, then drains any
// agent-build tasks registered on the ready gate. buildTools reads the
// per-call CallOptions (DisableSubAgents, ModelRole) from ctx, so the
// returned slice is scoped to exactly one call; the caller pins it onto the
// SessionAgentCall via resolvedOverrides.pin instead of publishing it to the
// shared agent.
//
// Return contract (R6-3, P1): a (nil, nil) result means "no per-call
// toolset was required — fall back to the shared toolset", the ONLY
// legitimate reason being ctx carries no CallOptions that
// scopedCallOptionsRequireDistinctTools would flag. Every other failure —
// the coder agent unconfigured, buildTools itself erroring, or a
// registered agent build failing on the ready gate — returns a non-nil
// error INSTEAD of a bare nil slice whenever ctx's CallOptions requires a
// distinct toolset (FolderScope, DiskProvider, DisableSubAgents, or a
// tool-shaping ModelRole): a nil slice was ambiguous between "no override
// requested" and "override failed", and R5-1's original version of this
// function pushed that disambiguation onto every caller individually
// (three call sites had to remember to call scopedCallToolsRequired(ctx)
// themselves) — R6-3 found that RunSessionAgentCall's durable-replay call
// site had NOT been widened to do so for DisableSubAgents/ModelRole. Moving
// the check in here means every caller gets the same answer from one place:
// err != nil always means "refuse the call", nil error always means "safe
// to use whatever slice (possibly nil) came back".
//
// For a genuinely legacy call (ctx carries no CallOptions, or one with none
// of those fields set), the same build/ready-gate failures still return
// (nil, nil) — log-only — preserving the pre-R6-3 "fall back to the shared
// toolset" behavior for callers that never asked for a distinct one.
func (c *coordinator) pinCallTools(ctx context.Context, cfg *config.Config) ([]fantasy.AgentTool, error) {
	required := scopedCallToolsRequired(ctx)
	fail := func(err error) ([]fantasy.AgentTool, error) {
		// R12-H2a round 2: a failure AFTER buildTools has already pinned
		// construction owners into the call's build scope (the empty-
		// toolset refusal, the ready-gate drain error) must not leak
		// them. Only the CALL's own scope is closed: callBuildScopeFrom
		// returns nil for bare contexts, so the coordinator's shared
		// scope is never touched here.
		if scope := callBuildScopeFrom(ctx); scope != nil {
			scope.close()
		}
		slog.Warn("Coordinator: failed to build per-call tools", "err", err, "distinct_toolset_required", required)
		if required {
			// Wrap both: callers match the cause (e.g. ErrDiskProviderNotDurable).
			return nil, fmt.Errorf("%w: %w", ErrScopedCallToolsUnavailable, err)
		}
		return nil, nil
	}

	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return fail(errCoderAgentNotConfigured)
	}
	tools, err := c.buildTools(ctx, cfg, agentCfg, false)
	if err != nil {
		return fail(err)
	}
	if pinCallToolsReadyGateSeam != nil {
		pinCallToolsReadyGateSeam()
	}
	if required && len(tools) == 0 {
		return fail(errors.New("per-call toolset is empty"))
	}
	// The toolset embeds a freshly built sub-agent (buildTools -> agentTool
	// -> buildAgent), whose prompt/tool builds run asynchronously on the
	// ready gate. Drain the gate before returning so the turn cannot
	// dispatch a still-under-construction sub-agent — the same ordering the
	// removed per-run UpdateModels publisher used to get from the run entry
	// points' readyWg.Wait.
	if waitErr := c.readyWg.Wait(); waitErr != nil {
		return fail(waitErr)
	}
	return tools, nil
}

// withoutCallOptions returns ctx with any per-call CallOptions removed, so
// downstream builds cannot consume a caller's per-call policy (R3-1).
// UpdateModels is a GLOBAL refresh (config reloads, credential refreshes)
// and must publish only global state even when the caller's context — e.g.
// runWithUnauthorizedRetry's 401-rebuild ctx — carries CallOptions.
func withoutCallOptions(ctx context.Context) context.Context {
	// The mirrored prompt-side scope flag must go with the CallOptions:
	// a stale true here would render the global refresh's sub-builds
	// (an embedded agent tool's prompt, for instance) from a caller's
	// per-call scope, exactly what this function exists to prevent.
	return prompt.WithFolderScoped(
		context.WithValue(ctx, callOptionsContextKey{}, (*CallOptions)(nil)),
		false)
}
