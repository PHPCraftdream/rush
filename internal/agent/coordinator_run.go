// Turn execution: SessionAgentCall assembly, the 401-retry run loop, and
// transient-failure retry classification. Extracted from coordinator.go —
// pure code move, bodies unchanged.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

const (
	// streamStallRetriesDefault is the default Options.StreamStallRetries
	// when the config key is absent or zero. We default to 2 (3 total
	// attempts per turn) rather than 0 because the user-visible failure
	// mode of a single transient provider error is a hard turn-error that
	// the orchestrator then has to handle — silently absorbing 1-2
	// provider hiccups is almost always what an operator wants. This bounds
	// retries for ALL transient turn failures (stream stall, empty stream,
	// overload, 5xx, network), not just stalls.
	streamStallRetriesDefault = 2
	// streamStallRetryBaseBackoff and streamStallRetryBackoffMultiplier
	// shape exponential backoff: 10s → 30s → 90s. Long enough to let a
	// rate-limit window roll over, short enough to keep one turn under
	// ~5 min including the prior watchdog timeout.
	streamStallRetryBaseBackoff       = 10 * time.Second
	streamStallRetryBackoffMultiplier = 3.0
	// streamStalledFinishTitle is the canonical Message field that
	// agent.Run writes on a watchdog stall. Match against this exact
	// string when deciding whether to retry.
	//
	// CROSS-LANGUAGE SYNC: a fifth copy of this literal lives in the
	// web UI, since TypeScript can't import this Go constant. See the
	// `part.Message === "Stream stalled"` check in
	// web/src/components/Message.tsx — it renders a stalled
	// turn-after-partial-work as a soft amber StreamPausedBlock
	// instead of a red failure block. If this literal ever changes,
	// update that TS check too. The two are intentionally not wired
	// through the WS/JSON protocol (LOW severity; a comment
	// cross-reference is the agreed sync mechanism).
	streamStalledFinishTitle = "Stream stalled"
)

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
	if err := checkPeakHours(providerCfg); err != nil {
		return SessionAgentCall{}, err
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
	sessionSystemPrompt := c.resolveSessionSystemPrompt(ctx, sessionID)

	pinnedSmart := model
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
		SmartModel:           &pinnedSmart,
		LogicalCallID:        uuid.New().String(), // P2-1: generate stable ID once
	}
	// Pinning matters more here than in runInternal: this call is QUEUED as a
	// replacement and may not start for a while, so the gap between "options
	// computed" and "turn starts" is unbounded rather than a few lines.
	pinned.pin(&call)
	return call, nil
}

