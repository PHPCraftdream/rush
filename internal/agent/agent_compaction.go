// Compaction and summarisation: the manual /compact path under mailbox
// ownership (Summarize/runSummarize), the inline body reused by auto-summarize
// (runSummarizeBody), and the summarize-queue accessors.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// ErrSummarizeQueued is returned by Summarize when the session is busy and
// the request has been queued for execution after the current task finishes.
var ErrSummarizeQueued = errors.New("summarize queued")

type contextBudgetHost interface {
	compactionHost
	tryAdmitRun() (done func(), ok bool)
	getMailbox(sessionID string) *mailboxpkg.Mailbox
	Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	abandonOwnershipWithHandoff(sessionID string, epoch uint64)
	effectiveStreamWatchdogTick() time.Duration
	preparePrompt(msgs []message.Message, todos []coresession.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart)
	getSessionMessages(ctx context.Context, session coresession.Session) ([]message.Message, error)
	openrouterCost(metadata fantasy.ProviderMetadata) *float64
	updateSessionUsage(model Model, sess *coresession.Session, usage fantasy.Usage, overrideCost *float64) float64
	recordMessageUsage(ctx context.Context, messageID string, model Model, usage fantasy.Usage, costDelta float64, estimated bool)
	streamIdleTimeoutCfg() time.Duration
	timeoutHardCapCfg() time.Duration
	timeoutExtendsOnProgressCfg() bool
	dataDirCfg() string
	lockOptionsCfg() []sessionlock.LockOption
}

var _ contextBudgetHost = (*sessionAgent)(nil)

type turnBudget struct {
	// summarizeQueue holds a pending manual-summarise snapshot per session,
	// queued while the session was busy. The snapshot contains the model,
	// provider options, and prompt prefix resolved from the target session,
	// ensuring the entire summarize operation uses consistent configuration
	// regardless of concurrent SetModels calls (task #341, P1-1).
	summarizeQueue *csync.Map[string, *SummarizeSnapshot]
}

// tryAdmitRun atomically admits one unit of work: it returns the matching
// runWg.Done release as a method value alongside the ok flag so the caller
// can `defer done()` at its former defer site without touching runWg
// directly (EX-0H; also the reuse seam for keepalive). ok is false when
// shutdown has already begun and nothing was registered.
func (a *sessionAgent) tryAdmitRun() (done func(), ok bool) {
	if !a.tryAdmitRunWg() {
		return nil, false
	}
	return a.runWg.Done, true
}

func (a *sessionAgent) TryAdmitRun() (func(), bool) { return a.tryAdmitRun() }
func (a *sessionAgent) openrouterCost(m fantasy.ProviderMetadata) *float64 {
	return usagepolicy.OpenrouterCost(m)
}

func (a *sessionAgent) updateSessionUsage(model Model, sess *coresession.Session, u fantasy.Usage, overrideCost *float64) float64 {
	return usagepolicy.UpdateSessionUsage(model, sess, u, overrideCost)
}

func (a *sessionAgent) OpenrouterCost(m fantasy.ProviderMetadata) *float64 {
	return usagepolicy.OpenrouterCost(m)
}

func (a *sessionAgent) recordMessageUsage(ctx context.Context, messageID string, model Model, usageValue fantasy.Usage, costDelta float64, estimated bool) {
	usagepolicy.NewMessageUsageComponent(a).RecordMessageUsage(ctx, messageID, model, usageValue, costDelta, estimated)
}

func (a *sessionAgent) RecordMessageUsageStore() usagepolicy.MessageUsageStore { return a.messages }

func (a *sessionAgent) CacheKeepAliveSessions() coresession.Service { return a.sessions }
func (a *sessionAgent) CacheKeepAlivePeakHours() callbeat.CacheKeepAlivePeakHoursChecker {
	if a.coord == nil {
		return nil
	}
	return a.coord
}

func (a *sessionAgent) streamIdleTimeoutCfg() time.Duration      { return a.streamIdleTimeout }
func (a *sessionAgent) timeoutHardCapCfg() time.Duration         { return a.timeoutHardCap }
func (a *sessionAgent) timeoutExtendsOnProgressCfg() bool        { return a.timeoutExtendsOnProgress }
func (a *sessionAgent) dataDirCfg() string                       { return a.dataDir }
func (a *sessionAgent) lockOptionsCfg() []sessionlock.LockOption { return a.lockOptions }

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	return a.summarizeRequested(a, ctx, sessionID, snapshot)
}

