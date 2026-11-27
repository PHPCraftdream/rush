package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/text/stringext"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// turnCheckpointWriter is runTurn's mid-stream persistence mechanism (Fork
// patch: batch 8 — auto-checkpoint state. See CHANGELOG.fork.md section 6).
// Split out of agent_turn.go's runTurn when the 1000-line limit landed:
// unlike runTurn's other callbacks, this one owns a genuinely self-contained
// slice of state (its own generation counter, stop/done channels and write
// cancel) and only READS the two things runTurn shares with it — sessionLock
// and the current assistant message — never owns them.
//
// Invariant: sessionLock protects EVERY touch of the current assistant
// message — mutation, Clone(), and even a bare len(Parts)/pointer read —
// because this writer's goroutine and the streaming callbacks (OnTextDelta,
// OnReasoningDelta, OnToolInputStart, ...) run concurrently on separate
// goroutines. message.Message.Clone() has no synchronization of its own, so
// a snapshot must be taken while holding sessionLock.
//
// The lock must NEVER be held across messages.Update (the SQLite write):
// the writer takes the lock, clones a private snapshot, releases the lock,
// then calls Update on the snapshot without the lock held. Otherwise each
// checkpoint tick would stall the whole streaming loop for the duration of
// a disk write. OnStepFinish drains the ticker and stops the goroutine
// (via stop()) before its final write; runTurn's own tail also calls
// stop() defensively before touching the assistant message, in case
// agent.Stream returned before OnStepFinish ever ran (e.g. the very first
// provider call failed).
//
// generation fences concurrent checkpoint writes across turns: each start()
// increments it, the goroutine captures the current value at launch, and
// stop() returns only after observing the goroutine's done signal. This
// ensures a hung checkpoint from turn N cannot race with a new checkpoint
// from turn N+1, and that stop()'s 5s timeout is reflected in the agent's
// runWg (see the P0-4 fix in start()'s write-context comment). mu
// synchronizes all access to generation; there is no shared
// checkpointPartsLen — each generation tracks its own lastPartsLen locally.
//
// stopCh and doneCh are reborn on every step. start() allocates a fresh
// pair and launches the ticker goroutine; stop() closes stopCh — the
// goroutine's dedicated exit signal — then waits on doneCh for it to
// actually exit, then nils both so the next step starts clean. The exit
// signal MUST be a dedicated channel, NOT genCtx.Done(): genCtx stays alive
// for the whole body of Run (cancelled only by the deferred cancel() at
// runTurn's return), so relying on it would force stop() to always hit its
// 5s backstop — the ~10s/turn stall this mechanism replaces. start/stop run
// on fantasy's single callback goroutine / the Run goroutine (never
// concurrent with each other); the ticker goroutine captures local channel
// refs at launch, so nil-ing the outer fields after stop does not affect
// it.
type turnCheckpointWriter struct {
	a         *sessionAgent
	sessionID string
	genCtx    context.Context

	// sessionLock and assistant are runTurn's own -- shared with every other
	// streaming callback, never owned here. assistant is a pointer to
	// runTurn's currentAssistant local so every read sees the latest value
	// PrepareStep/the streaming callbacks assign, exactly as a closure
	// capturing that local directly would.
	sessionLock *sync.Mutex
	assistant   **message.Message

	// stall is the turn's stall clock; each successful changed checkpoint
	// persist is a durable progress point.
	stall *drainctl.StallClock

	mu          sync.Mutex
	generation  int64
	stopCh      chan struct{}
	doneCh      chan struct{}
	writeCancel context.CancelFunc
}

func newTurnCheckpointWriter(a *sessionAgent, sessionID string, genCtx context.Context, sessionLock *sync.Mutex, assistant **message.Message, stall *drainctl.StallClock) *turnCheckpointWriter {
	return &turnCheckpointWriter{
		a:           a,
		sessionID:   sessionID,
		genCtx:      genCtx,
		sessionLock: sessionLock,
		assistant:   assistant,
		stall:       stall,
	}
}

