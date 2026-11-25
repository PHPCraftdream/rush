// Compaction and summarisation: the manual /compact path under mailbox
// ownership (Summarize/runSummarize), the inline body reused by auto-summarize
// (runSummarizeBody), and the summarize-queue accessors.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
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