func (a *sessionAgent) SummarizeNow(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	return a.summarizeNow(a, ctx, sessionID, snapshot)
}

func (a *sessionAgent) summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot, queueWhenBusy bool) error {
	return a.turnBudget.summarize(a, ctx, sessionID, snapshot, queueWhenBusy)
}

func (t *turnBudget) summarizeRequested(h contextBudgetHost, ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	return t.summarize(h, ctx, sessionID, snapshot, true)
}

func (t *turnBudget) summarizeNow(h contextBudgetHost, ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("standalone summary snapshot is required")
	}
	return t.summarize(h, ctx, sessionID, snapshot, false)
}

func (t *turnBudget) summarize(h contextBudgetHost, ctx context.Context, sessionID string, snapshot *SummarizeSnapshot, queueWhenBusy bool) error {
	var mb *mailboxpkg.Mailbox
	return turnstep.SummarizeAdmit(turnstep.SummarizeAdmission{
		Queue:       t.summarizeQueue,
		TryAdmitRun: h.tryAdmitRun,
		ShutdownErr: func() error { return ErrAgentShuttingDown },
		QueuedErr:   func() error { return ErrSummarizeQueued },
		BusyErr:     func() error { return ErrSessionBusy },
		BeginCompact: func(sessionID string, cancel context.CancelFunc) (uint64, bool) {
			mb = h.getMailbox(sessionID)
			return mb.BeginCompact(cancel)
		},
		RunSummarize: func(ctx, genCtx context.Context, sessionID string, snapshot *SummarizeSnapshot, epoch uint64, cancel context.CancelFunc, queueOnLockFailure bool) error {
			return t.runSummarize(h, ctx, genCtx, sessionID, snapshot, mb, epoch, cancel, queueOnLockFailure)
		},
	}, ctx, sessionID, snapshot, queueWhenBusy)
}

// runSummarize runs a manual /compact under mailbox ownership acquired by
// Summarize's beginCompact call. After the compaction body completes, it
// releases ownership via abandonOwnership (handing back any Run that queued
// behind it via mailbox.Submit during the stream) and starts a fresh Run for
// the first queued call — the same drain shape the old runSummarizeCore tail
// had, now operating on mailbox ownership instead of a synthetic
// activeRequests key.
//
// #312: Cross-process compaction serialization
//
// Manual compaction (this path) acquires the OS session lock before calling
// runSummarizeBody and releases it after. This prevents another process from
// starting a Run() for the same session while the compaction's commit phase
// (set SummaryMessageID + delete messages) is in flight.
//
// Inline compaction paths (runTurn's shouldSummarize and silentCompactNeeded)
// do NOT go through this function — they call runSummarizeBody or
// runSummarizeSilent directly while already holding the OS lock via the parent
// Run() call. No deadlock is possible because those paths never acquire a
// second lock.
//
// OS lock acquisition happens AFTER beginCompact succeeds and mb.Mu is
// released, avoiding #296's I/O-under-mu anti-pattern. The lock is held for
// the entire runSummarizeBody call to protect both the LLM stream and the
// commit phase, matching Run()'s lock-hold semantics.
//
// The snapshot parameter contains the model, provider options, and prompt
// prefix resolved from the target session (or shared state for sessions without
// overrides), ensuring the entire summarize operation uses consistent
// configuration regardless of concurrent SetModels calls (task #341, P1-1).
func (a *sessionAgent) runSummarize(ctx context.Context, genCtx context.Context, sessionID string, snapshot *SummarizeSnapshot, mb *mailboxpkg.Mailbox, epoch uint64, cancel context.CancelFunc, queueOnLockFailure bool) error {
	return a.turnBudget.runSummarize(a, ctx, genCtx, sessionID, snapshot, mb, epoch, cancel, queueOnLockFailure)
}