func (cp *turnCheckpointWriter) start() {
	if cp.a.checkpointInterval <= 0 || cp.stopCh != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	cp.stopCh = stop
	cp.doneCh = done
	// Fence the checkpoint writer so a hung write from turn N
	// cannot race with a new writer from turn N+1. The goroutine
	// captures the current generation at launch; stop() returns
	// only after observing the goroutine's done signal.
	cp.mu.Lock()
	cp.generation++
	myGeneration := cp.generation
	cp.mu.Unlock()
	// The generation context is deliberately cancellable by stop(), but has no
	// per-write deadline: each Update gets a fresh 30s budget from this parent.
	writeCtxParent, writeCancel := context.WithCancel(context.Background())
	cp.writeCancel = writeCancel
	// Register in runWg so a timeout reflects in stillBusy (P0-4).
	cp.a.runWg.Add(1)
	go func() {
		defer cp.a.runWg.Done()
		defer close(done)
		defer writeCancel()
		ticker := time.NewTicker(cp.a.checkpointInterval)
		defer ticker.Stop()
		// Keep coalescing state LOCAL to this generation to eliminate
		// cross-generation races. Each writer tracks its own lastPartsLen and
		// only writes if there's new content since its last write.
		lastPartsLen := 0
		lastContentBytes := 0
		for {
			select {
			case <-stop:
				return
			case <-cp.genCtx.Done():
				return
			case <-ticker.C:
				// stop() closes `stop` BEFORE cancelling writeCtx
				// (see its comment), so a tick that was already queued
				// while the previous write was blocked can become ready
				// in the SAME instant `stop` does — select's case order
				// above gives no priority, so it can pick ticker.C over
				// stop by chance. Re-checking non-blockingly here closes
				// that window: a write must never be attempted with a
				// writeCtx that stop() may have already
				// cancelled, since that write's failure would be
				// indistinguishable from any other cancellation to the
				// caller but still represents wasted, racy work the
				// goroutine was explicitly told to stop before reaching.
				select {
				case <-stop:
					return
				case <-cp.genCtx.Done():
					return
				default:
				}
				cp.sessionLock.Lock()
				var snap message.Message
				haveSnap := false
				var currentPartsLen int
				var currentContentBytes int
				cp.mu.Lock()
				isCurrentGen := myGeneration == cp.generation
				cp.mu.Unlock()
				if *cp.assistant != nil && isCurrentGen {
					currentPartsLen = len((*cp.assistant).Parts)
					currentContentBytes = checkpointContentBytes((*cp.assistant).Parts)
					// Text and reasoning deltas grow existing parts without changing
					// the part count, so length alone misses streamed content.
					if currentContentBytes != lastContentBytes || currentPartsLen != lastPartsLen {
						snap = (*cp.assistant).Clone()
						// Stamp the snapshot with THIS writer's generation
						// so the conditional update can reject it if a
						// newer writer already wrote (P1-3). The isCurrentGen
						// check above is not enough on its own: it runs
						// here, under sessionLock, while the DB write below
						// happens after the lock is released.
						snap.CheckpointGeneration = myGeneration
						snap.AddFinish(message.FinishReasonUnknown, "", "")
						for i := len(snap.Parts) - 1; i >= 0; i-- {
							if f, ok := snap.Parts[i].(message.Finish); ok {
								f.Partial = true
								snap.Parts[i] = f
								break
							}
						}
						haveSnap = true
					}
				}
				cp.sessionLock.Unlock()
				if haveSnap {
					// Each checkpoint attempt gets a fresh bounded budget, while
					// remaining cancellable by stop() through the parent.
					updateCtx, updateCancel := context.WithTimeout(writeCtxParent, 30*time.Second)
					// P0-4: stop() cancels writeCtxParent immediately to fence an
					// in-flight DB write.
					err := cp.a.messages.Update(updateCtx, snap)
					updateCancel()
					if err != nil {
						// Don't log cancelled errors as failures — they're
						// the expected outcome of stop() fencing.
						if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
							slog.Debug(
								"agent: checkpoint flush failed",
								"session_id", cp.sessionID,
								"message_id", snap.ID,
								"err", err,
							)
						}
					} else {
						// Update our local coalescing state and stall clock only
						// after the changed snapshot has been durably written.
						lastPartsLen = currentPartsLen
						lastContentBytes = currentContentBytes
						cp.stall.MarkProgress()
					}
				}
			}
		}
	}()
}

