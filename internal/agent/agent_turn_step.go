package agent

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// prepareStep, onStepFinish and stopConditions are runTurn's remaining
// step-lifecycle callbacks, moved onto turnStream mechanically (task
// #933/#937): identifiers renamed to ts.* fields, no behavioral change.
// onStepFinish is still the single ~230-line body it always was — splitting
// it into named phases is task #940's job, kept separate so this move's
// oracle stays a plain "identical modulo rename" comparison.
func (ts *turnStream) prepareStep(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
	// PrepareStep runs before the first token of the step and can
	// take non-trivial time (sliding-window trim, background
	// summarise kickoff, cache-control wiring). Bump first so a
	// slow prepare doesn't trip the watchdog before the stream
	// even starts.
	ts.bumpActivity()
	prepared.Messages = options.Messages
	for i := range prepared.Messages {
		prepared.Messages[i].ProviderOptions = nil
	}

	// R3-1: the call's PINNED tool slice when it carries one —
	// identical at every step, immune to a concurrent call's
	// SetTools landing between steps. Only legacy calls (nothing
	// pinned) re-read the shared slice (updated by SetTools when
	// MCP tools change). The cache-control marker is re-applied per
	// step through the same per-step wrapper the turn's initial
	// tool list uses; withProviderOptionsOnLast clones before
	// writing, so the pinned slice itself is never mutated.
	pinnedStepTools := ts.call.Tools
	if pinnedStepTools == nil {
		pinnedStepTools = ts.a.tools.Copy()
	}
	prepared.Tools = withProviderOptionsOnLast(pinnedStepTools, ts.a.getCacheControlOptions())

	for _, inj := range ts.a.drainDueInjects(ts.call.SessionID, ts.genID, ts.historyIDs) {
		prepared.Messages = append(prepared.Messages, inj.ToAIMessage()...)
	}

	// Cross-process inject drain: rows written by another process
	// (`rush sessions inject`) into pending_injects. The message
	// row already exists in the DB (the CLI created it at inject
	// time for immediate web-UI visibility), so we only load it by
	// message_id and splice it in — no second Create, no dup row.
	// DrainPendingInjects deletes the consumed non-interrupt rows in
	// the same transaction (delete-after-read).
	pending, hasInterrupt, drainErr := ts.a.sessions.DrainPendingInjects(callContext, ts.call.SessionID)
	if drainErr != nil {
		return callContext, prepared, drainErr
	}
	if hasInterrupt {
		// Defensive: interrupt rows are meant to be consumed by the
		// interrupt ticker before PrepareStep runs. If one is still
		// here it is a race, not a normal path.
		slog.Warn("pending interrupt inject present during non-interrupt PrepareStep drain",
			"session_id", ts.call.SessionID)
	}
	for _, inj := range pending {
		injMsg, getErr := ts.a.messages.Get(callContext, inj.MessageID)
		if getErr != nil {
			// The referenced message vanished (e.g. cascade delete):
			// skip it rather than aborting the whole step.
			slog.Warn("pending inject references missing message, skipping",
				"session_id", ts.call.SessionID, "message_id", inj.MessageID, "error", getErr)
			continue
		}
		prepared.Messages = append(prepared.Messages, injMsg.ToAIMessage()...)
		// The row was written by a foreign process (`rush sessions
		// inject`), so its Create() never published through THIS
		// process's message broker. If a web UI happens to be
		// attached to this process for the session, Notify pushes
		// the already-persisted message so it renders live instead
		// of waiting for a page reload.
		ts.a.messages.Notify(injMsg)
	}

	// Sliding-window context management: when the context is nearly
	// full, trim old messages so the agent can keep running without
	// blocking on a synchronous summarisation call.
	if !ts.a.disableAutoSummarize {
		cw := int64(ts.smartModel.CatwalkCfg.ContextWindow)
		if cw > 0 {
			usedTokens := ts.currentSession.CompletionTokens + ts.currentSession.PromptTokens
			remaining := cw - usedTokens
			var slideThreshold int64
			if cw > largeContextWindowThreshold {
				slideThreshold = largeContextWindowBuffer
			} else {
				slideThreshold = int64(float64(cw) * smallContextWindowRatio)
			}
			if remaining <= slideThreshold {
				targetTokens := int64(float64(cw) * contextSlideRatio)
				prepared.Messages = trimMessagesToWindow(prepared.Messages, targetTokens)

				// Record that a silent compact is needed — it runs
				// synchronously AFTER the turn completes (under the
				// turn's mailbox ownership), not as a concurrent
				// goroutine. A goroutine deleting messages while the
				// turn is still streaming was the P0-4 data corruption
				// bug (#268).
				ts.silentCompactNeeded = true
			}
		}
	}

	prepared.Messages = ts.a.workaroundProviderMediaLimitations(prepared.Messages, ts.smartModel)

	lastSystemRoleInx := 0
	systemMessageUpdated := false
	for i, msg := range prepared.Messages {
		// Only add cache control to the last message.
		if msg.Role == fantasy.MessageRoleSystem {
			lastSystemRoleInx = i
		} else if !systemMessageUpdated {
			prepared.Messages[lastSystemRoleInx].ProviderOptions = ts.a.getCacheControlOptions()
			systemMessageUpdated = true
		}
		// Than add cache control to the last 2 messages.
		if i > len(prepared.Messages)-3 {
			prepared.Messages[i].ProviderOptions = ts.a.getCacheControlOptions()
		}
	}

	if ts.promptPrefix != "" {
		prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(ts.promptPrefix)}, prepared.Messages...)
	}

	ts.mu.Lock()
	ts.stepMessages = cloneFantasyMessages(prepared.Messages)
	ts.stepTools = append([]fantasy.AgentTool(nil), prepared.Tools...)
	ts.mu.Unlock()

	var assistantMsg message.Message
	// Provenance is recorded from the model that ACTUALLY produced
	// the message, not from the configuration that selected it.
	//
	// Both values feed `GROUP BY model, provider` in the usage and
	// cost reports (internal/db/stats.sql.go, messages.sql.go), and
	// the two summarize paths in agent_compaction.go have always
	// recorded Model.Model()/Provider(). While this line recorded
	// ModelCfg instead, a provider whose canonical id differs from
	// the configured one would split a single session into two
	// groups in `rush sessions cost` — with neither number looking
	// wrong enough to notice.
	assistantMsg, err = ts.a.messages.Create(callContext, ts.call.SessionID, message.CreateMessageParams{
		Role:            message.Assistant,
		Parts:           []message.ContentPart{},
		Model:           ts.smartModel.Model.Model(),
		Provider:        ts.smartModel.Model.Provider(),
		ReasoningEffort: ts.currentSession.SmartModelReasoningEffort,
	})
	if err != nil {
		return callContext, prepared, err
	}
	callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
	callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, ts.smartModel.CatwalkCfg.SupportsImages)
	callContext = context.WithValue(callContext, tools.ModelNameContextKey, ts.smartModel.CatwalkCfg.Name)
	ts.mu.Lock()
	ts.currentAssistant = &assistantMsg
	ts.mu.Unlock()
	return callContext, prepared, err
}

