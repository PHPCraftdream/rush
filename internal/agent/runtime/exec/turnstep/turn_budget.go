// Package turnstep leaf helpers for per-turn step logic. turn_budget.go
// holds the stream-watchdog onFire handler moved verbatim from the agent
// package's turnBudget component (EX-0H): a pure helper with no package
// state — every value it touches arrives as a parameter, including the
// hard-cap configuration, which the agent-side root passes as the
// h.timeoutHardCapCfg method value so it is only ever evaluated in the
// causeHardCap branch, at the original point.
package turnstep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// SummarizeAdmission bundles the host seams SummarizeAdmit needs: the
// summarize queue, the runWg admission pair, the sentinel error providers,
// and the mailbox-ownership and runSummarize callbacks.
type SummarizeAdmission struct {
	Queue *csync.Map[string, *agentcall.SummarizeSnapshot]
	// TryAdmitRun is the host's tryAdmitRun method value.
	TryAdmitRun func() (done func(), ok bool)
	// ShutdownErr, QueuedErr and BusyErr are use-time providers for the
	// host-package sentinel errors (ErrAgentShuttingDown,
	// ErrSummarizeQueued, ErrSessionBusy) so the leaf never holds or
	// exports a mutable copy and the caller keeps error identity.
	ShutdownErr func() error
	QueuedErr   func() error
	BusyErr     func() error
	// BeginCompact resolves the session's mailbox and begins compaction
	// ownership on it, in that order.
	BeginCompact func(sessionID string, cancel context.CancelFunc) (epoch uint64, ok bool)
	// RunSummarize resolves the session's mailbox (the same one
	// BeginCompact acquired) and runs the compaction under it.
	RunSummarize func(ctx, genCtx context.Context, sessionID string, snapshot *agentcall.SummarizeSnapshot, epoch uint64, cancel context.CancelFunc, queueOnLockFailure bool) error
}

// SummarizeAdmit is the manual-summarize admission (Summarize/SummarizeNow's
// shared body): admit one unit of runWg work, open the 10-minute generation
// context, and either claim mailbox compaction ownership or queue the
// snapshot for the session's next owner.
func SummarizeAdmit(deps SummarizeAdmission, ctx context.Context, sessionID string, snapshot *agentcall.SummarizeSnapshot, queueWhenBusy bool) error {
	// Track this Summarize() call in the agent-wide runWg so CancelAll can join
	// on manual compactions that might be writing to the DB (P0-4).
	// This defer fires on EVERY return from Summarize, including the queued
	// early-return path and all error paths. The check and Add are gated
	// under admitMu (tryAdmitRunWg) to close the P1-1 race: without the mutex,
	// a concurrent CancelAll could set shuttingDown and start its Wait goroutine
	// while the counter was still 0, letting Wait return before Add(1) landed
	// (panics or admits work after "shutdown complete").
	done, ok := deps.TryAdmitRun()
	if !ok {
		return deps.ShutdownErr()
	}
	defer done()
	genCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	// Atomic check-and-reserve (#268/P0-4, design §6): beginCompact makes
	// us the sole owner of sessionID's mailbox or returns false if a turn
	// or another compaction already owns it. This replaces the old
	// non-atomic IsSessionBusy + runSummarize pair that had a TOCTOU window
	// between the busy check and the compaction's DB writes — a concurrent
	// Run could start between the check and the first delete, corrupting
	// history.
	epoch, ok := deps.BeginCompact(sessionID, cancel)
	if !ok {
		cancel()
		if !queueWhenBusy {
			return fmt.Errorf("session %q is already processing another request: %w", sessionID, deps.BusyErr())
		}
		deps.Queue.Set(sessionID, snapshot)
		return deps.QueuedErr()
	}
	return deps.RunSummarize(ctx, genCtx, sessionID, snapshot, epoch, cancel, queueWhenBusy)
}