func checkpointContentBytes(parts []message.ContentPart) int {
	var size int
	for _, part := range parts {
		switch content := part.(type) {
		case message.TextContent:
			size += len(content.Text)
		case message.ReasoningContent:
			size += len(content.Thinking)
		}
	}
	return size
}

// fenceFinalization prevents an old partial checkpoint from replacing a candidate.
func (cp *turnCheckpointWriter) fenceFinalization() int64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.generation++
	return cp.generation
}

func (cp *turnCheckpointWriter) stop() {
	if cp.stopCh == nil {
		return
	}
	close(cp.stopCh)
	cp.stopCh = nil
	// Cancel the write's own context immediately rather than waiting out
	// the 5s grace below first (P0-4). A checkpoint write in flight when
	// stop is requested has nothing left to accomplish — the turn is
	// ending — so there is no reason to let it keep holding a DB
	// connection for up to 5 more seconds (or longer, unbounded, if the
	// underlying driver never itself times out) before this function
	// even starts waiting. If the goroutine is between ticks (not
	// writing), cancelling here is a harmless no-op; the ticker loop's
	// own <-stop case still handles the ordinary shutdown path.
	if cp.writeCancel != nil {
		cp.writeCancel()
		cp.writeCancel = nil
	}
	select {
	case <-cp.doneCh:
	case <-time.After(5 * time.Second):
		slog.Warn(
			"agent: checkpoint goroutine did not exit within 5s of stop signal",
			"session_id", cp.sessionID,
		)
	}
	cp.doneCh = nil
}

// RunTimeoutCause remains a compatibility alias for internal/app callers,
// which still construct timeout causes through the agent package.
type RunTimeoutCause = watchdog.RunTimeoutCause