// runInternal executes the agent, handling 401 retries.
//
// pinned carries what the caller's resolveSessionModels just resolved.
// pinned is ALWAYS non-nil: every Run path resolves from the session DB or
// config defaults before calling runInternal. Everything below —
// maxOutputTokens, providerCfg, the peak-hours check, mergeCallOptions —
// is derived from ONE model value that also travels with the call, so the
// turn cannot end up running a different model than the options were
// computed for (task #265).
func (c *coordinator) runInternal(ctx context.Context, sessionID string, prompt string, pinned *resolvedOverrides, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if pinned == nil {
		return nil, errors.New("runInternal: pinned is required; caller must resolve session models first")
	}

	model := pinned.smart
	slog.Debug("Coordinator: running with model", "sessionID", sessionID, "model", model.ModelCfg.Model)

	maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxOutputTokens = model.ModelCfg.MaxTokens
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
	// came from (task #341/P1-1) -- reuse it rather than a second, live
	// c.cfg.Config() read, which would reopen the exact torn-read gap the
	// 401-rebuild path below already fixed, on the far more common
	// (every-run, not just every-401) path.
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return nil, errModelProviderNotConfigured
	}
	// Fork patch (peak-hours bypass): consume the one-shot allow flag
	// armed by SetAllowPeakHours (`crush run --allow-peak-hours`). Reset
	// immediately so a subsequent Run on the same coordinator does not
	// inherit the bypass.
	c.runLimitsMu.Lock()
	allowPeak := c.allowPeakHours
	c.allowPeakHours = false
	c.runLimitsMu.Unlock()
	if !allowPeak {
		if err := checkPeakHours(providerCfg); err != nil {
			return nil, err
		}
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	}

	sessionSystemPrompt := c.resolveSessionSystemPrompt(ctx, sessionID)

	// Fork patch: batch 30 — per-run limits, pass through to the agent.
	c.runLimitsMu.Lock()
	maxCost := c.maxCost
	c.maxCost = 0
	maxTokensRunLimit := c.maxTokens
	c.maxTokens = 0
	c.runLimitsMu.Unlock()

	// Pin the model this call already resolved (task #265). Everything above
	// — maxOutputTokens, mergedOptions, temp/topP/topK/penalties — was
	// derived from THIS `model` value. Without pinning it, the agent
	// re-reads its shared largeModel when the turn actually starts, so a
	// concurrent session's applyModelOverrides landing in between makes the
	// turn run one model with another model's options. Passing it down keeps
	// the call internally consistent.
	pinnedSmart := model
	agentCall := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxOutputTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		MaxCost:              maxCost,
		MaxTokens:            maxTokensRunLimit,
		SmartModel:           &pinnedSmart,
		LogicalCallID:        uuid.New().String(), // P2-1: generate stable ID once
	}
	// Overrides pin fast model / prefix / base prompt too; pin() leaves
	// LargeModel as set above when pinned is nil, and rewrites it to the same
	// value when it isn't (model was taken FROM pinned.smart).
	pinned.pin(&agentCall)

	// trackCall is a pointer to the current call so we can rebuild it after
	// a 401 credential refresh (task #341, P1-2).
	trackCall := &agentCall
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, *trackCall)
	}

	// rebuildCall reconstructs the call after credential refresh, preserving
	// the logical request ID and other fields but using fresh models with new
	// credentials (task #341, P1-2).
	rebuildCall := func() error {
		// Resolve fresh models with updated credentials.
		pinned, err := c.resolveSessionModels(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("failed to resolve models after credential refresh: %w", err)
		}

		// Rebuild the call with the new models, preserving all logical fields.
		model := pinned.smart
		maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxOutputTokens = model.ModelCfg.MaxTokens
		}

		// Re-filter attachments for the new model's image support.
		if !model.CatwalkCfg.SupportsImages && attachments != nil {
			filteredAttachments := make([]message.Attachment, 0, len(attachments))
			for _, att := range attachments {
				if att.IsText() {
					filteredAttachments = append(filteredAttachments, att)
				}
			}
			attachments = filteredAttachments
		}

		// Use the provider config resolveSessionModels already resolved from
		// the SAME snapshot it built `model` from above (task #341, P1-1).
		// This used to take a SEPARATE, freshly-timed c.cfg.Snapshot() here
		// — despite the old comment claiming that was "consistent with
		// resolveSessionModels above", a reload landing in the gap between
		// resolveSessionModels returning and this second Snapshot() call
		// could hand back provider options/credentials from a DIFFERENT
		// generation than the model pinned.smart was built from, i.e.
		// exactly the torn-read this whole rebuildCall path exists to
		// avoid. pinned.providerCfg removes the second snapshot entirely.
		providerCfg := pinned.providerCfg
		if providerCfg.ID == "" {
			return errModelProviderNotConfigured
		}

		mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

		pinnedSmart := model
		newCall := SessionAgentCall{
			SessionID:            trackCall.SessionID,
			Prompt:               trackCall.Prompt,
			Attachments:          attachments,
			MaxOutputTokens:      maxOutputTokens,
			ProviderOptions:      mergedOptions,
			Temperature:          temp,
			TopP:                 topP,
			TopK:                 topK,
			FrequencyPenalty:     freqPenalty,
			PresencePenalty:      presPenalty,
			SystemPromptOverride: trackCall.SystemPromptOverride,
			MaxCost:              trackCall.MaxCost,
			MaxTokens:            trackCall.MaxTokens,
			SmartModel:           &pinnedSmart,
			LogicalCallID:        trackCall.LogicalCallID, // Preserve logical ID
			ExistingMessageID:    trackCall.ExistingMessageID,
			InjectID:             trackCall.InjectID,
			FromDurableQueue:     trackCall.FromDurableQueue,
		}
		pinned.pin(&newCall)
		*trackCall = newCall
		return nil
	}

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `crush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. Runs for BOTH the initial turn and every retry
	// re-run below (each run() sees a fresh ticker via this closure). The
	// defer ensures the ticker goroutine has joined before runInternal returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done(). Combine both into one
	// deferred func so the order is explicit and can't be silently
	// reordered by a future defer inserted between the two.
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	beforeLoaded := c.skillTracker.LoadedNames()
	var result *fantasy.AgentResult
	originalErr := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var err error
		result, err = run()
		return err
	}, rebuildCall)
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate.
	if originalErr != nil && c.isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	// Auto-retry on transient provider failures. The agent may have written
	// a FinishReasonError message (stream stall, empty stream) or returned a
	// transient error (429 overload, 5xx, GOAWAY/EOF, network drop); we
	// re-run the turn with the same prompt after exponential backoff as long
	// as it produced NO content (so a re-run cannot clobber a partial answer)
	// AND the failure is not operator-actionable (quota wall, auth, context
	// overflow, bad request, user cancel).
	// "Solve it ourselves before bothering the user" — provider hiccups
	// (rate limits, HTTP/2 stalls, brief capacity drops) usually clear
	// within tens of seconds, so 2 retries after 10s + 30s of backoff
	// absorb the common cases without the orchestrator having to know.
	// The retried turn appears in session history as a fresh user+
	// assistant pair, which the model sees alongside the previous
	// failed attempt — slightly noisy but functionally correct.
	maxRetries := streamStallRetriesDefault
	if opts := c.cfg.Config().Options; opts != nil && opts.StreamStallRetries != nil {
		// Explicit override (including explicit 0 to disable entirely).
		maxRetries = *opts.StreamStallRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if !c.shouldRetryTurn(ctx, sessionID, originalErr) {
			break
		}
		backoff := streamStallRetryBaseBackoff
		for i := 1; i < attempt; i++ {
			backoff = time.Duration(float64(backoff) * streamStallRetryBackoffMultiplier)
		}
		slog.Warn(
			"coordinator: retrying transient turn failure",
			"session_id", sessionID,
			"attempt", attempt+1,
			"max_attempts", maxRetries+1,
			"backoff", backoff.String(),
		)
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(backoff):
		}
		result, originalErr = run()
	}

	return result, originalErr
}

// retryClass partitions a turn-terminating failure into "surface it" vs
// "transparently re-run it". See shouldRetryTurn for the policy.
type retryClass int

const (
	// classTerminal is an operator-actionable failure (quota wall, auth,
	// context overflow, bad request, user cancel) that must surface.
	classTerminal retryClass = iota
	// classTransient is a provider/network hiccup worth a re-run.
	classTransient
)

// classifyProviderError classifies a NON-NIL turn-terminating error.
// context cancellation is terminal here — watchdog stalls are matched
// separately by their persisted finish title in shouldRetryTurn, because
// a stall surfaces only as context.Canceled.
func classifyProviderError(err error) retryClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classTerminal
	}
	// Peak-hours refusal is operator policy, not a transient hiccup: the
	// condition only clears when the wall clock leaves the window, so a
	// backoff retry would just burn the backoff and fail identically.
	if errors.Is(err, errProviderPeakHours) {
		return classTerminal
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.IsContextTooLarge() {
			return classTerminal // the auto-summarize path owns this
		}
		switch providerErr.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired:
			return classTerminal
		case http.StatusForbidden:
			// 403 is ambiguous: it can be a real auth/geo wall (retry pointless)
			// or a CDN/anti-abuse banner from a fronting balancer that clears in
			// tens of seconds (z.ai "Forbidden ZS", Cloudflare-fronted providers).
			// Treat as transient: the worst case is ~40s of bounded backoff on a
			// truly bad key vs. losing a long agent run on a momentary block.
			return classTransient
		case http.StatusTooManyRequests:
			if isQuotaLimit(providerErr) {
				return classTerminal // multi-hour usage wall — operator accepts a fast fail
			}
			return classTransient // momentary overload
		case http.StatusRequestTimeout, http.StatusConflict:
			return classTransient
		}
		if providerErr.StatusCode >= 500 {
			return classTransient
		}
		if providerErr.StatusCode >= 400 {
			return classTerminal // genuine client error (400, 404, ...)
		}
		// No HTTP status (status 0): EOF / network wrapped as ProviderError.
		if providerErr.IsRetryable() {
			return classTransient
		}
		return classTerminal
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classTransient
	}
	return classTerminal
}

// isQuotaLimit reports whether a 429 is a hard usage/quota wall (resets on
// the order of hours) rather than a momentary overload. The two share
// status 429, so we discriminate on the provider's message text.
func isQuotaLimit(providerErr *fantasy.ProviderError) bool {
	msg := strings.ToLower(providerErr.Title + " " + providerErr.Message)
	return strings.Contains(msg, "usage limit") ||
		strings.Contains(msg, "limit will reset") ||
		strings.Contains(msg, "reset at") ||
		strings.Contains(msg, "quota")
}

// turnMadeProgress reports whether the assistant message carries any real
// output — text, reasoning, or a tool call (even a partial one). A turn
// that made progress must never be re-run: it would duplicate work the
// user already has.
func turnMadeProgress(msg message.Message) bool {
	return strings.TrimSpace(msg.FullText()) != "" ||
		strings.TrimSpace(msg.ReasoningContent().Thinking) != "" ||
		len(msg.ToolCalls()) > 0
}

// shouldRetryStalledMessage decides whether a watchdog-stalled assistant
// message warrants re-running the turn. The retry exists to recover from
// turns where the provider never delivered anything; ANY content reaching
// the assistant — text, reasoning, even a half-emitted tool call — proves
// the server received and processed the prompt, and re-running would just
// duplicate the user message in the DB and burn tokens redoing work the
// user already (partially) has.
//
// Returns false for any non-stalled finish reason (including nil), so the
// caller can pass the last assistant message unconditionally.
func shouldRetryStalledMessage(msg message.Message) bool {
	fp := msg.FinishPart()
	if fp == nil {
		return false
	}
	if fp.Reason != message.FinishReasonError || fp.Message != streamStalledFinishTitle {
		return false
	}
	return !turnMadeProgress(msg)
}

// lastAssistantMessage returns the most recent assistant message in the
// session. ok is false on a DB error or when there is no assistant message
// yet — callers treat that as "nothing to retry".
func (c *coordinator) lastAssistantMessage(ctx context.Context, sessionID string) (message.Message, bool) {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return message.Message{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			return msgs[i], true
		}
	}
	return message.Message{}, false
}

// shouldRetryTurn decides whether a finished turn should be transparently
// re-run. A turn qualifies ONLY if it ended in error WITHOUT producing any
// content (so a re-run cannot clobber a partial answer) AND the failure is
// a transient provider/network hiccup rather than an operator-actionable
// condition. This generalizes the original stall-only retry: a watchdog
// stall is one transient class among several.
//
// Decision order:
//   - no assistant message / clean finish / user-cancel finish → don't retry
//   - turn produced any content                                 → don't retry
//   - persisted "Stream stalled" title (err is context.Canceled) → retry
//   - turn returned no error (empty-stream close)                → retry
//   - otherwise classify the returned error                      → transient?
func (c *coordinator) shouldRetryTurn(ctx context.Context, sessionID string, err error) bool {
	msg, ok := c.lastAssistantMessage(ctx, sessionID)
	if !ok {
		return false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return false
	}
	if turnMadeProgress(msg) {
		return false
	}
	if fp.Message == streamStalledFinishTitle {
		return true
	}
	if err == nil {
		return true
	}
	return classifyProviderError(err) == classTransient
}

// RunWithOverrides implements Coordinator. It is like Run but uses the given
// smart/fast model overrides instead of the global config defaults.
func (c *coordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// Carry session-level reasoning effort into the overrides so that
	// applyModelOverrides restores it after resetting the model config.
	if sess, err := c.sessions.Get(ctx, sessionID); err == nil {
		if smart != nil && smart.ReasoningEffort == "" && sess.SmartModelReasoningEffort != "" {
			smart.ReasoningEffort = sess.SmartModelReasoningEffort
		}
		if fast != nil && fast.ReasoningEffort == "" && sess.FastModelReasoningEffort != "" {
			fast.ReasoningEffort = sess.FastModelReasoningEffort
		}
	}

	pinned, err := c.applyModelOverrides(ctx, smart, fast)
	if err != nil {
		return nil, err
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}