// DedupInjects splices a drained inject batch into the current prompt,
// skipping entries whose rows the preamble's getSessionMessages already
// loaded.
//
// Two rules, one per half of P1-1:
//   - drainInjects(genID) returns only entries stamped at or before THIS
//     turn's generation, so an inject that landed after a previous turn's
//     last PrepareStep is picked up by this turn rather than stranded (the
//     LOSS half).
//   - historyIDs skips any inject whose row the preamble's
//     getSessionMessages already loaded, so a DB write that raced ahead of
//     the preamble does not appear both in history and in the splice (the
//     DUPLICATION half).
//
// id and payload extract the dedup key and the spliced message from each
// element of due; the element type stays generic because the pending-inject
// struct belongs to the host package.
func DedupInjects[T any](due []T, historyIDs map[string]struct{}, id func(T) string, payload func(T) message.Message) []message.Message {
	if len(due) == 0 {
		return nil
	}
	spliced := make([]message.Message, 0, len(due))
	for _, inj := range due {
		if _, ok := historyIDs[id(inj)]; ok {
			continue
		}
		spliced = append(spliced, payload(inj))
	}
	return spliced
}

// HandleWatchdogFire is runTurn's stream-watchdog onFire callback, moved out
// into a named leaf function (task #243, EX-0H) so a unit test can invoke the
// REAL production logic directly — calling this function with a synthetic
// watchdogCauseVal — instead of only exercising a test-local copy of this
// shape that could silently drift from what runTurn actually wires up. The
// onFire closure inside runTurn is just a thin dispatch:
// `func(elapsed, cause) { a.handleWatchdogFire(...) }`.
//
// watchdogCauseVal is runTurn's local atomic.Int32 (not a sessionAgent
// field: it is genuinely per-turn state, reset fresh on every call), passed
// in by pointer so this function can store into the caller's copy.
// toolMaxDuration/idleTimeout are likewise runTurn locals (the resolved,
// possibly-overridden effective durations for THIS turn) rather than
// sessionAgent fields, so they are passed explicitly instead of read off a.
// smartModel follows the same rule, and then some: runTurn does NOT re-read
// a.smartModel here at fire time — it passes in the value it took once,
// before the turn, from the immutable turnConfig snapshot built by
// resolveTurnConfig, a per-call value copy of every shared model/prompt
// field (task #265 P0-1) that runTurn reads as `smartModel := cfg.smartModel`.
// a.smartModel is mutable mid-turn via SetModels (coordinator.UpdateModels /
// web-UI override path), so re-reading at fire time would name the model the
// user SWITCHED TO after the hang started rather than the one that actually
// hung (task #252 — the #243 extraction regressed exactly this by re-reading
// a.smartModel here).
//
// hardCap is the host's timeoutHardCapCfg method value (config lookup), NOT
// a resolved duration: it is called only in the causeHardCap branch below,
// exactly where the original body called h.timeoutHardCapCfg() — other fire
// causes pay no config-read cost.
//
// INVARIANT (task #227 + #232, preserved verbatim by this extraction): the
// cause is stored FIRST, synchronously, before any other work in this
// function. startStreamWatchdog invokes onFire strictly before cancel(), so
// whatever this function does before returning is on the critical path to
// unblocking agent.Stream. If the OUTER ctx is also cancelled independently
// of this watchdog (Ctrl-C, a --timeout firing at the same moment) while
// this function is still running, the main goroutine reading
// watchdogCauseVal after observing stalled==true must never see the zero
// value (CauseIdleStall) for what was actually a hard-cap/tool-timeout fire —
// that would misreport the cause via a DIFFERENT race than the one #227
// fixed (external cancellation racing this callback, rather than this
// callback racing its own cancel()).
func HandleWatchdogFire(
	cause watchdog.Cause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel agentcall.Model,
	hardCap func() time.Duration,
) {
	watchdogCauseVal.Store(int32(cause))
	// The watchdog firing IS the hang, caught at the only moment the
	// evidence still exists. Capture every goroutine's stack now,
	// SYNCHRONOUSLY: pprof is gated behind RUSH_PROFILE (so it can't be
	// turned on after the fact) and release builds strip symbols (so a
	// debugger attach yields nothing and merely kills the process). Without
	// this, every production hang is diagnosed by guesswork.
	//
	// CaptureGoroutineStack does no I/O — it's runtime.Stack plus a string
	// header — so it cannot block on a stuck disk, and running it
	// synchronously here is what guarantees the snapshot reflects the
	// actual moment of the hang. Capturing it from an async goroutine
	// instead (as an earlier version of this fix did) would let it run
	// AFTER cancel()/unwind had already started — or, if the process exited
	// or was force-killed first, never run at all — defeating the entire
	// point of a diagnostic taken "at the only moment it is still
	// available" (see the doc comment on CaptureGoroutineStack).
	stackDump := rushlog.CaptureGoroutineStack("stream watchdog fired")
	// Only the WRITE is dispatched async and NOT awaited: WriteGoroutineDump
	// does a synchronous os.WriteFile with no timeout of its own (see its
	// doc in internal/log/goroutine_dump.go). Since onFire now runs
	// strictly before cancel(), awaiting a write that hangs (e.g. the log
	// directory sits on a stuck network/SMB mount) would mean cancel()
	// never runs, agent.Stream never unblocks, and runTurn's deferred
	// <-wd.done blocks forever — a full process freeze, exactly the failure
	// mode this watchdog exists to prevent. The dump is best-effort
	// diagnostics only; nothing downstream needs the write to complete
	// before the turn can safely unwind, so firing it off and returning
	// immediately preserves the already-captured evidence without putting
	// an unbounded disk write on cancellation's critical path.
	go func() {
		if dumpPath, dumpErr := rushlog.WriteGoroutineDump(stackDump); dumpErr != nil {
			slog.Warn("agent: failed to write goroutine dump for watchdog fire", "err", dumpErr)
		} else {
			slog.Warn("agent: wrote goroutine dump for watchdog fire", "path", dumpPath)
		}
	}()
	switch cause {
	case watchdog.CauseToolTimeout:
		slog.Warn(
			"agent: watchdog firing — tool execution exceeded cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"cap", toolMaxDuration.String(),
		)
	case watchdog.CauseHardCap:
		slog.Warn(
			"agent: watchdog firing — turn exceeded --timeout-hard-cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"hard_cap", hardCap().String(),
		)
	default:
		slog.Warn(
			"agent: stream watchdog firing — no provider activity, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"idle_duration", elapsed.String(),
			"threshold", idleTimeout.String(),
		)
	}
}