// onStepFinish is fantasy's per-step callback, split into 8 named phases
// (task #940) after the mechanical move in task #939 kept it as one
// ~230-line body. Each phase is verified by a targeted revert-check (break
// the invariant, confirm the specific existing test fails with the specific
// expected symptom, restore) rather than a diff — a behavioral split has no
// diff oracle. The phase order below IS the invariant in three places:
// stopStepTicker must run before recordStepFinish touches currentAssistant
// (else the checkpoint ticker races the final write); recordStepHistory
// must run before recordStepFinish reads loopDetected (fantasy calls
// OnStepFinish before StopWhen for the same step, so a stale flag here
// never gets fixed by a later step); and enforceRunawayCaps/recheckPeakHours
// must call activeRequests' cancelFn(), not just return an error, because
// returning an error from OnStepFinish alone does not break fantasy's loop
// (BUG-4, pinned by TestActiveRequests_HoldsLiveCancelDuringTurn).
func (ts *turnStream) onStepFinish(stepResult fantasy.StepResult) error {
	ts.bumpActivity()
	ts.recordStepHistory(stepResult)
	// Surface provider CallWarnings (malformed tool-call sanitization,
	// unsupported settings, etc.) that fantasy otherwise discards
	// silently. Visible in logs only — does not interrupt the turn.
	logProviderWarnings(stepResult.Warnings)
	ts.stopStepTicker()

	finishReason := classifyStepFinishReason(stepResult)
	ts.recordStepFinish(finishReason)

	updatedSession, err := ts.applyStepUsage(stepResult)
	if err != nil {
		return err
	}
	if err := ts.enforceRunawayCaps(updatedSession); err != nil {
		return err
	}
	if err := ts.recheckPeakHours(); err != nil {
		return err
	}
	return ts.persistStepFinish()
}

