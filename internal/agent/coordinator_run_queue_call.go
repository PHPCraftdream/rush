// Durable run-queue call rebuild and execution for the RunQueuePump.
// Extracted from coordinator_interrupt.go — pure code move, bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
// for run queue pump execution (task #340, ROUND 3 migration).
//
// It reconstructs the live Model objects (with fantasy.LanguageModel and CatwalkCfg)
// from the serialized ModelCfg using the coordinator's provider configs and catwalk registry.
// ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/PresencePenalty are NOT
// reconstructed here — they are pure functions of (Model, ProviderConfig) computed
// via mergeCallOptions during normal execution path.
//
// R12-H2b §2.2 fence transfer: rowID is the leased run-queue entry's durable
// id ("" for callers without one — the row-keyed Continuation holder release
// then becomes a no-op). A row carrying a DiskRequirement resolves/acquires
// its fence BEFORE scope canonicalization or any other step; a legacy row
// marked HostDiskProvider with NO envelope fails closed terminal. A row
// with neither envelope nor HostDiskProvider mark still consults the fence
// with a nil source: a newly originated session-level continuation gets its
// baseline from the persisted session metadata (§2.2), so a fenced session
// cannot be restarted on the OS disk by an unmarked row. On success
// the Continuation owner transfers to a fresh Call owner registered on the
// rebuilt call's build scope; on ANY failure after acquisition both the prep
// token and the row-keyed holder are released, so a refused row never leaks
// the binding pin.
func (c *coordinator) RebuildSessionAgentCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (SessionAgentCall, error) {
	// R12-H2b §2.2: a legacy row says a provider rode the call, but the
	// envelope that names it does not exist — rebuilding could only mean
	// OS disk. Terminal, so the pump never retries a row that can never
	// succeed differently.
	if data.DiskRequirement == nil && data.HostDiskProvider {
		slog.Error("agent: refusing to rebuild a durable row marked host-disk-provider but carrying no resolvable execution requirement; rebuilding it would silently restart the turn on the real disk",
			"session_id", data.SessionID, "logical_call_id", data.LogicalCallID)
		return SessionAgentCall{}, &ErrCallAlreadyAttempted{
			Err: fmt.Errorf("%w: durable row is marked host-disk-provider but carries no resolvable execution requirement (session=%s)", ErrDiskProviderUnavailable, data.SessionID),
		}
	}

	// R12-H2b §2.2: resolve and acquire the fence FIRST, before scope
	// canonicalization or model/tool work, so the binding cannot be pruned
	// under the rebuild. The row-keyed Continuation holder (held by the
	// durable producer since enqueue) is released here in BOTH outcomes —
	// transfer-to-call below, or any refusal after this point.
	var prep *preparedContinuation
	if data.DiskRequirement != nil {
		resolved, prepErr := c.PrepareContinuation(ctx, data.SessionID, data.DiskRequirement)
		if prepErr != nil {
			releaseDurableContinuationOwner(c.diskCaps, data.DiskRequirement.BindingID, rowID)
			return SessionAgentCall{}, &ErrCallAlreadyAttempted{
				Err: fmt.Errorf("%w: durable row's disk provider is unavailable (session=%s): %s", ErrDiskProviderUnavailable, data.SessionID, prepErr),
			}
		}
		prep = resolved
	} else if !data.HostDiskProvider {
		// R12-H2b §2.2 / acceptance 14: a provider-less unmarked continuation
		// is fenced by the persisted session metadata. An unfenced session
		// resolves (nil, nil) and leaves prep nil, so the rebuild is unchanged
		// beyond one PK SELECT.
		resolved, prepErr := c.PrepareContinuation(ctx, data.SessionID, nil)
		if prepErr != nil {
			return SessionAgentCall{}, &ErrCallAlreadyAttempted{
				Err: fmt.Errorf("%w: unmarked continuation of a fenced session cannot resolve its provider (session=%s): %s", ErrDiskProviderUnavailable, data.SessionID, prepErr),
			}
		}
		prep = resolved
	}
	releasePrep := func() {
		if prep == nil {
			return
		}
		bindingID := prep.requirement.BindingID
		prep.release()
		releaseDurableContinuationOwner(c.diskCaps, bindingID, rowID)
	}

	var smartModel, fastModel Model
	var err error

	// Determine which models to rebuild using a single atomic snapshot.
	cfg, _ := c.cfg.Snapshot()
	var smartCfg, fastCfg config.SelectedModel
	if data.SmartModel != nil {
		smartCfg = fromSessionModelCfg(*data.SmartModel)
	} else {
		// Use default config for smart model
		smartCfg = cfg.Models[config.SelectedModelTypeSmart]
	}

	if data.FastModel != nil {
		fastCfg = fromSessionModelCfg(*data.FastModel)
	} else {
		// Use default config for fast model
		fastCfg = cfg.Models[config.SelectedModelTypeFast]
	}

	// Build both models (buildModelsFromCfg requires both)
	smartModel, fastModel, err = c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		releasePrep()
		return SessionAgentCall{}, fmt.Errorf("failed to rebuild models from config: %w", err)
	}

	// cfg here is the SAME atomic snapshot captured above (line ~437) for the
	// smart/fast model rebuild -- NOT a fresh c.cfg.Config() read. A reload
	// landing between the model rebuild above and this provider-options lookup
	// used to be able to hand back provider options from a DIFFERENT config
	// generation than the model itself was built from (task #577/P1-2) -- the
	// entire point of durable recovery is that replaying a call reproduces
	// exactly what was queued, which requires reading provider options from
	// the same generation the model came from.
	//
	// sessionAgent.Run reads ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/
	// PresencePenalty directly off the call (agent.go's fantasy.AgentStreamCall
	// construction) -- it does NOT recompute them from SmartModel itself. Every
	// other call-site populates these via mergeCallOptions before the call ever
	// reaches Run, so we must do the same here or every durably-recovered call
	// silently loses its provider options and sampling knobs.
	smartProviderCfg, _ := cfg.Providers.Get(smartModel.ModelCfg.Provider)
	providerOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(data.SessionID, smartModel, smartProviderCfg)

	// R4-1/R4-2/R4-3: recompile and rebind the call's OWN restricted-run
	// policy from the spec serialized on the durable row. This is what
	// makes the R3-4 per-call arming in runOwned (SetSessionRunAllowlistForCall,
	// keyed by this call's LogicalCallID) apply to durable restarts: each
	// rebuilt call is judged by the policy ITS caller declared, never by a
	// session-wide last-writer-wins baseline, and its sub-agents inherit
	// both auto-approval and the restriction (runSubAgent →
	// InheritSessionRunAllowlist). A nil spec — rows persisted before the
	// spec field existed, or rows queued by non-ExecuteRun callers — arms
	// nothing: such a turn keeps the historical fallback chain (session
	// baseline if one is armed in this process, else the process-wide
	// gate). That is deliberately NOT a synthetic deny-all: web-origin
	// durable calls belong to interactive sessions whose permission
	// requests must still reach the UI, and the gate is only consulted on
	// the auto-approve path anyway. BuildRunAllowlist drops unparseable
	// patterns and reports them; the compiled allowlist stays restricted
	// even then, so a corrupted spec fails closed per pattern.
	var rebuiltRunAllowlist *permission.RunAllowlist
	if data.RunAllowlistSpec != nil {
		compiledRebuilt, compileErr := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
			Restrict:   data.RunAllowlistSpec.Restrict,
			AllowTools: data.RunAllowlistSpec.AllowTools,
			AllowBash:  data.RunAllowlistSpec.AllowBash,
		})
		if compileErr != nil {
			slog.Warn("RebuildSessionAgentCall: dropped invalid restricted-run patterns from the durable row's policy spec",
				"session_id", data.SessionID, "err", compileErr)
		}
		rebuiltRunAllowlist = &compiledRebuilt
	}

	// T12: recompile and rebind the call's OWN folder scope from the spec
	// serialized on the durable row. CallOptions is json:"-" and never
	// survives the queue handoff, so without this a rebuilt scoped call
	// would carry no FolderScope at all and fall back to the shared
	// unscoped toolset — the silent restart promotion T12 exists to
	// prevent. A nil spec arms nothing: unscoped calls, web-origin rows
	// (never folder-scoped today), and pre-migration rows keep the
	// historical fallback unchanged. A spec that FAILS to canonicalize or
	// recompile keeps the zero FolderScope — which denies every operation
	// on every path — so a corrupted or unresolvable row fails CLOSED (a
	// file-blind turn that can still talk) rather than open (an unscoped
	// one with the full legacy file surface). That is the same direction
	// as the run-allowlist handling above, where dropped patterns leave
	// the compiled matcher restricted.
	//
	// R5-2 (P0 security review): the persisted spec is raw (never
	// canonicalized before persisting, see ExecuteRun), so it is
	// canonicalized here with the SAME resolveScopedPath algorithm every
	// REQUESTED item path goes through, exactly like the initial in-process
	// compile does.
	//
	// R12-H2b §2.2: a rebuilt call that carries a fence canonicalizes its
	// scope WITH the resolved provider, not against the real disk — the
	// nil disk argument was only correct while no provider could survive
	// the queue. prep was acquired at the top, so the provider is live
	// here; the OS-disk (nil) argument stays for every unfenced row.
	//
	// R5-3 (P0 security review): this used to be the ONLY thing
	// rebuiltCallOptions ever carried — a rebuilt call's DisableSubAgents,
	// ModelRole and timeout-watchdog policy were silently dropped even
	// though CallOptionsSpec now persists them, because this block never
	// looked at that field at all. Reconstruct the primitive fields FIRST
	// (fromSessionCallOptionsSpec needs no compilation, unlike FolderScope
	// below) and layer the compiled scope on top when the row also
	// declares one, so every replay-relevant field lands on the SAME
	// CallOptions value together. A row carrying neither spec still
	// leaves rebuiltCallOptions nil, exactly as before this fix.
	//
	// R6-4 (P2 security review round 6): fromSessionCallOptionsSpec now
	// refuses a non-nil spec whose Version does not match the current
	// session.CallOptionsSpecVersion instead of partially decoding it —
	// propagate that refusal here rather than swallowing it, so a row
	// written by a newer/older binary's incompatible schema fails the
	// whole restart closed instead of silently running with whatever
	// subset of fields happened to decode.
	var rebuiltCallOptions *CallOptions
	if data.CallOptionsSpec != nil || data.FolderScopeSpec != nil {
		var err error
		rebuiltCallOptions, err = fromSessionCallOptionsSpec(data.CallOptionsSpec)
		if err != nil {
			releasePrep()
			return SessionAgentCall{}, fmt.Errorf(
				"RebuildSessionAgentCall: refusing durable row with an incompatible CallOptionsSpec: %w", err)
		}
		if rebuiltCallOptions == nil {
			rebuiltCallOptions = &CallOptions{}
		}
		// The rebuilt call is a bare internal call builder: it owns a fresh
		// build-resource scope (R12-H2 §1.2 — no process-lifetime fallback),
		// closed when RunSessionAgentCall's execution ends or fails.
		EnsureCallBuildScope(rebuiltCallOptions)
	}
	if data.FolderScopeSpec != nil {
		var compiledScope permission.FolderScope
		var canonDisk tools.DiskProvider
		if prep != nil {
			canonDisk = prep.provider
		}
		canonSpec, canonErr := tools.CanonicalizeFolderScopeSpec(
			ctx, canonDisk, *fromSessionFolderScopeSpec(data.FolderScopeSpec))
		if canonErr != nil {
			slog.Error("RebuildSessionAgentCall: the durable row's folder-scope spec failed to canonicalize; scoping the rebuilt turn to deny-everything",
				"session_id", data.SessionID, "err", canonErr)
		} else {
			var scopeErr error
			compiledScope, scopeErr = permission.BuildFolderScope(canonSpec)
			if scopeErr != nil {
				slog.Error("RebuildSessionAgentCall: the durable row's folder-scope spec failed to recompile; scoping the rebuilt turn to deny-everything",
					"session_id", data.SessionID, "err", scopeErr)
			}
		}
		rebuiltCallOptions.FolderScope = &compiledScope
	}
	if rebuiltCallOptions != nil && rebuiltCallOptions.FolderScope != nil {
		replayCtx := WithCallOptions(ctx, rebuiltCallOptions)
		if err := c.rejectScopedCallOnCLIProvider(replayCtx, "smart", smartProviderCfg); err != nil {
			releasePrep()
			return SessionAgentCall{}, err
		}
		if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok && workerModelCfg.Model != "" {
			if workerProviderCfg, ok := cfg.Providers.Get(workerModelCfg.Provider); ok {
				if err := c.rejectScopedCallOnCLIProvider(replayCtx, "worker", workerProviderCfg); err != nil {
					releasePrep()
					return SessionAgentCall{}, err
				}
			}
		}
	}

	// R12-H2b §2.2 fence transfer: every fallible rebuild step has passed.
	// Bind a fresh Call owner to the fence's live binding, register its
	// release on the rebuilt call's build scope (closed by
	// RunSessionAgentCall when the call's execution ends or fails), stamp
	// the requirement, and release both the prep token and the row-keyed
	// holder — the transfer is complete.
	if prep != nil {
		if rebuiltCallOptions == nil {
			rebuiltCallOptions = &CallOptions{}
		}
		EnsureCallBuildScope(rebuiltCallOptions)
		requirement := prep.requirement
		opts, callRelease, bindErr := bindExistingCallDiskBinding(c.diskCaps, requirement.BindingID, uuid.NewString(), rebuiltCallOptions)
		if bindErr != nil {
			releasePrep()
			return SessionAgentCall{}, &ErrCallAlreadyAttempted{
				Err: fmt.Errorf("%w: durable row's disk provider could not be re-bound (session=%s): %s", ErrDiskProviderUnavailable, data.SessionID, bindErr),
			}
		}
		opts.Requirement = &requirement
		opts.buildScope.register(callRelease)
		rebuiltCallOptions = opts
		prep.release()
		releaseDurableContinuationOwner(c.diskCaps, requirement.BindingID, rowID)
	}

	return SessionAgentCall{
		SessionID:        data.SessionID,
		LogicalCallID:    data.LogicalCallID, // P2-1 fix: restore stable ID
		Prompt:           data.Prompt,
		Attachments:      data.Attachments,
		ProviderOptions:  providerOptions,
		MaxOutputTokens:  data.MaxOutputTokens,
		Temperature:      temp,
		TopP:             topP,
		TopK:             topK,
		FrequencyPenalty: freqPenalty,
		PresencePenalty:  presPenalty,
		NonInteractive:   data.NonInteractive,
		// recompiled from the durable row's spec (R4-1).
		RunAllowlist: rebuiltRunAllowlist,
		// Keep the spec on the rebuilt call so a re-queued copy re-serializes it.
		RunAllowlistSpec: fromSessionRunAllowlistSpec(data.RunAllowlistSpec),
		FolderScopeSpec:  fromSessionFolderScopeSpec(data.FolderScopeSpec),
		// The recompiled scope rides in CallOptions (json:"-",
		// process-local); RunSessionAgentCall rebinds the toolset from it.
		CallOptions:          rebuiltCallOptions,
		SystemPromptOverride: data.SystemPromptOverride,
		MaxCost:              data.MaxCost,
		MaxTokens:            data.MaxTokens,
		ExistingMessageID:    data.ExistingMessageID,
		InjectID:             data.InjectID,
		SmartModel:           &smartModel,
		FastModel:            &fastModel,
		PersistSmartModel:    data.PersistSmartModel,
		PersistFastModel:     data.PersistFastModel,
		SystemPromptPrefix:   data.SystemPromptPrefix,
		SystemPrompt:         data.SystemPrompt,
		// R5-7 (P2 security review): restore the persisted entry-channel
		// origin. Both ToSessionAgentCallData/FromSessionAgentCallData
		// already round-trip Origin, but this rebuild path constructs its
		// own SessionAgentCall literal from `data` directly rather than
		// calling FromSessionAgentCallData, so it must copy the field
		// itself or a replayed call silently reverts to
		// message.OriginUnspecified despite the durable row carrying the
		// real value — disagreeing with the audit/transport metadata of
		// the request that actually entered the queue.
		Origin:              data.Origin,
		AutoResumed:         data.AutoResumed,
		BackgroundJobNotice: data.BackgroundJobNotice,
		// Mark as originating from the durable queue so mailbox.submit can
		// skip mb.submitted for this call (P0-1: avoid double-execution).
		// See agent.SessionAgentCall.FromDurableQueue documentation.
		FromDurableQueue: true,
	}, nil
}

// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
// call is already fully reconstructed with all necessary data.
func (c *coordinator) RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}
	// Durable calls bypass runInternal, so wire both result identities here.
	// The session callback is also required when a background pump execution is
	// later observed by DrainSessionNow.
	originalOnAssistantMessageCreated := call.OnAssistantMessageCreated
	if recorder := callResultRecorderFrom(ctx); recorder != nil {
		recorder.BeginCall()
		record := recorder.Capture()
		call.OnAssistantMessageCreated = func(id string) {
			record(id)
			session.RecordExecutionAssistantIdentity(ctx, id)
			if originalOnAssistantMessageCreated != nil {
				originalOnAssistantMessageCreated(id)
			}
		}
	} else {
		call.OnAssistantMessageCreated = func(id string) {
			session.RecordExecutionAssistantIdentity(ctx, id)
			if originalOnAssistantMessageCreated != nil {
				originalOnAssistantMessageCreated(id)
			}
		}
	}

	sessionID := call.SessionID

	// R4-3: after a REAL process restart the permission service's
	// autoApproveSessions map is empty (it is in-memory only, like
	// everything else the pump used to lose), so the first
	// permission-requiring tool call of a rebuilt non-interactive turn
	// would enter the interactive path and hang on a UI responder a
	// detached pump run does not have. A rebuilt call that carries a
	// policy spec is by construction an ExecuteRun-lineage call — only
	// ExecuteRun attaches WithRunAllowlistSpec, and it always
	// auto-approves its session — so its presence is the durable marker
	// that THIS session was being driven non-interactively. Re-arm it.
	// Idempotent for the in-process pump case (ExecuteRun already armed
	// it). Calls with no spec (web-origin durable calls, pre-migration
	// rows) are left exactly as before: interactive sessions must keep
	// reaching the UI, and a legacy row restarts with the pre-R4-3 status
	// quo. SessionAgentCall.NonInteractive would be the more direct
	// signal, but it is only set for sub-agent calls today and stamping it
	// on top-level runs would also change desktop-notify behavior
	// (agent_turn's `!call.NonInteractive` notification gate), which this
	// fix deliberately does not touch.
	if call.RunAllowlistSpec != nil && c.permissions != nil {
		c.permissions.AutoApproveSession(sessionID)
	}

	// T12: rebind the rebuilt call's scoped filesystem toolset. The turn
	// consumes call.Tools (runTurn), which is json:"-" and nil for every
	// pump-rebuilt call — and that nil falls back to the SHARED agent
	// toolset, which is unscoped. With the recompiled CallOptions from
	// RebuildSessionAgentCall, rebuild the per-call toolset exactly like
	// resolveSessionModels does for an in-process scoped call: attach the
	// options to THIS turn's context (buildTools and every sub-agent
	// build it spawns read CallOptions from ctx) and pin the freshly
	// built slice onto the call. A nil slice from pinCallTools must fail
	// the turn, NOT fall back to the shared toolset: for a call we KNOW
	// was scoped, the shared toolset IS the unrestricted restart T12
	// exists to prevent, so a build failure refuses the row (the pump
	// retries it) — the fail-closed direction.
	//
	// R5-3 (P0 security review): the trigger used to be FolderScope alone.
	// DisableSubAgents and ModelRole ALSO decide the pinned toolset
	// (applyCallDisableSubAgents, coordinator_tools.go reads both off the
	// ctx-carried CallOptions), so a rebuilt --agents single call with no
	// folder scope used to skip this block entirely, leave call.Tools nil,
	// and silently fall back to the shared toolset — regaining the
	// delegation tools (agent/agentic_fetch) it was declared not to have.
	// Widen the trigger to any of the three.
	//
	// R6-3 (P1 security review, round 6): the fail-closed refusal below
	// USED to be scoped to only scopedCallToolsRequired(ctx) — which, at
	// the time, only covered FolderScope/DiskProvider — so a
	// DisableSubAgents/ModelRole-only build failure fell all the way
	// through this if/else to the untouched, still-nil call.Tools, which
	// resolves to the shared unscoped toolset at turn start
	// (agent_turn.go). That is precisely the fail-open widening this
	// block exists to prevent, just for a different pair of fields than
	// the ones it originally guarded. scopedCallOptionsRequireDistinctTools
	// (coordinator_models.go) now agrees with this block's own trigger
	// condition — FolderScope, DiskProvider, DisableSubAgents, or a
	// non-empty ModelRole — so pinCallTools itself returns a non-nil error
	// in every case this outer `if` is entered and the build fails; there
	// is no longer a live "fall back to shared toolset" branch inside this
	// block at all.
	if scopedCallOptionsRequireDistinctTools(call.CallOptions) {
		ctx = WithCallOptions(ctx, call.CallOptions)
		cfg, _ := c.cfg.Snapshot()
		scopedTools, err := c.pinCallTools(ctx, cfg)
		if err != nil {
			// Assembly error: the build scope closes here, releasing this
			// call's construction owners (R12-H2 §1.2).
			call.CallOptions.buildScope.close()
			return nil, fmt.Errorf(
				"failed to build the rebuilt call's tool-shaping toolset; refusing to restart the turn on the shared unscoped toolset: %w", err)
		}
		call.Tools = scopedTools
	}

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `rush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. The defer ensures the ticker goroutine has joined
	// before RunSessionAgentCall returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done().
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	// queued-call-owned scopes are closed by the call resolution hook when
	// rejected, superseded, or completed. Durable rebuilt scopes remain owned
	// here and close on every return.
	if call.CallOptions != nil && call.onQueueResolved == nil {
		defer call.CallOptions.buildScope.close()
	}

	return c.currentAgent.Run(ctx, call)
}