// RequeueSummarizeSnapshot puts a compaction back in the summarize queue after a
// lock-acquisition failure so the session's next owner drains and runs it.
// GetOrSet (not Set) never clobbers a newer queued request, and no compaction
// is spawned here, so a long-held lock cannot cause a busy retry loop.
func RequeueSummarizeSnapshot(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string, snapshot *agentcall.SummarizeSnapshot) {
	queue.GetOrSet(sessionID, func() *agentcall.SummarizeSnapshot { return snapshot })
	slog.Info("agent.runSummarize: re-queued compaction after session lock failure",
		"session_id", sessionID)
}

func SummarizeQueued(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) bool {
	_, ok := queue.Get(sessionID)
	return ok
}

func TakeSummarizeQueue(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) (*agentcall.SummarizeSnapshot, bool) {
	snapshot, ok := queue.Take(sessionID)
	return snapshot, ok
}

func CancelQueuedSummarize(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) {
	queue.Del(sessionID)
}

// SummarizeBodyDeps bundles the host seams RunSummarizeBody needs. Sessions
// and Messages are lazy service accessors (the original
// h.compactionSessions()/h.compactionMessages() calls); SummaryPrompt and
// CommitMaxDuration are the root's embedded template and commit-phase bound;
// the remaining funcs are the root's shared helpers, passed as values so the
// leaf holds no mutable root state.
type SummarizeBodyDeps struct {
	Sessions           func() coresession.Service
	Messages           func() message.Service
	NotifyActivity     func(ctx context.Context)
	NotifyWatchdog     func(ctx context.Context)
	PreparePrompt      func(msgs []message.Message, todos []coresession.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart)
	GetSessionMessages func(ctx context.Context, session coresession.Session) ([]message.Message, error)
	// SummaryPrompt is a use-time provider for the root's embedded template so
	// the fresh byte-slice→string allocation only happens when the prompt is
	// actually handed to the provider (after the early-error paths).
	SummaryPrompt      func() string
	CommitMaxDuration  time.Duration
	BuildSummaryPrompt func(todos []coresession.Todo) string
	CompletionTokens   func(usage fantasy.Usage, summaryMessage message.Message) int64
	SessionHeaders     func(sessionID string) map[string]string
	NormalizeUsage     func(provider string, usage fantasy.Usage) fantasy.Usage
	OpenrouterCost     func(metadata fantasy.ProviderMetadata) *float64
	UpdateSessionUsage func(model agentcall.Model, sess *coresession.Session, usage fantasy.Usage, overrideCost *float64) float64
	RecordMessageUsage func(ctx context.Context, messageID string, model agentcall.Model, usage fantasy.Usage, costDelta float64, estimated bool)
}