// recordStepHistory accumulates this step and recomputes loop detection
// NOW, in this callback invocation, so recordStepFinish below can use the
// result for THIS step. Fantasy calls OnStepFinish BEFORE StopWhen for the
// same step, so relying on the StopWhen closure to set loopDetected would
// read a stale (still-false) flag for the very step that trips the
// detector — the loop would break with empty finish text and no later
// OnStepFinish to fix it. See turnStream's doc for the ordering rationale.
func (ts *turnStream) recordStepHistory(stepResult fantasy.StepResult) {
	ts.stepHistory = append(ts.stepHistory, stepResult)
	ts.loopDetected, ts.loopDetail = hasRepeatedToolCalls(ts.stepHistory, loopDetectionWindowSize, loopDetectionMaxRepeats)
}

// stopStepTicker stops the checkpoint ticker BEFORE the final write below so
// it doesn't race with OnStepFinish (Fork patch: batch 8), and resets the
// tool-boundary phase tracker for the next step.
func (ts *turnStream) stopStepTicker() {
	ts.stopCheckpoint()
	ts.phase = phaseToolBoundary
}

// classifyStepFinishReason maps fantasy's step finish reason to this
// package's, then upgrades FinishReasonToolUse to FinishReasonEndTurn when a
// tool result halted the turn (e.g. a hook halt or a permission denial): the
// step ends on FinishReasonToolCalls but the model will not be called
// again, so the UI must still render the assistant footer.
func classifyStepFinishReason(stepResult fantasy.StepResult) message.FinishReason {
	finishReason := message.FinishReasonUnknown
	switch stepResult.FinishReason {
	case fantasy.FinishReasonLength:
		finishReason = message.FinishReasonMaxTokens
	case fantasy.FinishReasonStop:
		finishReason = message.FinishReasonEndTurn
	case fantasy.FinishReasonToolCalls:
		finishReason = message.FinishReasonToolUse
	}
	if finishReason == message.FinishReasonToolUse {
		for _, tr := range stepResult.Content.ToolResults() {
			if tr.StopTurn {
				finishReason = message.FinishReasonEndTurn
				break
			}
		}
	}
	return finishReason
}

// recordStepFinish writes this step's Finish part onto currentAssistant.
// Fork patch: surface empty-stream as a visible error. Some providers (e.g.
// z.ai) sometimes close the stream without sending any content (no text, no
// tool_call, no reasoning) and without an explicit finish reason. The
// upstream code records this as FinishReasonUnknown with empty parts, which
// the WUI renders as a blank assistant block — looking like a session
// lockup. Convert this case to an error so both the WUI fallback and the
// user see an actionable message. See CHANGELOG.fork.md section 4.D.
//
// currentAssistant reads/mutations below are under mu: OnStepFinish never
// runs concurrently with the other streaming callbacks (fantasy invokes
// them sequentially from one loop), but it DOES run concurrently with the
// checkpoint ticker and the peak-hours watcher goroutines, which also touch
// currentAssistant.
func (ts *turnStream) recordStepFinish(finishReason message.FinishReason) {
	ts.mu.Lock()
	if finishReason == message.FinishReasonUnknown &&
		ts.currentAssistant.FullText() == "" &&
		ts.currentAssistant.ReasoningContent().Thinking == "" &&
		len(ts.currentAssistant.ToolCalls()) == 0 {
		slog.Warn(
			"agent: empty stream from provider — recording as error",
			"sessionID", ts.call.SessionID,
			"provider", ts.smartModel.ModelCfg.Provider,
			"model", ts.smartModel.ModelCfg.Model,
		)
		ts.currentAssistant.AddFinish(
			message.FinishReasonError,
			"Empty response",
			fmt.Sprintf(
				"Provider %q closed the stream for model %q without returning any content. This is usually a transient provider/network issue — please retry.",
				ts.smartModel.ModelCfg.Provider, ts.smartModel.ModelCfg.Model,
			),
		)
	} else if ts.loopDetected {
		// Loop detection force-stopped the turn. The reason stays
		// FinishReasonEndTurn (NOT a new distinct enum value) so
		// reclassifyCrashedAsDone / sessions-why keep treating this as
		// "done" — but the message/details are non-empty so an operator
		// or orchestrator can distinguish "model finished voluntarily"
		// from "we truncated a likely loop (possibly a legitimate poll)".
		loopMsg, loopDetails := loopDetectedFinishText(ts.loopDetail)
		ts.currentAssistant.AddFinish(finishReason, loopMsg, loopDetails)
	} else {
		ts.currentAssistant.AddFinish(finishReason, "", "")
	}
	ts.mu.Unlock()
	// Drain any pending UI snapshot so the ticker goroutine does not
	// publish a stale state after messages.Update writes the final one.
	ts.drainPendingUI()
}