// handleStreamFailure is runTurn's error path once agent.Stream returns a
// non-nil err (already passed through normalizeTurnError by the caller).
// Moved out mechanically (task #933/#938) — this was the largest cluster
// runTurn's callback set had that an earlier pass over this function missed
// entirely, since it isn't a fantasy callback at all, just the block of
// runTurn's own body that runs right after the Stream call returns. It is a
// pure function of ts's settled callback state plus the two locals runTurn
// still owns at the call site (result, userMessageCreated): no defer, no
// goroutine, nothing outlives this call.
func (ts *turnStream) handleStreamFailure(
	result *fantasy.AgentResult,
	err error,
	userMessageCreated bool,
) (*fantasy.AgentResult, SessionAgentCall, bool, error) {
	isHyper := ts.smartModel.ModelCfg.Provider == hyper.Name
	isCancelErr := errors.Is(err, context.Canceled)
	isWatchdogStall := isCancelErr && ts.wd.Stalled.Load()
	// Turn-stall abort (watchdog.CauseStall): join ErrTurnStalled BEFORE the
	// ErrCallAlreadyAttempted wrap so the CLI's exit mapping can classify
	// the run as "stalled" through the Unwrap chain.
	if isWatchdogStall && ts.watchdogCauseVal != nil && watchdog.Cause(ts.watchdogCauseVal.Load()) == watchdog.CauseStall {
		err = errors.Join(err, ErrTurnStalled)
	}
	// The TURN's own context decides what a deadline error means: a net/http
	// timeout satisfies errors.Is(err, context.DeadlineExceeded) with a live
	// turn context and is a provider failure, not an operator stop.
	turnCtxDone := ts.genCtx.Err() != nil
	if ts.att != nil {
		// A watchdog stall surfaces as context.Canceled but is a real,
		// paid attempt (accountDrainAttempt does not exempt it).
		ts.att.Stalled.Store(isWatchdogStall)
		ts.att.TurnCtxDone.Store(turnCtxDone)
		ts.att.StreamErr = err
	}
	// `rush run --timeout` bounds the whole invocation via
	// context.WithTimeout on the root ctx (run.go); when it fires
	// mid-turn, ctx.Err() is context.DeadlineExceeded, NOT
	// context.Canceled, so isCancelErr above never catches it. Without
	// this branch it fell into the generic `else` below as "Provider
	// Error" with a bare "context deadline exceeded" — indistinguishable
	// from a real provider failure and useless to `sessions why`. Only a
	// deadline of the turn's own context counts: a transport timeout
	// (net/http Client.Timeout) is a provider failure and takes the generic
	// branch, not this "--timeout" text.
	isRunTimeout := errors.Is(err, context.DeadlineExceeded) && turnCtxDone
	// If userMessageCreated is true (either we just created it or
	// call.ExistingMessageID was set), the call has already left a
	// persistent trace. Wrap the error to prevent duplicate execution
	// on retry (task #339). This handles ALL errors after user message
	// creation, not just those after currentAssistant is set.
	//
	// The wrapping must happen BEFORE we check nilAssistant because
	// errors in PrepareStep (before currentAssistant is set) also need
	// to be wrapped. See call_attempted_error.go for the design rationale.
	if userMessageCreated {
		err = &ErrCallAlreadyAttempted{Err: err}
	}
	// currentAssistant is only ever reassigned (never set back to nil)
	// by PrepareStep, under mu. agent.Stream has already
	// returned by this point so no streaming callback can race this
	// read, but the peak-hours watcher goroutine may still be alive
	// (it only stops when genCtx is cancelled by the deferred cancel()
	// at the end of Run) and touches currentAssistant under the same
	// lock, so guard the read too.
	ts.mu.Lock()
	nilAssistant := ts.currentAssistant == nil
	ts.mu.Unlock()
	if nilAssistant {
		// A cancellation during PrepareStep can fail before fantasy has created
		// currentAssistant. Recover the mailbox replacement before returning:
		// otherwise Run's abandonment path treats its live waiter as an orphan
		// and splitCallsWithoutWaiters resolves it as never run.
		if isCancelErr {
			if next, ok := ts.a.getMailbox(ts.call.SessionID).DrainAfterCancel(); ok {
				ts.cancel()
				return result, next, true, nil
			}
		}
		return result, SessionAgentCall{}, false, err
	}
	// All DB writes in the error path use a detached context. The outer
	// ctx may itself be cancelled — in `rush run` it's the
	// signal.NotifyContext from fang, so Ctrl-C cancels it too; in the
	// web UI a request abort cancels it; the stream watchdog above
	// cancels genCtx (whose parent is ctx, so it doesn't cancel ctx,
	// but defensively we still detach). Without a detached ctx the
	// finish part Update fails with context.Canceled and the assistant
	// ends up half-saved in the DB — the "silent dying" pattern
	// observed in 162-promise-all. Codec must surface control: the
	// finish part MUST land on disk before we return.
	flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ts.ctx), 15*time.Second)
	defer flushCancel()
	// Turn-stall abort threshold for finish/tool-result wording (nil-safe).
	var stallTimeout time.Duration
	if ts.call.CallOptions != nil {
		stallTimeout = ts.call.CallOptions.TurnStallTimeout
	}
	// Ensure we finish thinking on error to close the reasoning state.
	// From here to the final flush below, currentAssistant's Parts are
	// mutated in place; every touch (including the plain reads used to
	// build msgs/toolCalls) takes mu to stay consistent with
	// the peak-hours watcher goroutine that may still be running.
	ts.mu.Lock()
	ts.currentAssistant.FinishThinking()
	toolCalls := ts.currentAssistant.ToolCalls()
	assistantID := ts.currentAssistant.ID
	sessionID := ts.currentAssistant.SessionID
	ts.mu.Unlock()
	msgs, createErr := ts.a.messages.List(flushCtx, sessionID)
	if createErr != nil {
		return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
	}
	for _, tc := range toolCalls {
		if !tc.Finished {
			tc.Finished = true
			tc.Input = "{}"
			ts.mu.Lock()
			ts.currentAssistant.AddToolCall(tc)
			snap := ts.currentAssistant.Clone()
			ts.mu.Unlock()
			updateErr := ts.a.messages.Update(flushCtx, snap)
			if updateErr != nil {
				return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
			}
		}

		if hasToolResultAfterAssistantPosition(msgs, assistantID, tc.ID) {
			continue
		}
		content := "There was an error while executing the tool"
		if isWatchdogStall {
			content = watchdog.ToolResultMessage(
				watchdog.Cause(ts.watchdogCauseVal.Load()),
				ts.toolMaxDuration,
				ts.timeoutHardCap,
				ts.idleTimeout,
				stallTimeout,
				ts.smartModel.ModelCfg.Provider,
			)
		} else if isCancelErr {
			content = "Error: user cancelled assistant tool calling"
		}
		toolResult := message.ToolResult{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    content,
			IsError:    true,
		}
		_, createErr = ts.a.messages.Create(flushCtx, sessionID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{
				toolResult,
			},
		})
		if createErr != nil {
			return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
		}
	}
	var fantasyErr *fantasy.Error
	var providerErr *fantasy.ProviderError
	var peakErr *usagepolicy.PeakHoursError
	var awaitingErr *AwaitingAnswerError
	const defaultTitle = "Provider Error"
	// None of the branches below perform I/O — they only decide which
	// AddFinish to record based on err/isWatchdogStall/etc. — so the
	// whole chain can run under a single lock/unlock pair guarding the
	// currentAssistant mutation, matching the pattern used everywhere
	// else in Run().
	ts.mu.Lock()
	if isWatchdogStall {
		// Close the observability loop: the watchdog goroutine already
		// emitted its slog.Warn at fire-time, but a log reader
		// chasing the trail needs to see that the stall actually
		// made it into the user-visible finish part on this session.
		slog.Info(
			"agent: watchdog stall surfaced as FinishReasonError",
			"session_id", ts.call.SessionID,
			"provider", ts.smartModel.ModelCfg.Provider,
		)
		cause := watchdog.Cause(ts.watchdogCauseVal.Load())
		title, _ := watchdog.FinishMessage(
			cause,
			ts.toolMaxDuration,
			ts.timeoutHardCap,
			ts.idleTimeout,
			stallTimeout,
			ts.smartModel.ModelCfg.Provider,
		)
		body := watchdog.ComposeFinishBody(ts.call.SessionID, cause, ts.toolMaxDuration, ts.timeoutHardCap, ts.idleTimeout, stallTimeout, ts.smartModel.ModelCfg.Provider)
		ts.currentAssistant.AddFinish(message.FinishReasonError, title, body)
	} else if isCancelErr {
		ts.currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
	} else if isRunTimeout {
		title, details := watchdog.RunTimeoutFinishText(context.Cause(ts.genCtx), ts.call.SessionID)
		ts.currentAssistant.AddFinish(message.FinishReasonError, title, details)
	} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized {
		ts.currentAssistant.AddFinish(message.FinishReasonError, "Unauthorized", `Please re-authenticate with Hyper. You can also run "rush auth" to re-authenticate.`)
	} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusPaymentRequired {
		url := hyper.BaseURL()
		ts.currentAssistant.AddFinish(message.FinishReasonError, "No credits", "You're out of credits. Add more at "+url)
	} else if errors.As(err, &providerErr) {
		if providerErr.Message == "The requested model is not supported." {
			url := "https://github.com/settings/copilot/features"
			ts.currentAssistant.AddFinish(
				message.FinishReasonError,
				"Copilot model not enabled",
				fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", ts.smartModel.CatwalkCfg.Name, url),
			)
		} else {
			// R979: a quota-classified 429 (isQuotaLimit) embeds the
			// provider's OWN reset timestamp, in whatever timezone the
			// remote server used -- rush ping already solved converting
			// that to local time (QuotaLimitGuidance, extracted from its
			// former pingRateLimitReset); reuse it here so `rush run`
			// stops showing the reset time in remote time. The raw
			// provider message is kept as-is and the local-time line is
			// appended, matching PeakHoursGuidance's own "add a precise
			// line, don't hide the underlying text" pattern. ok=false
			// (not a quota wall, or no parseable reset hint) leaves the
			// message exactly as before -- graceful degradation, no panic.
			// An inline stamp is rewritten in place (one reset, one time);
			// the extra line is only for resets that came from headers.
			details := usagepolicy.LocalizeResetHint(providerErr.Message, time.Now())
			if details == providerErr.Message {
				if guidance, ok := usagepolicy.QuotaLimitGuidance(err); ok {
					details = fmt.Sprintf("%s\n\n%s", providerErr.Message, guidance)
				}
			}
			ts.currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), details)
		}
	} else if errors.As(err, &fantasyErr) {
		ts.currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
	} else if errors.As(err, &peakErr) {
		// Re-derive the same (msg, details) OnStepFinish's peak-hours
		// check already wrote (with the RESUME AT guidance) — this
		// path (err forced to peakHoursAbortErr) ALSO reaches here,
		// and AddFinish always replaces the prior finish part, so
		// without this branch the generic `else` below would
		// overwrite the useful message with a bare "Provider Error:
		// <terse text>" that drops the resume-time guidance entirely.
		peakMsg, peakDetails := usagepolicy.PeakHoursStoppedFinishText(err)
		ts.currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
	} else if errors.As(err, &awaitingErr) {
		// Same rationale as the peakErr branch above: without this,
		// the generic `else` below would overwrite the question/options/
		// resume-command guidance with a bare "Provider Error: <text>".
		role := ""
		if ts.call.CallOptions != nil {
			role = string(ts.call.CallOptions.ModelRole)
		}
		awaitingMsg, awaitingDetails := awaitingAnswerStoppedFinishText(err, role)
		ts.currentAssistant.AddFinish(message.FinishReasonError, awaitingMsg, awaitingDetails)
	} else {
		// R2-5: this generic finish is the diagnostic path that persists
		// raw error text into the transcript's finish details; redact
		// URL secrets (userinfo/query) before it lands on disk.
		ts.currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, modelkit.RedactNetworkURLs(usagepolicy.LocalizeResetHint(err.Error(), time.Now())))
	}
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Detached flush (flushCtx is context.WithoutCancel + 15s timeout,
	// created at the top of this error block). This is the call that
	// MUST land on disk — without it the assistant message has tool
	// calls but no finish part, and the WUI/recovery sees it as still
	// in-flight forever.
	updateErr := ts.persistFailureFinish(flushCtx, snap, awaitingErr != nil)
	if updateErr != nil {
		slog.Error(
			"agent: failed to persist final finish part",
			"session_id", ts.call.SessionID,
			"err", updateErr,
		)
		return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
	}
	if !isCancelErr && !turnCtxDone && !ts.call.RecoveryContinuation && ts.recovery != nil &&
		ts.smartModel.Model.Provider() == "openai-codex" && codexContextRejectedBeforeOutput(err) &&
		len(toolCalls) == 0 && snap.FullText() == "" && snap.FullReasoningText() == "" {
		next, ok, recoveryErr := ts.a.recoverCodexContext(ts.genCtx, ts.call, ts.smartModel, ts.promptPrefix, ts.recovery)
		if recoveryErr == nil && ok {
			next.DrainTurnCommitted = next.IsDrain
			ts.cancel()
			return nil, next, true, nil
		}
		if recoveryErr != nil {
			slog.Warn("automatic Codex context recovery stopped", "session_id", ts.call.SessionID, "err", recoveryErr)
			recoveryFailure := errors.Join(err, fmt.Errorf("automatic context recovery failed: %w", recoveryErr))
			var attempted *ErrCallAlreadyAttempted
			if errors.As(err, &attempted) {
				attempted.Err = errors.Join(attempted.Err, fmt.Errorf("automatic context recovery failed: %w", recoveryErr))
				err = attempted
			} else {
				err = &ErrCallAlreadyAttempted{Err: recoveryFailure}
			}
		} else if ok {
			err = &ErrCallAlreadyAttempted{Err: errors.Join(err, errors.New("automatic recovery did not schedule a continuation"))}
		}
	}

	// Drain on cancel via the mailbox's generation-aware drain (design
	// §4): an interrupt-and-replace payload (mb.replacement) takes
	// precedence over a plain queued follow-up (mb.submitted). The
	// busy reservation itself stays claimed (Run's loop is about to
	// run another turn for the same sessionID) and is only released
	// by Run() once the loop has no more queued work.
	if isCancelErr {
		if next, ok := ts.a.getMailbox(ts.call.SessionID).DrainAfterCancel(); ok {
			ts.cancel()
			return nil, next, true, nil
		}
	}
	// err was already wrapped in ErrCallAlreadyAttempted above (if
	// userMessageCreated), so return it directly rather than wrapping
	// it a second time.
	return nil, SessionAgentCall{}, false, err
}