// RunSummarizeBody performs the actual summarisation: load messages, snapshot
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
func RunSummarizeBody(deps SummarizeBodyDeps, ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel agentcall.Model, systemPromptPrefix string) error {
	// Heartbeat attribution: summary runs on the smart slot (design pt. 1).
	role, source := callbeat.HbInherited(ctx)
	ctx = callbeat.WithHeartbeat(ctx, sessionID, heartbeat.PurposeSummary, role, source)
	currentSession, err := deps.Sessions().Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := deps.GetSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	// Snapshot non-pinned message IDs for deletion AFTER the summary stream
	// completes. The summary message itself is created BELOW, after this
	// snapshot, so it is never in toDelete. Pinned messages stay.
	var toDelete []message.Message
	for _, m := range msgs {
		if !m.Pinned {
			toDelete = append(toDelete, m)
		}
	}

	aiMsgs, _ := deps.PreparePrompt(msgs, nil, smartModel.Model.Provider())

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(deps.SummaryPrompt()),
		fantasy.WithUserAgent(agentcall.UserAgent),
	)
	summaryMessage, err := deps.Messages().Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            smartModel.Model.Model(),
		Provider:         smartModel.Model.Provider(),
		ReasoningEffort:  smartModel.ModelCfg.ReasoningEffort,
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := deps.BuildSummaryPrompt(currentSession.Todos)
	var discardRetryContent bool
	truncateAbandonedContent := func() error {
		if !discardRetryContent {
			return nil
		}
		discardRetryContent = false
		if !summaryMessage.TruncateStreamedContent() {
			return nil
		}
		return deps.Messages().Update(ctx, summaryMessage.Clone())
	}

	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		MaxRetries:      &agentcall.ProviderMaxRetries,
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         deps.SessionHeaders(sessionID),
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
			deps.NotifyActivity(ctx)
			deps.NotifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.StartReasoningContent(reasoning.Text)
			return deps.Messages().Update(ctx, summaryMessage)
		},
		OnReasoningDelta: func(id string, text string) error {
			deps.NotifyActivity(ctx)
			deps.NotifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.AppendReasoningContent(text)
			return deps.Messages().Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			deps.NotifyActivity(ctx)
			deps.NotifyWatchdog(ctx)
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return deps.Messages().Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			deps.NotifyActivity(ctx)
			deps.NotifyWatchdog(ctx)
			if err := truncateAbandonedContent(); err != nil {
				return err
			}
			summaryMessage.AppendContent(text)
			return deps.Messages().Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// User cancelled summarize; remove the summary message.
			// Use a bounded cancel-immune context for cleanup (P1-4): the
			// stream's context is already canceled, so Delete would silently
			// fail and leave an orphaned summary message in the DB.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), deps.CommitMaxDuration)
			defer cleanupCancel()
			deleteErr := deps.Messages().Delete(cleanupCtx, summaryMessage.ID)
			if deleteErr != nil {
				slog.Error("Failed to delete orphaned summary message after cancel", "session_id", sessionID, "err", deleteErr)
			}
			return err
		}
		// Mark the summary message as finished with an error so the UI
		// stops spinning. Use a bounded cancel-immune context for cleanup (P1-4).
		summaryMessage.AddFinish(message.FinishReasonError, "Summarization Error", err.Error())
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), deps.CommitMaxDuration)
		defer cleanupCancel()
		if updateErr := deps.Messages().Update(cleanupCtx, summaryMessage); updateErr != nil {
			slog.Error("Failed to mark summary message as error", "session_id", sessionID, "err", updateErr)
			return updateErr
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), deps.CommitMaxDuration)
	defer cleanupCancel()
	if err = deps.Messages().Update(cleanupCtx, summaryMessage); err != nil {
		return err
	}

	// COMMIT PHASE — the commit must not be tearable by cancellation.
	//
	// Without WithoutCancel, a cancellation (Stop, CancelAll, --timeout, stream
	// watchdog) could land between SetSummaryAndUsage and the deletes, leaving
	// the session in a holed state: summary pointer is set but old messages
	// still exist, getSessionMessages sees neither, history is unrecoverably
	// corrupt.
	//
	// Bounded rather than unbounded: the provider stream is already finished,
	// so what remains is local DB work. Uses the same timeout as
	// runSummarizeSilent.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), deps.CommitMaxDuration)
	defer commitCancel()

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := deps.OpenrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	// Re-fetch the session to pick up any user edits that happened while the
	// summary was streaming, then overlay our own fields.
	freshSession, err := deps.Sessions().Get(commitCtx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to re-fetch session before save: %w", err)
	}
	// Normalize once, upstream of both sinks (see agent_turn.go's OnStepFinish).
	totalUsage := deps.NormalizeUsage(smartModel.Model.Provider(), resp.TotalUsage)
	costDelta := deps.UpdateSessionUsage(smartModel, &freshSession, totalUsage, openrouterCost)
	// Heartbeat usage: one add per accounted compaction (design pt. 4).
	callbeat.AddHeartbeatUsage(ctx, smartModel.Model.Provider(), smartModel.Model.Model(), totalUsage, costDelta)
	if costDelta != 0 {
		if _, costErr := deps.Sessions().IncrementCost(commitCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}

	// Per-message usage for the summary turn itself (task #469). Recorded
	// against totalUsage — the same figure updateSessionUsage just billed —
	// so the summarization's own token cost is visible in analytics instead
	// of appearing as an unattributed jump in the session total.
	deps.RecordMessageUsage(commitCtx, summaryMessage.ID, smartModel, totalUsage, costDelta, false)

	usage := resp.Response.Usage
	if err := deps.Sessions().SetSummaryAndUsage(commitCtx, freshSession.ID, summaryMessage.ID, 0, deps.CompletionTokens(usage, summaryMessage)); err != nil {
		// Nothing has been deleted yet, so the session is still whole: the
		// compaction simply did not happen, and the next turn retries it.
		return err
	}

	// Now that the summary is persisted and SummaryMessageID is wired up,
	// drop the historical non-pinned messages. The summary message itself was
	// created AFTER the snapshot above so it is not in toDelete.
	for _, m := range toDelete {
		if delErr := deps.Messages().Delete(commitCtx, m.ID); delErr != nil {
			slog.Warn("summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}

	return nil
}

// RunSummarizeDeps bundles the host seams RunSummarize needs. Every field is
// either a value resolved once by the root at the original point (the
// watchdog start configuration) or a use-time provider/callback; the leaf
// holds no mutable root state and never sees the *mailbox, which stays
// package-private in the root behind the narrow callbacks below (the test
// seams in particular are read lazily inside the root's closures, at the
// original call sites).
type RunSummarizeDeps struct {
	// Queue is the summarize queue; Requeue re-queues the snapshot after a
	// lock-acquisition failure (root requeueSummarizeSnapshot) and TakeQueue
	// drains it on the success path (root takeSummarizeQueue), returning
	// whether an entry was actually taken (for the coalesce log).
	Queue     *csync.Map[string, *agentcall.SummarizeSnapshot]
	Requeue   func()
	TakeQueue func() bool

	// Watchdog start configuration, each resolved once by the root exactly
	// where the original body read it. HardCap is the exception: the original
	// body re-read h.timeoutHardCapCfg() at fire time inside the
	// causeHardCap log branch, so it stays a use-time provider.
	// ExtendsOnProgress is likewise evaluated at the original argument site
	// (the StartStreamWatchdog call below), once.
	IdleTimeout        time.Duration
	WatchdogTick       time.Duration
	ExtendsOnProgress  func() bool
	HardCap            func() time.Duration
	NotifyActivity     func(ctx context.Context)
	WithWatchdogBump   func(ctx context.Context, bump func()) context.Context
	WithActivityNotify func(ctx context.Context, lk *sessionlock.SessionLock) context.Context

	// DataDir/LockOptions are use-time reads of the root config at lock
	// acquisition, exactly as the original body read h.dataDirCfg() and
	// h.lockOptionsCfg() at that point. QueuedErr/BusyErr are use-time
	// providers for the host sentinel errors so error identity stays in the
	// root.
	DataDir     func() string
	LockOptions func() []sessionlock.LockOption
	QueuedErr   func() error
	BusyErr     func() error

	// RunBody runs the compaction body (root runSummarizeBody) on the
	// watchdog-wrapped genCtx, using the caller's already-captured snapshot.
	RunBody func(ctx context.Context) error

	// Mailbox seams, all closures over the root-private *mailbox:
	// PreSnapshotSeam/PreAbandonSeam are the test-only seams, read lazily
	// inside the closures (nil-checked at the original points);
	// BeginRelease/FinishRelease bracket the mbReleasing window;
	// AbandonHandoff hands ownership back; AbandonPopFirst is the atomic
	// abandonOwnershipAndPopFirstSubmitted pair (P2-5).
	PreSnapshotSeam func()
	PreAbandonSeam  func()
	BeginRelease    func(epoch uint64) bool
	FinishRelease   func(epoch uint64)
	AbandonHandoff  func()
	AbandonPopFirst func() (agentcall.SessionAgentCall, bool)

	// RunQueued starts a fresh Run for the first queued call after a
	// successful compaction; only its error matters to this path.
	RunQueued func(ctx context.Context, call agentcall.SessionAgentCall) error
}

// RunSummarize is the manual /compact body under mailbox ownership acquired
// by Summarize's beginCompact call, moved verbatim from the agent package's
// turnBudget.runSummarize. After the compaction body completes, it releases
// ownership via AbandonHandoff (handing back any Run that queued behind it
// via mailbox.submit during the stream) and starts a fresh Run for the first
// queued call — the same drain shape the old runSummarizeCore tail had, now
// operating on mailbox ownership instead of a synthetic activeRequests key.
//
// #312: Cross-process compaction serialization
//
// Manual compaction (this path) acquires the OS session lock before running
// the compaction body and releases it after. This prevents another process
// from starting a Run() for the same session while the compaction's commit
// phase (set SummaryMessageID + delete messages) is in flight.
//
// Inline compaction paths (runTurn's shouldSummarize and silentCompactNeeded)
// do NOT go through this function — they call RunSummarizeBody or the silent
// variant directly while already holding the OS lock via the parent Run()
// call. No deadlock is possible because those paths never acquire a second
// lock.
//
// OS lock acquisition happens AFTER beginCompact succeeds and mb.mu is
// released, avoiding #296's I/O-under-mu anti-pattern. The lock is held for
// the entire compaction body call to protect both the LLM stream and the
// commit phase, matching Run()'s lock-hold semantics.
//
// The snapshot parameter contains the model, provider options, and prompt
// prefix resolved from the target session (or shared state for sessions
// without overrides), ensuring the entire summarize operation uses consistent
// configuration regardless of concurrent SetModels calls (task #341, P1-1).
func RunSummarize(deps RunSummarizeDeps, ctx context.Context, genCtx context.Context, sessionID string, snapshot *agentcall.SummarizeSnapshot, epoch uint64, cancel context.CancelFunc, queueOnLockFailure bool) error {
	// Start stream watchdog to detect idle stalls in the summarization stream.
	// No tools in manual compaction, so use 0 for tool bounds.
	toolMaxDuration := time.Duration(0)
	toolCleanupGrace := time.Duration(0)
	var watchdogCauseVal atomic.Int32
	wd := watchdog.StartStreamWatchdog(
		genCtx, cancel, deps.IdleTimeout, deps.WatchdogTick,
		// Use a simple callback - just log and cancel (no tools to report)
		func(elapsed time.Duration, cause watchdog.Cause) {
			watchdogCauseVal.Store(int32(cause))
			stackDump := rushlog.CaptureGoroutineStack("summarize watchdog fired")
			go func() {
				if dumpPath, dumpErr := rushlog.WriteGoroutineDump(stackDump); dumpErr != nil {
					slog.Warn("agent.runSummarize: failed to write goroutine dump", "err", dumpErr)
				} else {
					slog.Warn("agent.runSummarize: wrote goroutine dump", "path", dumpPath)
				}
			}()
			switch cause {
			case watchdog.CauseToolTimeout:
				slog.Warn(
					"agent.runSummarize: watchdog firing — tool execution exceeded cap, force-cancelling",
					"session_id", sessionID,
					"elapsed", elapsed.String(),
					"cap", toolMaxDuration.String(),
				)
			case watchdog.CauseHardCap:
				slog.Warn(
					"agent.runSummarize: watchdog firing — summarization exceeded hard cap, force-cancelling",
					"session_id", sessionID,
					"elapsed", elapsed.String(),
					"hard_cap", deps.HardCap().String(),
				)
			default:
				slog.Warn(
					"agent.runSummarize: watchdog firing — no provider activity, force-cancelling",
					"session_id", sessionID,
					"idle_duration", elapsed.String(),
					"threshold", deps.IdleTimeout.String(),
				)
			}
		},
		deps.ExtendsOnProgress(),
		deps.HardCap(),
		toolMaxDuration,
		toolCleanupGrace,
		// IMPORTANT: this closure captures the genCtx VARIABLE, exactly as
		// the original body did (`func() { notifyActivity(genCtx) }`) — not
		// a snapshot of its current value. The watchdog invokes recordActivity
		// during activity ticks that happen AFTER the reassignments below
		// (withWatchdogBump, and crucially withActivityNotify(lk)), so each
		// tick's activity notification walks the lock's activity chain; a
		// snapshot taken here would freeze the pre-lock context and silently
		// stop updating the session lock's mtime during watchdog activity
		// (the exact staleness #222's withActivityNotify wiring fixes).
		func() { deps.NotifyActivity(genCtx) },
	)
	// Wrap genCtx so notifyWatchdog calls reach this watchdog
	genCtx = deps.WithWatchdogBump(genCtx, wd.Bump)
	// Defer order matters: <-wd.Done first so it runs after cancel()
	defer func() { <-wd.Done }()
	defer cancel()

	// #312: Acquire OS session lock for cross-process serialization.
	// Manual compaction runs outside any Run() call, so the OS lock is not
	// already held. Acquiring it here prevents another process from starting
	// a concurrent Run() that would race with the compaction's DB writes.
	var lk *sessionlock.SessionLock
	if deps.DataDir() != "" {
		var lockErr error
		lk, lockErr = sessionlock.TryAcquireSessionLockWithOptions(deps.DataDir(), sessionID, deps.LockOptions()...)
		if lockErr != nil {
			var busyErr *sessionlock.SessionLockBusyError
			if errors.As(lockErr, &busyErr) {
				slog.Warn(
					"agent.runSummarize: rejected — session locked by another process",
					"session_id", sessionID,
					"holder_pid", busyErr.HolderPID,
					"lock_path", busyErr.Path,
				)
				// Release mailbox ownership with handoff so queued work gets a runner.
				deps.AbandonHandoff()
				if queueOnLockFailure {
					deps.Requeue()
					return fmt.Errorf("%w: session %q is already in use: %w", deps.QueuedErr(), sessionID, lockErr)
				}
				return fmt.Errorf("session %q is already in use: %w", sessionID, deps.BusyErr())
			}
			slog.Error("agent.runSummarize: failed to acquire inter-process session lock, refusing to compact unprotected",
				"session_id", sessionID, "err", lockErr)
			// Release mailbox ownership with handoff so queued work gets a runner.
			deps.AbandonHandoff()
			if queueOnLockFailure {
				deps.Requeue()
				return fmt.Errorf("%w: session %q: could not acquire session lock: %w", deps.QueuedErr(), sessionID, lockErr)
			}
			return fmt.Errorf("session %q: could not acquire session lock: %w", sessionID, lockErr)
		}
	}

	// Wire this lock into genCtx's activity-notify chain (task #222's
	// pattern, mirrored from runTurn's own withActivityNotify call) so
	// RunSummarizeBody's notifyActivity(ctx) calls on its stream callbacks
	// (#310) actually reach THIS lock. Without this, manual /compact
	// acquires its own lk above but never records activity on it — the
	// lock's mtime is frozen from acquisition until release, up to the full
	// 10-minute genCtx timeout, so `sessions locks`/`watch` can misreport a
	// healthy long-running /compact as heartbeat-stale. nil-receiver-safe
	// when lk is nil (a.dataDir == "").
	genCtx = deps.WithActivityNotify(genCtx, lk)

	// Test-only seam: fires strictly after the snapshot above was captured
	// and strictly before it is consumed below, letting a test land a
	// concurrent shared-state mutation deterministically inside the window a
	// pre-#341 regression would have re-read from. The seam field itself is
	// read lazily inside the root's closure, at the original point.
	deps.PreSnapshotSeam()

	// Use the snapshot provided by the caller. The snapshot contains the
	// model, provider options, and prompt prefix resolved from the target
	// session (or shared state for sessions without overrides), ensuring the
	// entire summarize operation uses consistent configuration regardless of
	// concurrent SetModels calls (task #341, P1-1).
	err := deps.RunBody(genCtx)

	// P1-2 fix (2026-08-09): Transition mailbox to mbReleasing before releasing
	// the OS lock. This provides the same visibility that normal turns get
	// via drainOrReleaseFinal, allowing diagnostic tools to distinguish "releasing
	// OS lock" from "still actively streaming". If the filesystem/AV/SMB hangs
	// during lk.Release(), mbReleasing is still observable as "busy but not
	// streaming", which is semantically correct - the session is technically busy
	// (ownership not yet relinquished to another caller) but not making progress.
	releaseStarted := deps.BeginRelease(epoch)

	// Release the OS lock. Even if beginRelease failed (epoch mismatch or state
	// already transitioned), we still need to release the lock to avoid leaking it.
	// The worst case is we release without mbReleasing visibility, which is the
	// old (pre-P1-2) behavior for that particular edge case.
	if lk != nil {
		if relErr := lk.Release(); relErr != nil {
			slog.Debug("agent.runSummarize: release session lock failed", "session_id", sessionID, "err", relErr)
		}
	}

	// Test-only seam: fires strictly after the OS lock above is released and
	// strictly before abandonOwnership below makes that visible as mbIdle to
	// same-process callers. Read lazily inside the root's closure.
	deps.PreAbandonSeam()

	// Complete the mbReleasing -> mbIdle transition if we successfully began it.
	// If beginRelease failed (epoch changed, wrong state), we skip this - the
	// actual state transition already happened elsewhere (e.g., a concurrent
	// abandonOwnership from a Cancel or shutdown).
	if releaseStarted {
		deps.FinishRelease(epoch)
	}

	if err != nil {
		deps.AbandonHandoff()
		return err
	}

	// Success path: release ownership, drain the mailbox queue, and coalesce
	// any pending summarize requests. If a second /compact was queued while
	// this one was running, we discard it (coalesce) because the history has
	// already been compressed by this successful compaction.
	//
	// Uses the atomic AbandonPopFirst (abandonOwnershipAndPopFirstSubmitted,
	// P2-5 follow-up) instead of a separate abandonOwnership() +
	// popFirstSubmitted() pair — two independent lock acquisitions here left
	// the same era-boundary reordering window abandonOwnershipWithHandoff's
	// own P2-5 fix closed for the "pop all" case: a new owner's submit()+queue()
	// landing in the gap between them could get scooped into firstQueued below
	// instead of staying queued for that owner's own turn.
	firstQueued, hasNext := deps.AbandonPopFirst()

	// Drain any queued summarize request - if a second /compact was queued
	// while this one was running, the history is already compressed, so we
	// coalesce by discarding the stale queued entry rather than executing it.
	// This is semantically correct: the queued request's goal (compress history)
	// is already satisfied by this compaction, and avoids redundant LLM calls.
	if deps.Queue != nil {
		if deps.TakeQueue() {
			slog.Debug("agent: queued summarize coalesced with successful compaction, discarded stale entry",
				"session_id", sessionID)
		}
	}

	if !hasNext {
		return nil
	}
	return deps.RunQueued(ctx, firstQueued)
}