func (t *turnBudget) runSummarize(h contextBudgetHost, ctx context.Context, genCtx context.Context, sessionID string, snapshot *SummarizeSnapshot, mb *mailboxpkg.Mailbox, epoch uint64, cancel context.CancelFunc, queueOnLockFailure bool) error {
	// Resolve the idle timeout for the summarization stream watchdog (config
	// read, once, at the original point); the body itself lives in the
	// turnstep leaf as turnstep.RunSummarize — this wrapper is pure
	// adaptation: it resolves the leaf's deps (including narrow closures
	// over the package-private *mailboxpkg.Mailbox, whose test seams are read lazily
	// inside the closures) and forwards.
	idleTimeout := streamIdleTimeoutDefault
	if h.streamIdleTimeoutCfg() > 0 {
		idleTimeout = h.streamIdleTimeoutCfg()
	}
	// Original order preserved: stallSetPhase runs here, BEFORE the leaf reads
	// effectiveStreamWatchdogTick / starts the watchdog.
	stallSetPhase(sessionID, "internal", "compaction")
	return turnstep.RunSummarize(turnstep.RunSummarizeDeps{
		Queue:     t.summarizeQueue,
		Requeue:   func() { t.requeueSummarizeSnapshot(sessionID, snapshot) },
		TakeQueue: func() bool { _, queued := t.takeSummarizeQueue(sessionID); return queued },

		IdleTimeout:        idleTimeout,
		WatchdogTick:       h.effectiveStreamWatchdogTick(),
		ExtendsOnProgress:  h.timeoutExtendsOnProgressCfg,
		HardCap:            h.timeoutHardCapCfg,
		NotifyActivity:     notifyActivity,
		WithWatchdogBump:   withWatchdogBump,
		WithActivityNotify: withActivityNotify,

		DataDir:     h.dataDirCfg,
		LockOptions: h.lockOptionsCfg,
		QueuedErr:   func() error { return ErrSummarizeQueued },
		BusyErr:     func() error { return ErrSessionBusy },

		RunBody: func(bodyCtx context.Context) error {
			return t.runSummarizeBody(h, bodyCtx, sessionID, snapshot.ProviderOptions, snapshot.Model, snapshot.PromptPrefix)
		},

		PreSnapshotSeam: func() {
			if mb.TestPreSnapshotConsumeSeam != nil {
				mb.TestPreSnapshotConsumeSeam()
			}
		},
		PreAbandonSeam: func() {
			if mb.TestPreAbandonSeam != nil {
				mb.TestPreAbandonSeam()
			}
		},
		BeginRelease:   mb.BeginRelease,
		FinishRelease:  func(epoch uint64) { mb.FinishRelease(epoch) },
		AbandonHandoff: func() { h.abandonOwnershipWithHandoff(sessionID, epoch) },
		AbandonPopFirst: func() (SessionAgentCall, bool) {
			return mb.AbandonOwnershipAndPopFirstSubmitted(epoch)
		},

		RunQueued: func(runCtx context.Context, call SessionAgentCall) error {
			_, runErr := h.Run(runCtx, call)
			return runErr
		},
	}, ctx, genCtx, sessionID, snapshot, epoch, cancel, queueOnLockFailure)
}

// requeueSummarizeSnapshot puts a compaction back in summarizeQueue after a
// lock-acquisition failure so the session's next owner drains and runs it.
// GetOrSet (not Set) never clobbers a newer queued request, and no compaction
// is spawned here, so a long-held lock cannot cause a busy retry loop.
func (t *turnBudget) requeueSummarizeSnapshot(sessionID string, snapshot *SummarizeSnapshot) {
	turnstep.RequeueSummarizeSnapshot(t.summarizeQueue, sessionID, snapshot)
}

func (a *sessionAgent) requeueSummarizeSnapshot(sessionID string, snapshot *SummarizeSnapshot) {
	a.turnBudget.requeueSummarizeSnapshot(sessionID, snapshot)
}

func (a *sessionAgent) SummarizeQueued(sessionID string) bool {
	return a.summarizeQueued(sessionID)
}

func (t *turnBudget) summarizeQueued(sessionID string) bool {
	return turnstep.SummarizeQueued(t.summarizeQueue, sessionID)
}

// testBuildSummarizeSnapshot is a test helper that creates a SummarizeSnapshot
// from the agent's current shared state. Used by tests that call Summarize directly.
func (a *sessionAgent) testBuildSummarizeSnapshot() *SummarizeSnapshot {
	model := a.smartModel.Get()
	prefix := a.systemPromptPrefix.Get()

	// Build minimal provider options (same as getProviderOptions but for tests).
	opts := fantasy.ProviderOptions{}

	return &SummarizeSnapshot{
		Model:           model,
		ProviderOptions: opts,
		PromptPrefix:    prefix,
	}
}