// applyStepUsage fetches the session row this step just updated, folds in
// this step's usage/cost, and records both the session- and message-level
// breakdown. Any error here short-circuits OnStepFinish exactly as it did
// before this split — cap enforcement and the peak-hours recheck never run
// against a session snapshot that failed to load or persist.
func (ts *turnStream) applyStepUsage(stepResult fantasy.StepResult) (session.Session, error) {
	updatedSession, getSessionErr := ts.a.sessions.Get(ts.ctx, ts.call.SessionID)
	if getSessionErr != nil {
		return session.Session{}, getSessionErr
	}
	// Fork merge note (origin/main 6ed8852b "fix(agent): estimate
	// missing streamed usage"): if the provider omits the final
	// usage chunk, use upstream's token estimator so our sliding
	// context window stays accurate. We drop the "estimated" flag
	// (TUI marker — see CHANGELOG.fork.md Section 2).
	usage, estimated := fallbackStepUsage(ts.stepMessages, stepResult)
	// Normalize once, upstream of both updateSessionUsage and
	// recordMessageUsage, so InputTokens is exclusive-of-cache for
	// every provider before either consumer sees it.
	usage = normalizeProviderUsage(ts.smartModel.Model.Provider(), usage)
	costDelta := ts.a.updateSessionUsage(ts.smartModel, &updatedSession, usage, ts.a.openrouterCost(stepResult.ProviderMetadata))
	if costDelta != 0 {
		if _, costErr := ts.a.sessions.IncrementCost(ts.ctx, updatedSession.ID, costDelta); costErr != nil {
			return session.Session{}, costErr
		}
	}
	if sessionErr := ts.a.sessions.SetUsage(ts.ctx, updatedSession.ID, updatedSession.PromptTokens, updatedSession.CompletionTokens); sessionErr != nil {
		return session.Session{}, sessionErr
	}
	// Per-message breakdown (task #469). The session-level figures
	// above are a last-snapshot overwrite plus a running cost, which
	// cannot answer "how well is the cache working" for a message, a
	// model, or a day. currentAssistant.ID is read under mu
	// because the checkpoint ticker and peak-hours watcher also touch
	// currentAssistant (same reason recordStepFinish locks).
	ts.mu.Lock()
	assistantID := ts.currentAssistant.ID
	ts.mu.Unlock()
	ts.a.recordMessageUsage(ts.ctx, assistantID, ts.smartModel, usage, costDelta, estimated)
	if usage.CacheCreationTokens > 0 {
		ts.a.scheduleCacheKeepAlive(ts.call.SessionID, ts.smartModel, ts.stepMessages, ts.stepTools, ts.call.ProviderOptions, ts.call.MaxCost)
	}
	ts.currentSession = updatedSession
	return updatedSession, nil
}