// hasToolResultAfterAssistantPosition only accepts a matching result from the
// issuing assistant message onward. Tool-call IDs can be reused across turns,
// so an older result with the same ID must not suppress recovery for this call.
func hasToolResultAfterAssistantPosition(msgs []message.Message, assistantID, toolCallID string) bool {
	start := -1
	for i := range msgs {
		if msgs[i].ID == assistantID {
			start = i
			break
		}
	}
	if start < 0 {
		return false
	}
	for _, msg := range msgs[start:] {
		if msg.Role != message.Tool {
			continue
		}
		for _, result := range msg.ToolResults() {
			if result.ToolCallID == toolCallID {
				return true
			}
		}
	}
	return false
}

// persistFailureFinish writes the error-path final finish. A question the
// agent asked is a reaction (it answered the notices visible in its prompt
// with real content), so its finish goes through the same one-transaction
// write as a normal step and marks the owner's delivered debt reacted; a
// failed mark falls back to the plain update so the finish still lands.
func (ts *turnStream) persistFailureFinish(ctx context.Context, snap message.Message, awaiting bool) error {
	if awaiting && hasReactionContent(snap) {
		var err error
		// The batch twin marks the batch debt (and clears its reacted_failed);
		// a failed mark still falls back to the plain update.
		if store := batchCutoverReactionStore(ts.a.coord); store != nil {
			err = store.BatchReactionMarkReactedWithMessageUpdate(ctx, ts.a.messages, ts.call.SessionID, snap)
		} else {
			err = ts.a.messages.Update(ctx, snap)
		}
		if err == nil {
			return nil
		}
		slog.Warn("agent: recording the question as a reaction failed; writing the finish alone",
			"session_id", ts.call.SessionID, "err", err)
	}
	return ts.a.messages.Update(ctx, snap)
}