func (a *sessionAgent) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return a.takeSummarizeQueue(sessionID)
}

func (t *turnBudget) takeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return turnstep.TakeSummarizeQueue(t.summarizeQueue, sessionID)
}

func (a *sessionAgent) CancelQueuedSummarize(sessionID string) {
	a.cancelQueuedSummarize(sessionID)
}

func (t *turnBudget) cancelQueuedSummarize(sessionID string) {
	turnstep.CancelQueuedSummarize(t.summarizeQueue, sessionID)
}

func (a *sessionAgent) runSummarizeBody(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	return a.turnBudget.runSummarizeBody(a, ctx, sessionID, opts, smartModel, systemPromptPrefix)
}

// runSummarizeBody performs the actual summarisation: load messages, snapshot
// non-pinned IDs for deletion, create the summary message, stream the
// summary, persist it, wire up SummaryMessageID, and delete the old messages.
// It performs NO busy-check and NO ownership management — the caller must
// already hold sessionID's mailbox ownership (via beginCompact for
// standalone /compact, or via the turn loop's submit for inline
// auto-summarize). The caller's cancel is already registered in the mailbox
// (current.cancel), so Cancel(sessionID) naturally targets this compaction
// — no synthetic "sessionID-summarize" key.
//
// genCtx is the caller's already-cancel-scoped context (beginCompact's
// cancel for manual /compact, or the turn's genCtx for inline
// auto-summarize).
func (t *turnBudget) runSummarizeBody(h contextBudgetHost, ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	return turnstep.RunSummarizeBody(turnstep.SummarizeBodyDeps{
		Sessions:           h.compactionSessions,
		Messages:           h.compactionMessages,
		NotifyActivity:     notifyActivity,
		NotifyWatchdog:     notifyWatchdog,
		PreparePrompt:      h.preparePrompt,
		GetSessionMessages: h.getSessionMessages,
		SummaryPrompt:      provauth.SummaryPromptText,
		CommitMaxDuration:  provauth.SummaryCommitMaxDuration,
		BuildSummaryPrompt: provauth.BuildSummaryPrompt,
		CompletionTokens:   provauth.SummaryCompletionTokens,
		SessionHeaders:     sessionHeaders,
		NormalizeUsage:     provauth.NormalizeProviderUsage,
		OpenrouterCost:     h.openrouterCost,
		UpdateSessionUsage: h.updateSessionUsage,
		RecordMessageUsage: h.recordMessageUsage,
	}, ctx, sessionID, opts, smartModel, systemPromptPrefix)
}

type compactionHost interface {
	compactionSessions() session.Service
	compactionMessages() message.Service
}

func (a *sessionAgent) compactionSessions() session.Service { return a.sessions }

func (a *sessionAgent) compactionMessages() message.Service { return a.messages }

type recoveryHost interface {
	compactionHost
	preparePrompt(msgs []message.Message, todos []session.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart)
	getSessionMessages(ctx context.Context, sess session.Session) ([]message.Message, error)
	accountRecoverySummary(ctx context.Context, sessionID, messageID string, model Model, usage fantasy.Usage, cost float64, snapshot session.Session) error
	checkRecoverySummaryInput(ctx context.Context, sessionID string, summary message.Message, tail, pinned []message.Message, todos []session.Todo, state *codexRecoveryState) error
	checkRecoverySummaryCost(ctx context.Context, sessionID string, maxCost, delta float64) error
	openrouterCost(metadata fantasy.ProviderMetadata) *float64
	updateSessionUsage(model Model, sess *session.Session, usage fantasy.Usage, overrideCost *float64) float64
	recordMessageUsage(ctx context.Context, messageID string, model Model, usage fantasy.Usage, costDelta float64, estimated bool)
}

type recoveryComponent struct {
	host recoveryHost
}

func newRecoveryComponent(a *sessionAgent) *recoveryComponent {
	return &recoveryComponent{host: a}
}

