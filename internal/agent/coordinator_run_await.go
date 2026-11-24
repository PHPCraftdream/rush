// runAwaitingAdmission: a blocking helper for callers that need the REAL
// outcome of a Run() call even when the mailbox was busy and this specific
// invocation only queued it (#1036).
package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

// runAwaitingAdmission runs call on agent and, if THIS invocation queues the
// call instead of executing it (mailbox busy, non-fail-fast), blocks
// (bounded by ctx) until the queued call's own eventual turn -- run by
// whichever goroutine drains the mailbox -- resolves, and returns THAT
// turn's outcome instead of (nil, nil). Blocking (not a returned future) is
// deliberate: every existing caller that needs this (runSubAgent) already
// calls Run synchronously and immediately consumes *fantasy.AgentResult, so
// a blocking helper needs no restructuring of that code, and per the design
// doc's own structured-concurrency principle, waiting on live work is
// correct behavior here, not a hang -- the queued turn WILL end.
//
// queued reports whether this call's own turn was admitted immediately
// (false) or had to wait behind another owner (true) -- callers that only
// need OBSERVABILITY (wakeSession's failure-visibility bookkeeping) read
// this without blocking by NOT calling this helper at all: they call
// agent.Run directly with a turnAdmission armed on ctx and check
// wasQueued() themselves -- queued is an EXPECTED, non-failure outcome for a
// wake (the notice is already durable), so there is nothing to await there.
func (c *coordinator) runAwaitingAdmission(ctx context.Context, agent SessionAgent, call SessionAgentCall) (result *fantasy.AgentResult, err error, queued bool) {
	type outcome struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan outcome, 1)
	// resolveOnce guards the hook itself, independent of the channel's own
	// buffering: this call's outcome must be delivered exactly once even if
	// something upstream (a future call site, not any today) ever invokes
	// OnQueueResolved more than once for the same call.
	var resolveOnce sync.Once
	call.OnQueueResolved = func(r *fantasy.AgentResult, e error) {
		resolveOnce.Do(func() {
			select {
			case done <- outcome{r, e}:
			default:
			}
		})
	}
	admission := agentcall.NewTurnAdmission()
	result, err = agent.Run(agentcall.WithTurnAdmission(ctx, admission), call)
	if !(result == nil && err == nil && admission.WasQueued()) {
		return result, err, false
	}
	select {
	case o := <-done:
		return o.result, o.err, true
	case <-ctx.Done():
		return nil, ctx.Err(), true
	}
}

// buildCall assembles the SessionAgentCall for the current agent + model
// state. Extracted so InterruptAndSend can queue a call shaped exactly like
// runInternal would.
//
// pinned is ALWAYS non-nil: every caller resolves from the session DB or
// config defaults before calling buildCall. This ensures session-scoped model
// overrides are respected even on cross-process paths (e.g., interrupt requeue).
func (c *coordinator) buildCall(ctx context.Context, sessionID, prompt string, pinned *resolvedOverrides, attachments []message.Attachment) (SessionAgentCall, error) {
	if pinned == nil {
		return SessionAgentCall{}, errors.New("buildCall: pinned is required; caller must resolve session models first")
	}

	model := pinned.smart

	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// pinned.providerCfg was resolved from the SAME snapshot pinned.smart
	// came from (task #341/P1-1) -- a live c.cfg.Config() read here would
	// reintroduce the torn-read this whole `pinned` threading exists to
	// close, since buildCall may run well after resolveSessionModels did
	// (e.g. for a queued replacement call, per the comment below).
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return SessionAgentCall{}, errModelProviderNotConfigured
	}
	// A per-call --allow-peak-hours waives the window here too.
	if opts := callOptionsFrom(ctx); opts == nil || !opts.AllowPeakHours {
		if err := checkPeakHours(providerCfg); err != nil {
			return SessionAgentCall{}, err
		}
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(sessionID, model, providerCfg)
	sessionSystemPrompt := c.sessionPromptForCall(ctx, sessionID, pinned)

	pinnedSmart := model
	// R1-1: carry whatever per-call options the arming context holds so a
	// queued replacement (InterruptAndSend) keeps its own policy when it
	// eventually starts, however long the queue delay is.
	callOpts := callOptionsFrom(ctx)
	if callOpts != nil {
		EnsureCallBuildScope(callOpts)
	}
	autoResumed, backgroundJobNotice := noticeFlagsFrom(ctx)

	call := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		// Preserve the accepted call's policy when this run is queued as a
		// mailbox follow-up and later executed under the owner's context.
		CallOptions:  callOpts,
		RoleContract: modelkit.CloneRoleContractFromCallOptions(callOpts),
		// R3-4: this call's restricted-run policy, armed at turn start; the
		// uncompiled spec travels too so the durable queue can persist it.
		RunAllowlist:        runAllowlistFrom(ctx),
		RunAllowlistSpec:    runAllowlistSpecFrom(ctx),
		FolderScopeSpec:     folderScopeSpecFrom(ctx),
		SmartModel:          &pinnedSmart,
		LogicalCallID:       uuid.New().String(), // P2-1: generate stable ID once
		AutoResumed:         autoResumed,
		BackgroundJobNotice: backgroundJobNotice,
		IsDrain:             isDrainCallFrom(ctx),
	}
	// Stamp the entry-channel origin at BUILD time, not message-creation
	// time: the call may be queued as an InterruptAndSend replacement and
	// drained much later under a DIFFERENT turn's context, so the sender's
	// origin must travel on the call struct itself.
	call.Origin = CallOriginFrom(ctx)
	// R12-H2b §2.2: a run armed with an execution requirement envelope
	// stamps it onto the built call so durable producers can persist and
	// carry it (diskRequirementForCall reads this field first).
	if callOpts != nil && callOpts.Requirement != nil {
		call.DiskRequirement = callOpts.Requirement
	}
	// Pinning matters more here than in runInternal: this call is QUEUED as a
	// replacement and may not start for a while, so the gap between "options
	// computed" and "turn starts" is unbounded rather than a few lines.
	pinned.pin(&call)
	// A batch delegation child's freshly built call (interrupt replacement,
	// interrupt tick, InterruptAndSend) must carry the delegation's frozen
	// question capability and its shaped child toolset — buildCall resolves
	// from the caller's context and cannot know them.
	if err := c.inheritBatchQuestionState(ctx, sessionID, &call); err != nil {
		return SessionAgentCall{}, err
	}
	return call, nil
}