// enforceRunawayCaps implements Fork patch: batch 30 — cancel + runaway
// protection: the DB cancel flag (cross-process signal) and the cost/token
// caps. BUG-4 (full-project reviewer audit, 2026-08-11): these abort paths
// (max-cost, max-tokens; peak-hours is recheckPeakHours below) stop the
// turn ONLY via the cancelFunc looked up from activeRequests — returning an
// error from OnStepFinish alone does NOT break fantasy's loop. This is safe
// today ONLY because runTurn stores the turn's genCtx cancel via
// activeRequests.Set before the agent.Stream call whose OnStepFinish looks
// it up, and nothing ever calls activeRequests.Del for this key (entries
// live forever — see IsBusy's doc). Any future change that reclaims an
// activeRequests entry before the turn ends silently turns these aborts
// into no-ops: the error is returned but the turn keeps running. Pinned by
// TestActiveRequests_HoldsLiveCancelDuringTurn.
func (ts *turnStream) enforceRunawayCaps(updatedSession session.Session) error {
	canc, cancErr := ts.a.sessions.IsCancelRequested(ts.ctx, ts.call.SessionID)
	if cancErr != nil {
		// A failed read is NOT treated as a cancellation: a transient
		// DB error is no evidence the operator asked for one, and
		// aborting on it would turn every hiccup into what looks like
		// a user abort. But it must not be silent either — if the
		// operator did request a cancel and this is the read that
		// failed, the turn runs on with nothing in the log to say why
		// the request appeared to be ignored.
		slog.Warn("could not read the cancel-requested flag; continuing the turn",
			"session_id", ts.call.SessionID, "err", cancErr)
	}
	if cancErr == nil && canc {
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
		return fmt.Errorf("session %s cancelled by user", ts.call.SessionID)
	}
	if ts.call.MaxCost > 0 && updatedSession.Cost > ts.call.MaxCost {
		slog.Warn(
			"agent: aborting — max-cost exceeded",
			"session_id", ts.call.SessionID,
			"cost", updatedSession.Cost,
			"max", ts.call.MaxCost,
		)
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
		return fmt.Errorf("session %s aborted: cost $%.4f exceeds max $%.4f",
			ts.call.SessionID, updatedSession.Cost, ts.call.MaxCost)
	}
	totalTokens := updatedSession.PromptTokens + updatedSession.CompletionTokens
	if ts.call.MaxTokens > 0 && totalTokens > ts.call.MaxTokens {
		slog.Warn(
			"agent: aborting — max-tokens exceeded",
			"session_id", ts.call.SessionID,
			"tokens", totalTokens,
			"max", ts.call.MaxTokens,
		)
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
		return fmt.Errorf("session %s aborted: %d tokens exceeds max %d",
			ts.call.SessionID, totalTokens, ts.call.MaxTokens)
	}
	return nil
}

// recheckPeakHours re-checks the provider's peak-hours window once per step
// (Fork patch). Peak-hours is normally only checked once, at the START of a
// turn (coordinator.buildCall/runInternal) — an already-in-flight turn was
// never re-checked, so a long turn that started before the window opened
// ran straight through it.
func (ts *turnStream) recheckPeakHours() error {
	if ts.a.peakHoursCheck == nil {
		return nil
	}
	pErr := ts.a.peakHoursCheck()
	if pErr == nil {
		return nil
	}
	if ts.setPeakHoursAbortErr(pErr) {
		slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
			"session_id", ts.call.SessionID, "error", pErr)
		peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
		ts.mu.Lock()
		ts.currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
		snap := ts.currentAssistant.Clone()
		ts.mu.Unlock()
		// Use the parent ctx (not genCtx) for the DB write —
		// genCtx dies as soon as we cancel below.
		if uErr := ts.a.messages.Update(ts.ctx, snap); uErr != nil {
			slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
		}
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
	}
	// Stash the specific error so Run() can return it AFTER fantasy's
	// agent.Stream exits. We must call cancelFn() to break fantasy's loop
	// (returning an error from OnStepFinish alone doesn't stop it), but
	// cancel() makes fantasy return context.Canceled — swallowing our
	// pErr. The stash lets Run() replace that generic error with the real
	// one.
	return pErr
}

// persistStepFinish is OnStepFinish's normal-path final write.
func (ts *turnStream) persistStepFinish() error {
	ts.mu.Lock()
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) stopConditions() []fantasy.StopCondition {
	return []fantasy.StopCondition{
		func(_ []fantasy.StepResult) bool {
			cw := int64(ts.smartModel.CatwalkCfg.ContextWindow)
			// If context window is unknown (0), skip auto-summarize
			// to avoid immediately truncating custom/local models.
			if cw == 0 {
				return false
			}
			tokens := ts.currentSession.CompletionTokens + ts.currentSession.PromptTokens
			remaining := cw - tokens
			var threshold int64
			if cw > largeContextWindowThreshold {
				threshold = largeContextWindowBuffer
			} else {
				threshold = int64(float64(cw) * smallContextWindowRatio)
			}
			if (remaining <= threshold) && !ts.a.disableAutoSummarize {
				ts.shouldSummarize = true
				return true
			}
			return false
		},
		func(steps []fantasy.StepResult) bool {
			// StopWhen runs AFTER OnStepFinish for the same step, so by the
			// time this executes, OnStepFinish has already appended to
			// stepHistory and recomputed loopDetected/loopDetail. We only
			// need to return the boolean here to tell fantasy to break the
			// loop — do NOT mutate loopDetected/loopDetail here, OnStepFinish
			// owns them (mutating here would race for the last step's
			// finish text and re-introduce the stale-flag bug).
			detected, _ := hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
			return detected
		},
	}
}