// runSummarizeSilent compacts the older portion of the session history
// synchronously under the caller's mailbox ownership without any visible
// change in the UI. It retains the recent tail verbatim, summarizes eligible
// older messages, creates a hidden summary, and deletes only the messages it
// summarized. Pinned messages are never deleted.
//
// DESIGN (intentional, not a bug): The sliding window preserves recent
// messages verbatim for continuity while replacing older context with a
// summary. Summarizing everything defeats that purpose. A prior review
// mistook the summary sorting after the retained tail (which hid it from the
// reader) for a design flaw; the summary belongs before the verbatim tail.
func (a *sessionAgent) runSummarizeSilent(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	return newRecoveryComponent(a).runSummarizeSilent(ctx, sessionID, opts, smartModel, systemPromptPrefix)
}

func (a *sessionAgent) runSummarizeSilentForRecovery(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix, retainUserID string, maxOutputTokens int64, state *codexRecoveryState) error {
	return newRecoveryComponent(a).runSummarizeSilentForRecovery(ctx, sessionID, opts, smartModel, systemPromptPrefix, retainUserID, maxOutputTokens, state)
}

func (c *recoveryComponent) runSummarizeSilent(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	return c.runSummarizeSilentForRecovery(ctx, sessionID, opts, smartModel, systemPromptPrefix, "", 0, nil)
}