// Helpers extracted from runTurn (agent_turn.go): provider-warning logging,
// mailbox-inject drain, stream-watchdog onFire handling, and turn-error
// normalization.
// stepToolCapabilities reports what this step's tool set allows: editing files
// and delegating to a sub-agent. Read from the set's own names, since a stale
// session prompt cannot make a tool appear (§4 of the in-turn plan).
func stepToolCapabilities(set []fantasy.AgentTool) (canEdit, canDelegate bool) {
	for _, tool := range set {
		switch tool.Info().Name {
		case tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName,
			tools.FSWriteToolName, tools.FSReplaceToolName, tools.FSWriteLinesToolName,
			tools.FSDeleteToolName:
			canEdit = true
		case modelkit.AgentToolName:
			canDelegate = true
		}
	}
	return canEdit, canDelegate
}

// logProviderWarnings emits each fantasy CallWarning from a step at WARN
// level. Without this, warnings such as malformed-tool-call input
// sanitization are silently dropped and never reach the logs. Optional
// fields (setting, tool, details) are attached only when present so the
// line stays terse for the common type+message case.
func logProviderWarnings(warnings []fantasy.CallWarning) {
	for _, w := range warnings {
		attrs := []any{"type", w.Type}
		if w.Message != "" {
			attrs = append(attrs, "message", w.Message)
		}
		if w.Setting != "" {
			attrs = append(attrs, "setting", w.Setting)
		}
		if w.Tool != nil && w.Tool.GetName() != "" {
			attrs = append(attrs, "tool", w.Tool.GetName())
		}
		if w.Details != "" {
			attrs = append(attrs, "details", w.Details)
		}
		slog.Warn("Provider warning", attrs...)
	}
}