func (c *recoveryComponent) runSummarizeSilentForRecovery(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix, retainUserID string, maxOutputTokens int64, state *codexRecoveryState) error {
	// Keep the ordinary silent-compaction path intact; recovery adds checks around
	// the same stream and persistence operations only when retainUserID is set.
	recovery := retainUserID != ""
	if recovery {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state == nil {
			return errors.New("automatic context recovery state is unavailable")
		}
	}
	// Heartbeat attribution: same summary purpose, silent path (design pt. 1).
	role, source := callbeat.HbInherited(ctx)
	ctx = callbeat.WithHeartbeat(ctx, sessionID, heartbeat.PurposeSummary, role, source)
	currentSession, err := c.host.compactionSessions().Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := c.host.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) < 4 && retainUserID == "" {
		// Existing silent compaction keeps its historical minimum.
		return nil
	}

	// sliding window: keep the recent tail verbatim. Start at the first
	// eligible non-tool message in the latter half so a tool exchange is not
	// split; if none exists, summarize the entire history.
	boundary := len(msgs)
	for i := len(msgs) / 2; i < len(msgs); i++ {
		if msgs[i].Role != message.Tool {
			boundary = i
			break
		}
	}
	if retainUserID != "" {
		retainedBoundary, found := retainHistoryBoundary(msgs, retainUserID)
		if !found {
			return errors.New("automatic context recovery cannot locate the original user message")
		}
		boundary = min(boundary, retainedBoundary)
		if boundary <= 0 {
			return errors.New("automatic context recovery has no eligible older history to summarize")
		}
	}
	var toSummarise []message.Message
	for _, m := range msgs[:boundary] {
		if !m.Pinned {
			toSummarise = append(toSummarise, m)
		}
	}
	if len(toSummarise) == 0 {
		if retainUserID != "" {
			return errors.New("automatic context recovery has no eligible older history to summarize")
		}
		return nil
	}
	aiMsgs, _ := c.host.preparePrompt(toSummarise, nil, smartModel.Model.Provider())
	if recovery {
		if maxOutputTokens <= 0 {
			return errors.New("automatic context recovery requires known worker output limits")
		}
		preparedSummary := provauth.RecoverySummarySystemMessages(aiMsgs, provauth.BuildSummaryPrompt(currentSession.Todos))
		if err := provauth.RecoveryInputCapacity(smartModel, maxOutputTokens, systemPromptPrefix, preparedSummary, nil, nil); err != nil {
			return err
		}
	}

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(provauth.SummaryPromptText()),
	)
	// Create the summary message as hidden so it is invisible in the UI.
	summaryMessage, err := c.host.compactionMessages().Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            smartModel.Model.Model(),
		Provider:         smartModel.Model.Provider(),
		ReasoningEffort:  provauth.TurnSmartReasoningEffort(smartModel),
		IsSummaryMessage: true,
		Hidden:           true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := provauth.BuildSummaryPrompt(currentSession.Todos)
	var discardRetryContent bool
	truncateAbandonedContent := func() error {
		if !discardRetryContent {
			return nil
		}
		discardRetryContent = false
		if !summaryMessage.TruncateStreamedContent() {
			return nil
		}
		return c.host.compactionMessages().Update(ctx, summaryMessage.Clone())
	}

	var pinnedOutside []message.Message
	if recovery {
		for _, m := range msgs[:boundary] {
			if m.Pinned {
				pinnedOutside = append(pinnedOutside, m)
			}
		}
	}
	var outputLimit *int64
	if recovery {
		outputLimit = &maxOutputTokens
	}
	if recovery {
		if err := ctx.Err(); err != nil {
			return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, err)
		}
	}
	var retryCount *int
	if recovery {
		zero := 0
		retryCount = &zero
	} else {
		retryCount = &agentcall.ProviderMaxRetries
	}
	streamCtx := ctx
	if recovery {
		streamCtx = context.WithValue(streamCtx, cliprovider.SessionIDContextKey, sessionID)
		if smartModel.Model.Provider() == codexprovider.Name {
			streamCtx = context.WithValue(streamCtx, cliprovider.ReasoningEffortContextKey, provauth.TurnSmartReasoningEffort(smartModel))
			streamCtx = context.WithValue(streamCtx, cliprovider.ServiceTierContextKey, turnstep.TurnSmartServiceTier(smartModel))
		} else {
			streamCtx = context.WithValue(streamCtx, cliprovider.ReasoningEffortContextKey, "")
		}
	}
	resp, err := agent.Stream(streamCtx, fantasy.AgentStreamCall{
		MaxRetries:      retryCount,
		MaxOutputTokens: outputLimit,
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnRetry: func(_ *fantasy.ProviderError, _ time.Duration) {
			discardRetryContent = true
		},
		OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.StartReasoningContent(reasoning.Text)
			return c.host.compactionMessages().Update(ctx, summaryMessage)
		},
		OnReasoningDelta: func(id string, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.AppendReasoningContent(text)
			return c.host.compactionMessages().Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return c.host.compactionMessages().Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.AppendContent(text)
			return c.host.compactionMessages().Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if retainUserID != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), provauth.SummaryCommitMaxDuration)
			deleteErr := c.host.compactionMessages().Delete(cleanupCtx, summaryMessage.ID)
			cleanupCancel()
			return errors.Join(fmt.Errorf("automatic context recovery summary stream: %w", err), deleteErr)
		}
		if errors.Is(err, context.Canceled) {
			// User cancelled summarize; remove the summary message.
			// Use a bounded cancel-immune context for cleanup (P1-4): the
			// stream's context is already canceled, so Delete would silently
			// fail and leave an orphaned summary message in the DB.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), provauth.SummaryCommitMaxDuration)
			defer cleanupCancel()
			deleteErr := c.host.compactionMessages().Delete(cleanupCtx, summaryMessage.ID)
			if deleteErr != nil {
				slog.Warn("Silent summarize: failed to delete orphaned summary message after cancel", "session_id", sessionID, "err", deleteErr)
			}
		}
		return err
	}

	var recoveryUsage fantasy.Usage
	var recoverySpent int64
	var recoveryCostDelta float64
	var recoverySession session.Session
	var recoveryAccounted bool
	var recoverySpentAdded bool
	accountRecovery := func() error {
		if !recovery || recoveryAccounted {
			return nil
		}
		recoveryAccounted = true
		if !recoverySpentAdded {
			state.Spent.Add(recoverySpent)
			recoverySpentAdded = true
		}
		accountCtx, accountCancel := context.WithTimeout(context.WithoutCancel(ctx), provauth.SummaryCommitMaxDuration)
		defer accountCancel()
		return c.host.accountRecoverySummary(accountCtx, sessionID, summaryMessage.ID, smartModel, recoveryUsage, recoveryCostDelta, recoverySession)
	}
	if recovery {
		recoveryUsage = provauth.NormalizeProviderUsage(smartModel.Model.Provider(), resp.TotalUsage)
		recoverySpent = provauth.AddStepUsageTokens(0, recoveryUsage)
		var providerCost *float64
		for _, step := range resp.Steps {
			if stepCost := c.host.openrouterCost(step.ProviderMetadata); stepCost != nil {
				if providerCost == nil {
					providerCost = new(float64)
				}
				*providerCost += *stepCost
			}
		}
		var usageBase session.Session
		recoveryCostDelta = c.host.updateSessionUsage(smartModel, &usageBase, recoveryUsage, providerCost)
		var usageErr error
		recoverySession, usageErr = c.host.compactionSessions().Get(ctx, sessionID)
		if usageErr != nil {
			recoverySession = usageBase
			return errors.Join(errors.New("automatic context recovery cannot account summary usage"), accountRecovery())
		}
		provauth.UpdateSessionTokenCounters(&recoverySession, recoveryUsage)
		capacityErr := c.host.checkRecoverySummaryInput(ctx, sessionID, summaryMessage, msgs[boundary:], pinnedOutside, currentSession.Todos, state)
		if strings.TrimSpace(summaryMessage.Content().Text) == "" {
			capacityErr = errors.Join(capacityErr, errors.New("automatic context recovery summary was empty"))
		}
		var tokenErr error
		if state.MaxTokens > 0 && state.Spent.Load()+recoverySpent > state.MaxTokens {
			tokenErr = errors.New("automatic context recovery would exceed the call token cap")
		}
		costErr := c.host.checkRecoverySummaryCost(ctx, sessionID, state.MaxCost, recoveryCostDelta)
		if costErr != nil {
			accountErr := accountRecovery()
			return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, errors.Join(costErr, accountErr))
		}
		if err := ctx.Err(); err != nil {
			capacityErr = errors.Join(capacityErr, err)
		}
		if capacityErr != nil || tokenErr != nil || costErr != nil {
			accountErr := accountRecovery()
			return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, errors.Join(capacityErr, tokenErr, costErr, accountErr))
		}
		state.Spent.Add(recoverySpent)
		recoverySpentAdded = true
	}
	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), provauth.SummaryCommitMaxDuration)
	defer cleanupCancel()
	if err = c.host.compactionMessages().Update(cleanupCtx, summaryMessage); err != nil {
		if recovery {
			return errors.Join(err, accountRecovery())
		}
		return err
	}

	// COMMIT PHASE — the order and the context were both wrong here, and
	// P0-4's own change is what made that reachable.
	//
	// Order: wire up SummaryMessageID BEFORE deleting the messages it
	// replaces, exactly as runSummarizeBody does (see its comment at the
	// equivalent point). This function did the opposite. Interrupted between
	// the two, that leaves the old half deleted with no summary pointer
	// standing in for it — getSessionMessages then finds neither, and the
	// history is unrecoverably holed.
	//
	// Context: the commit must not be tearable by cancellation. While the
	// silent compaction was a detached goroutine on context.WithoutCancel,
	// this window was unreachable. Making it synchronous (P0-4) put it on the
	// turn's cancellable genCtx and turned a long-standing latent ordering
	// bug into a live one, reachable via Stop in the web UI, CancelAll on
	// shutdown, --timeout, or the turn's own stream watchdog. Bounded rather
	// than unbounded: the provider stream is already finished, so what
	// remains is local DB work.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), provauth.SummaryCommitMaxDuration)
	defer commitCancel()

	// Point SummaryMessageID at the new hidden summary and reset token
	// counters so the next call gets an accurate remaining-context estimate.
	freshSession, err := c.host.compactionSessions().Get(commitCtx, sessionID)
	if err != nil {
		if recovery {
			return errors.Join(fmt.Errorf("silent summarise: failed to re-fetch session: %w", err), accountRecovery())
		}
		return fmt.Errorf("silent summarise: failed to re-fetch session: %w", err)
	}
	var openrouterCost *float64
	for _, step := range resp.Steps {
		if stepCost := c.host.openrouterCost(step.ProviderMetadata); stepCost != nil {
			if openrouterCost == nil {
				openrouterCost = new(float64)
			}
			*openrouterCost += *stepCost
		}
	}
	// Normalize once, upstream of both sinks (see agent_turn.go's OnStepFinish).
	totalUsage := provauth.NormalizeProviderUsage(smartModel.Model.Provider(), resp.TotalUsage)
	costDelta := c.host.updateSessionUsage(smartModel, &freshSession, totalUsage, openrouterCost)
	if recovery {
		freshSession.PromptTokens = recoverySession.PromptTokens
		freshSession.CompletionTokens = recoverySession.CompletionTokens
		freshSession.OwnCost = recoverySession.OwnCost
		costDelta = recoveryCostDelta
		totalUsage = recoveryUsage
	}
	if recovery {
		if ctx.Err() != nil {
			return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, ctx.Err())
		}
		if err := c.host.compactionSessions().SetUsage(commitCtx, freshSession.ID, recoverySession.PromptTokens, recoverySession.CompletionTokens); err != nil {
			_ = accountRecovery()
			return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, err)
		}
		if recoveryCostDelta != 0 {
			if _, err := c.host.compactionSessions().IncrementCost(commitCtx, freshSession.ID, recoveryCostDelta); err != nil {
				_ = accountRecovery()
				return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, err)
			}
		}
		callbeat.AddHeartbeatUsage(ctx, smartModel.Model.Provider(), smartModel.Model.Model(), recoveryUsage, recoveryCostDelta)
		c.host.recordMessageUsage(commitCtx, summaryMessage.ID, smartModel, recoveryUsage, recoveryCostDelta, false)
		recoveryAccounted = true
	} else {
		callbeat.AddHeartbeatUsage(ctx, smartModel.Model.Provider(), smartModel.Model.Model(), totalUsage, costDelta)
		if costDelta != 0 {
			if _, err := c.host.compactionSessions().IncrementCost(commitCtx, freshSession.ID, costDelta); err != nil {
				return err
			}
		}
		c.host.recordMessageUsage(commitCtx, summaryMessage.ID, smartModel, totalUsage, costDelta, false)
	}

	if boundary < len(msgs) {
		// Place summary immediately before the first kept message before
		// updating the pointer, or timestamp-based reads may omit it.
		// A pinned older row at keptAt sorts after the summary in the same second,
		// so it stays in the tail in chronological order; manual compaction differs.
		keptAt := msgs[boundary].CreatedAt
		timestampService, ok := c.host.compactionMessages().(interface {
			SetSummaryCreatedAt(context.Context, string, string, int64) (bool, error)
		})
		if !ok {
			cleanupErr := c.host.compactionMessages().Delete(commitCtx, summaryMessage.ID)
			stampErr := errors.Join(errors.New("message service does not support summary timestamps"), cleanupErr)
			if recovery {
				return errors.Join(stampErr, accountRecovery())
			}
			return stampErr
		}
		if applied, stampErr := timestampService.SetSummaryCreatedAt(commitCtx, sessionID, summaryMessage.ID, keptAt-1); stampErr != nil || !applied {
			cleanupErr := c.host.compactionMessages().Delete(commitCtx, summaryMessage.ID)
			if recovery {
				if stampErr == nil && !applied {
					stampErr = errors.New("set sliding summary timestamp: no matching summary row updated")
				}
				return errors.Join(stampErr, cleanupErr, accountRecovery())
			}
			if stampErr != nil {
				failure := fmt.Errorf("set sliding summary timestamp: %w", stampErr)
				if recovery {
					return errors.Join(failure, cleanupErr, accountRecovery())
				}
				if cleanupErr != nil {
					return errors.Join(failure, fmt.Errorf("delete orphaned summary: %w", cleanupErr))
				}
				return failure
			}
			if cleanupErr != nil {
				cause := errors.New("set sliding summary timestamp: no matching summary row updated")
				if recovery {
					return errors.Join(cause, cleanupErr, accountRecovery())
				}
				return errors.Join(cause, fmt.Errorf("delete orphaned summary: %w", cleanupErr))
			}
			cause := errors.New("set sliding summary timestamp: no matching summary row updated")
			if recovery {
				return errors.Join(cause, accountRecovery())
			}
			return cause
		}
	}
	if recovery && ctx.Err() != nil {
		return provauth.CleanupUncommittedSummary(c.host.compactionMessages(), ctx, summaryMessage.ID, ctx.Err())
	}
	if err := c.host.compactionSessions().SetSummaryAndUsage(commitCtx, freshSession.ID, summaryMessage.ID, 0, provauth.SummaryCompletionTokens(resp.Response.Usage, summaryMessage)); err != nil {
		if recovery {
			return errors.Join(err, accountRecovery())
		}
		// Nothing has been deleted yet, so the session is still whole: the
		// compaction simply did not happen, and the next turn retries it.
		return err
	}

	// Only now, with the summary persisted AND pointed at, drop what it
	// replaces. A failure here is survivable — the summary already stands in
	// for these rows, so the worst case is redundant history, not a hole.
	for _, m := range toSummarise {
		if delErr := c.host.compactionMessages().Delete(commitCtx, m.ID); delErr != nil {
			slog.Warn("silent summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}
	return nil
}