func (a *sessionAgent) drainDueInjects(sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	return a.turnBudget.drainDueInjects(a, sessionID, genID, historyIDs)
}

// drainDueInjects is PrepareStep's mailbox-inject drain (design §5, stage
// 2.4), pulled out into a named method for the same reason handleWatchdogFire
// below was (task #243): a test must be able to drive the REAL production
// logic instead of a copy of it. The first version of this lived inline in
// the PrepareStep closure and its tests re-implemented the drain-and-dedup
// in a local helper — a mirror that passes whether or not production still
// matches it, which is exactly the shape #243 exists to stop.
//
// The rows are ALREADY in the DB (InjectMessage persisted them via
// createUserMessage), so callers only splice them into the current prompt —
// no second Create call.
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
func (t *turnBudget) drainDueInjects(h contextBudgetHost, sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	due := h.getMailbox(sessionID).DrainInjects(genID)
	return turnstep.DedupInjects(due, historyIDs,
		func(inj mailboxpkg.PendingInject) string { return inj.Msg.ID },
		func(inj mailboxpkg.PendingInject) message.Message { return inj.Msg },
	)
}

func (a *sessionAgent) handleWatchdogFire(
	cause watchdog.Cause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
) {
	a.turnBudget.handleWatchdogFire(a, cause, elapsed, sessionID, watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
}

// handleWatchdogFire is the EX-0H root route: the real fire-handler body
// lives in the turnstep leaf as turnstep.HandleWatchdogFire (no state
// instantiated there — everything is a parameter). This delegate forwards
// the cause/durations/model snapshot unchanged and passes h.timeoutHardCapCfg
// as a method value, so the config read still happens only in the leaf's
// watchdog.CauseHardCap branch, at the original point. See the leaf doc for the
// full invariant commentary (first synchronous Store, sync capture, async
// write, per-cause logs — all preserved verbatim).
func (t *turnBudget) handleWatchdogFire(
	h contextBudgetHost,
	cause watchdog.Cause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
) {
	turnstep.HandleWatchdogFire(
		cause, elapsed, sessionID, watchdogCauseVal,
		toolMaxDuration, idleTimeout,
		agentcall.Model(smartModel), // Model = agentcall.Model alias
		h.timeoutHardCapCfg,
	)
}

// normalizeTurnError folds the out-of-band way a turn can end (the
// ask-question tool error) into the single error every downstream consumer
// reads, so nothing past this point has to know either shape.
//
// Extracted from runTurn — one of only two blocks in that function that
// cross no boundary worth worrying about: no defer, no early return, no
// closure created, no lock held, two inputs and one output. See the
// "runTurn is not being decomposed" commit for why the rest of runTurn
// stays where it is.
func normalizeTurnError(err error) error {
	// The ask_question tool reports "agent asked a question" as the Go
	// error its Run() returns; fantasy's executeSingleTool treats a
	// non-nil tool error as critical and propagates it as the whole
	// Stream() call's error, so it surfaces here exactly like any other
	// stream error. tools.AskQuestionError
	// (package tools) exists only because package tools cannot import
	// this package back (this package already imports tools — see the
	// comment on AskQuestionError in ask_question.go for the full import
	// cycle rationale); normalize it into AwaitingAnswerError here so
	// every downstream consumer — the errors.As(err, &awaitingErr) branch
	// at the call site, RunNonInteractive's exit_reason mapping, sessions
	// why/diff, … — only ever has to know about the one agent-level type.
	var askErr *tools.AskQuestionError
	if errors.As(err, &askErr) {
		err = &AwaitingAnswerError{
			Question:  askErr.Question,
			Options:   askErr.Options,
			SessionID: askErr.SessionID,
		}
	}
	return err
}

func asyncLaunchCommand(call fantasy.ToolCallContent) (string, bool) {
	var command string
	switch call.ToolName {
	case tools.BashToolName:
		var params tools.BashParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = params.Command
	case tools.RunCommandToolName:
		var params tools.RunCommandParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = (tools.RunCommandPermissionsParams{Program: params.Program, Args: params.Args}).RunAllowlistCommand()
	default:
		return "", false
	}
	return strings.TrimSpace(command), true
}
