// The ExecuteRun event loop as a reusable single-turn phase, plus the
// reviewer pass that re-enters that phase on the Reviewer model after a
// clean --role smart run. The phase body (runTurnPhase and the
// handleMessageEvent/drainMessageEvents/finish methods) is a verbatim
// extraction from app_run.go's ExecuteRun — same order, same comments,
// with the per-invocation state it read as closures moved onto
// executeRunLoop — so a run that does not hit the reviewer-pass gate
// behaves exactly as before the split.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// reviewerPassPrompt is the fixed user prompt of the automatic reviewer
// pass. It must always steer the reviewer toward ending with a conclusion
// and never toward asking a question or proposing more work: an
// ask_question tool call here would force-finish the turn with
// awaiting_answer, which nothing is watching to answer.
const reviewerPassPrompt = `You are now acting as the independent reviewer for this session.
Review everything that happened above: what was asked, what was delegated,
what was actually done (tool calls, diffs, test results), and the
orchestrator's own final answer. Then give your own concluding assessment
as the final message of this session: what was actually accomplished, any
gaps, risks, or concerns you found, and your overall verdict. This is the
session's final message — do not ask questions and do not propose further
work; conclude.`

// turnRunFunc is the shape of the per-turn runner the phase loop hands to
// its turn goroutine: the coordinator Run/RunWithOverrides/
// RunWithCredentials dispatch for the primary turn, or the reviewer pass's
// fixed RunWithOverrides call.
type turnRunFunc func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error)

// executeRunLoop carries the state ExecuteRun's event loop reads, split
// into per-invocation fields (identical across both phases) and
// per-phase tracking fields (reset by resetForReviewerPass). The struct
// exists so the select loop can run twice in one ExecuteRun call — once
// for the primary turn, once for the review turn — with the review
// turn's finish() result becoming ExecuteRun's own return value.
type executeRunLoop struct {
	// Immutable for the whole invocation (both phases).
	app            *App
	sess           session.Session
	ctx            context.Context
	mode           RunMode
	overrides      RunOverrides
	stdout, stderr io.Writer
	stderrTTY      bool
	progress       bool
	stopSpinner    func()
	runStart       time.Time
	tokensBefore   int64
	costBefore     float64
	// hookExitReason points at ExecuteRun's local variable: finish()
	// writes it and ExecuteRun's ended_reason/on-finish-hook defers read
	// it after both phases are done, so the review turn's outcome wins by
	// running last.
	hookExitReason *string

	// Per-phase tracking state. resetForReviewerPass zeroes these (plus a
	// fresh baselineIDs snapshot) between the primary and review turns.
	// canceledAfterCommit and the invocationToolCalls/invocationToolCallIDs
	// inventory are excluded: the first is read after the reset, the second
	// must span both phases (F8).
	baselineIDs      map[string]struct{}
	baselineKnown    bool
	messageEvents    <-chan pubsub.Event[message.Message]
	messageReadBytes map[string]int
	seenToolCalls    map[string]bool
	toolCallCounts   map[string]int
	finalText        string
	finalReason      string
	finalErrTitle    string
	finalErrDetails  string
	printed          bool
	// reconciliationDiagnostic is produced/consumed inside finish only.
	reconciliationDiagnostic string

	// canceledAfterCommit records that finish() treated a committed
	// success as the phase's outcome even though the parent context was
	// already canceled (the authoritativeTerminal suppression in
	// finish). The reviewer gate must not start a new phase for a run
	// whose parent canceled it, no matter how clean the committed
	// primary looks (R2-4, 2026-09-22 audit).
	canceledAfterCommit bool

	// invocationToolCalls is the run-wide tool-call inventory: it spans
	// BOTH phases (primary turn + review turn) and is deliberately NOT
	// reset by resetForReviewerPass, so cost/duration/tool_calls keep
	// covering the whole invocation (F8, 2026-09-22 audit).
	// invocationToolCallIDs deduplicates by tool-call ID across phases.
	invocationToolCalls   map[string]int
	invocationToolCallIDs map[string]struct{}

	cachedTerminal       *terminalReconciliation
	cachedTerminalCtx    context.Context
	cachedTerminalCancel context.CancelFunc

	// callResultRec is this phase's call-result recorder (R8-3): armed
	// fresh at the top of runTurnPhase, carried on the ctx the turn
	// goroutine runs under, and read back by both reconcileTerminalMessage
	// call sites to scope reconciliation to THIS phase's own runInternal
	// invocation instead of any newer, not-in-baseline row.
	callResultRec *agent.CallResultRecorder
}

// runTurnPhase runs one "snapshot baseline → subscribe (if needed) →
// startTurn → select-loop → finish" cycle — the whole body ExecuteRun
// used to inline — and returns its finish() result. Extracted verbatim so
// ExecuteRun can run it twice: once for the primary turn and, when the
// reviewer pass fires, once more for the review turn.
func (s *executeRunLoop) runTurnPhase(prompt string, runFn turnRunFunc) (*RunResult, error) {
	done := make(chan agentTurnResponse, 1)
	// R8-3: a fresh recorder per phase -- the primary turn and the review
	// turn are separate runInternal invocations, each with its own result
	// identity.
	s.callResultRec = agent.NewCallResultRecorder()
	startTurn := func() {
		if executeRunBeforeTurnLaunchSeam != nil {
			executeRunBeforeTurnLaunchSeam()
		}
		go runAgentTurnRecovered(agent.WithCallResultRecorder(s.ctx, s.callResultRec), s.sess.ID, prompt, runFn, done)
	}
	// Snapshot the pre-turn messages and subscribe before launching the
	// turn. The message broker is live-only; a fast provider can publish
	// and finish before a later subscriber exists. The second (reviewer)
	// phase re-snapshots the baseline via resetForReviewerPass and reuses
	// the first phase's subscription: it is still open — the run's ctx has
	// not been canceled between phases — and the re-fenced baselineIDs
	// mean only the review turn's own messages read as new.
	s.baselineIDs = make(map[string]struct{})
	s.baselineKnown = true
	if existing, listErr := s.app.Messages.List(s.ctx, s.sess.ID); listErr != nil {
		s.baselineKnown = false
		slog.Warn("run: failed to snapshot pre-run messages; terminal reconciliation will use live events", "session", s.sess.ID, "err", listErr)
	} else {
		for _, msg := range existing {
			s.baselineIDs[msg.ID] = struct{}{}
		}
	}
	if s.messageEvents == nil {
		s.messageEvents = s.app.Messages.Subscribe(s.ctx)
	}
	// The old inline code created these maps before the select loop ran;
	// they must exist before the turn goroutine is launched.
	s.messageReadBytes = make(map[string]int)
	s.seenToolCalls = make(map[string]bool)
	s.toolCallCounts = make(map[string]int)
	startTurn()
	drainDone := make(chan error, 1)

	for {
		if s.progress && s.stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(s.stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			if executeRunDoneCaseSeam != nil {
				executeRunDoneCaseSeam()
			}
			if err := s.drainMessageEvents(); err != nil {
				return nil, err
			}
			if result.queued {
				return s.finish(&runQueuedError{sessionID: s.sess.ID})
			}
			runErr := result.err
			isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

			// P0-1 fix (task #421): a cross-process interrupt landing on a
			// busy session (rush sessions inject --interrupt) cancels the
			// in-flight generation and durably enqueues its replacement
			// (handleInterruptTick), deliberately WITHOUT a live mailbox
			// handoff — the durable run_queue row is the only remaining
			// owner (see mailbox.go's FromDurableQueue guard). Without this,
			// that row sits pending until the background RunQueuePump's
			// next tick (3s in production) happens to fire before this
			// process exits — a race this short-lived process routinely
			// loses, since the rest of this select fires within
			// milliseconds of the cancellation. DrainSessionNow runs any
			// such pending continuation to completion, in THIS process,
			// before the envelope is built from what would otherwise be a
			// stale, cancelled-generation result.
			//
			// isCanceled gates this deliberately: DrainSessionNow itself is
			// a no-op (DrainNoWork) when nothing is pending, so a plain
			// user/--timeout cancellation with no durable continuation is
			// unaffected — the drainDone case below restores the ORIGINAL
			// runErr in that case, rather than fabricating a success.
			//
			// Runs in its OWN goroutine (see drainDone's doc above for why
			// synchronous-in-place doesn't work) — this select loop keeps
			// servicing messageEvents (and ctx.Done()) the whole time.
			if isCanceled && s.app.RunQueuePump != nil {
				go func(originalErr error) {
					result, drainErr := s.app.RunQueuePump.DrainSessionNow(s.ctx, s.sess.ID)
					drainDone <- drainOutcomeError(s.sess.ID, result, drainErr, originalErr)
				}(runErr)
				continue
			}

			return s.finish(runErr)

		case drainErr := <-drainDone:
			// R8-3's ownID scoping only covers a message THIS phase's own
			// runInternal invocation produced. A drained continuation
			// (DrainSessionNow, task #421/P0-1) executes through the
			// durable queue pump's own separate ctx chain, so it never
			// reports into s.callResultRec -- the ID captured there
			// belongs to the ORIGINAL, now-superseded attempt. Reset to a
			// fresh (empty) recorder so reconciliation falls back to its
			// pre-R8-3 baseline-only scan, exactly what the original P0-1
			// fix relies on to find the drained continuation's own
			// committed row.
			s.callResultRec = agent.NewCallResultRecorder()
			return s.finish(drainErr)

		case event, ok := <-s.messageEvents:
			if !ok {
				s.messageEvents = nil
				if messageEventsClosedSeam != nil {
					messageEventsClosedSeam()
				}
				continue
			}
			if err := s.handleMessageEvent(event); err != nil {
				return nil, err
			}
		case <-s.ctx.Done():
			probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(s.ctx), cleanupTimeout)
			reconciled, reconcileErr := s.app.reconcileTerminalMessage(probeCtx, s.sess.ID, s.baselineIDs, s.baselineKnown, s.runStart, s.callResultRec.Resolve())
			if reconcileErr == nil {
				s.cachedTerminal = &reconciled
				s.cachedTerminalCtx = probeCtx
				s.cachedTerminalCancel = probeCancel
				return s.finish(s.ctx.Err())
			}
			probeCancel()
			// Cancellation and the buffered turn result can become ready in
			// either order. Prefer the committed result when it is already
			// available so final reconciliation still runs.
			select {
			case result := <-done:
				if err := s.drainMessageEvents(); err != nil {
					return nil, err
				}
				if result.queued {
					return s.finish(&runQueuedError{sessionID: s.sess.ID})
				}
				if result.err != nil && (errors.Is(result.err, context.Canceled) || errors.Is(result.err, agent.ErrRequestCancelled)) && s.app.RunQueuePump != nil {
					go func(originalErr error) {
						queuedResult, drainErr := s.app.RunQueuePump.DrainSessionNow(s.ctx, s.sess.ID)
						drainDone <- drainOutcomeError(s.sess.ID, queuedResult, drainErr, originalErr)
					}(result.err)
					continue
				}
				return s.finish(result.err)
			default:
				s.stopSpinner()
				*s.hookExitReason = "cancelled"
				return nil, s.ctx.Err()
			}
		}
	}
}

// handleMessageEvent folds one published assistant message into the
// per-phase tracking state and the streaming output modes.
func (s *executeRunLoop) handleMessageEvent(event pubsub.Event[message.Message]) error {
	msg := event.Payload
	if msg.SessionID != s.sess.ID || msg.Role != message.Assistant || len(msg.Parts) == 0 {
		return nil
	}
	s.stopSpinner()
	// Tool-call names always go to stderr - one short line per new call.
	for _, p := range msg.Parts {
		if tc, ok := p.(message.ToolCall); ok && tc.Name != "" && !s.seenToolCalls[tc.ID] {
			s.seenToolCalls[tc.ID] = true
			s.toolCallCounts[tc.Name]++
			prefix := ""
			if s.stderrTTY {
				prefix = "\r" + ansi.EraseEntireLine
			}
			fmt.Fprintf(s.stderr, prefix+"▶ %s\n", tc.Name)
		}
	}

	// Live events drive progress and streaming. The persisted row below
	// is authoritative for the terminal envelope.
	if msg.IsFinished() {
		s.finalText = msg.FullText()
		for _, p := range msg.Parts {
			if f, ok := p.(message.Finish); ok {
				s.finalReason = string(f.Reason)
				s.finalErrTitle = f.Message
				s.finalErrDetails = f.Details
				break
			}
		}
	}

	switch s.mode {
	case RunModeJSON:
		// Suppress per-message stdout; the summary is printed below.
	case RunModeTerse:
		// R5-3 (2026-09-22 audit): nothing is published per message.
		// Publishing here put the primary's text on stdout before the
		// reviewer gate decided, so an auto-reviewed run emitted
		// "PRIMARYREVIEW\n" concatenated. flushTerseOutput prints the
		// single selected final result once, after the gate.
	case RunModeStream:
		content := msg.FullText()
		readBytes := s.messageReadBytes[msg.ID]
		if len(content) < readBytes {
			slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
			return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
		}
		part := content[readBytes:]
		if readBytes == 0 {
			part = strings.TrimLeft(part, " \t")
		}
		if s.printed || strings.TrimSpace(part) != "" {
			s.printed = true
			fmt.Fprint(s.stdout, part)
		}
		s.messageReadBytes[msg.ID] = len(content)
	}
	return nil
}

func (s *executeRunLoop) drainMessageEvents() error {
	for s.messageEvents != nil {
		select {
		case event, ok := <-s.messageEvents:
			if !ok {
				s.messageEvents = nil
				if messageEventsClosedSeam != nil {
					messageEventsClosedSeam()
				}
				continue
			}
			if err := s.handleMessageEvent(event); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

// mergeReconciledToolCalls folds one phase's reconciled tool inventory
// into the run-wide accounting (F8, 2026-09-22 audit). finish used to
// REPLACE toolCallCounts with the current phase's reconciled counts, so
// after resetForReviewerPass zeroed them the final envelope's tool_calls
// covered the review turn alone and the primary phase's calls
// disappeared — including from the sub-agent reduction warning. Calls
// with an ID are deduplicated across phases; ID-less calls cannot be
// keyed and are added as reported.
func (s *executeRunLoop) mergeReconciledToolCalls(reconciled terminalReconciliation) {
	s.ensureInvocationToolInventory()
	for id, name := range reconciled.toolCallByID {
		if _, dup := s.invocationToolCallIDs[id]; dup {
			continue
		}
		s.invocationToolCallIDs[id] = struct{}{}
		s.invocationToolCalls[name]++
	}
	for name, count := range reconciled.toolCallsNoID {
		s.invocationToolCalls[name] += count
	}
	s.toolCallCounts = s.invocationToolCalls
}

// foldLiveToolCallCounts is the reconcile-failure fallback: the phase's
// live-event counts (already ID-deduplicated within the phase by
// handleMessageEvent) join the run-wide inventory so a failed lookup in
// one phase does not erase the other phase's calls. Aggregated counts
// cannot be ID-deduplicated across phases; that residual belongs to the
// degraded path the reconciliation diagnostic already warns about.
func (s *executeRunLoop) foldLiveToolCallCounts() {
	if len(s.toolCallCounts) == 0 {
		return
	}
	s.ensureInvocationToolInventory()
	for name, count := range s.toolCallCounts {
		s.invocationToolCalls[name] += count
	}
	s.toolCallCounts = s.invocationToolCalls
}

func (s *executeRunLoop) ensureInvocationToolInventory() {
	if s.invocationToolCalls == nil {
		s.invocationToolCalls = make(map[string]int)
	}
	if s.invocationToolCallIDs == nil {
		s.invocationToolCallIDs = make(map[string]struct{})
	}
}

// flushTerseOutput prints the run's buffered terse output exactly once,
// after ExecuteRun has selected the single final result: the primary's
// own for a run without the reviewer pass, the review turn's conclusion
// otherwise. Publishing per finished message instead put the primary's
// text on stdout before the gate decided, so an auto-reviewed run
// emitted "PRIMARYREVIEW\n" concatenated (R5-3, 2026-09-22 audit).
// finalText is the selected final text — authoritative reconciliation,
// continuation-chain combination and the queued-outcome clear have all
// already been applied by finish().
func (s *executeRunLoop) flushTerseOutput() {
	if s.mode != RunModeTerse {
		return
	}
	fmt.Fprint(s.stdout, strings.TrimLeft(s.finalText, " \t\n"))
}

// finish builds the final envelope/error from runErr plus whatever
// finalText/finalReason/toolCallCounts have accumulated via messageEvents
// so far, and is the sole return point for a completed run. Extracted
// (task #421/P0-1) from the body of `case result := <-done:` so BOTH that
// case AND drainDone's case (a durable continuation's outcome, possibly
// arriving well after the original done fired) can reach it — see the
// select loop's own doc for why this split exists.
func (s *executeRunLoop) finish(runErr error) (*RunResult, error) {
	s.stopSpinner()
	if errors.Is(runErr, ErrRunQueued) {
		// The shared session stream may have delivered the active owner's
		// messages before the mailbox reported this call as queued. Do not
		// attribute that output or its tool calls to the queued prompt.
		s.finalText = ""
		s.finalReason = ""
		s.finalErrTitle = ""
		s.finalErrDetails = ""
		s.toolCallCounts = make(map[string]int)
	}
	isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, agent.ErrRequestCancelled))
	finalCtx := s.cachedTerminalCtx
	finalCancel := s.cachedTerminalCancel
	if finalCtx == nil {
		finalCtx, finalCancel = context.WithTimeout(context.WithoutCancel(s.ctx), cleanupTimeout)
	}
	defer finalCancel()
	if !errors.Is(runErr, ErrRunQueued) {
		var reconciled terminalReconciliation
		var reconcileErr error
		authoritativeTerminal := false
		if s.cachedTerminal != nil {
			reconciled = *s.cachedTerminal
			authoritativeTerminal = true
		} else {
			reconciled, reconcileErr = s.app.reconcileTerminalMessage(finalCtx, s.sess.ID, s.baselineIDs, s.baselineKnown, s.runStart, s.callResultRec.Resolve())
		}
		if reconcileErr != nil {
			s.reconciliationDiagnostic = "authoritative terminal message reconciliation failed: " + reconcileErr.Error() + "; using live run events"
			slog.Warn("run: failed to reconcile authoritative terminal message", "session", s.sess.ID, "err", reconcileErr)
			s.foldLiveToolCallCounts()
		} else {
			// Replay the committed row through the normal output handler.
			// It emits only unread content, so a dropped terminal event cannot
			// lose output or duplicate it.
			if outputErr := s.handleMessageEvent(pubsub.Event[message.Message]{Payload: reconciled.message}); outputErr != nil {
				return nil, outputErr
			}
			s.mergeReconciledToolCalls(reconciled)
			// F3 (2026-09-21 weekly audit): a successful continuation chain
			// leaves each attempt's partial text in its own history row and
			// the reconciled terminal message alone carries only the tail.
			// combinedText is the full chain text; it is empty unless the
			// terminal message resumed such a chain, so ordinary runs keep
			// the terminal row's own text.
			if reconciled.combinedText != "" {
				s.finalText = reconciled.combinedText
			}
			authoritativeTerminal = true
		}
		if authoritativeTerminal && isCanceled && !runFailed(s.finalReason, nil, false) {
			if s.finalReason == string(message.FinishReasonEndTurn) {
				// R2-4: the parent canceled after this turn had already
				// committed a successful terminal message. The committed
				// success is still this phase's outcome, but it must not
				// look clean to the reviewer gate: record the cancel so
				// no new phase starts for a canceled run.
				s.canceledAfterCommit = true
				runErr = nil
				isCanceled = false
			} else {
				// R8-2 (2026-09-22 audit): the reconciled row is a
				// non-terminal Finish (tool_use in the proven scenario) --
				// it proves only that ONE intermediate step committed, not
				// that the logical call itself completed. A tool_use step
				// is always followed by a further step, and that step's own
				// outcome (success, error, or the cancellation itself) is
				// exactly what got missed. Do not let an intermediate
				// step's Finish silence a real cancellation; clear the
				// stale mid-step label so the envelope reports the actual
				// outcome (buildRunResult falls back to "canceled" for an
				// empty reason) instead of a misleading "tool_use" success.
				s.finalReason = ""
			}
		}
	}

	if s.mode == RunModeJSON {
		// Re-fetch the session row so the usage delta reflects
		// the writes the agent made during the run.
		freshSess, usageErr := s.app.Sessions.Get(finalCtx, s.sess.ID)
		deltaTokens := int64(0)
		deltaCost := float64(0)
		if usageErr != nil {
			slog.Warn("run: failed to read session usage for the JSON envelope; reporting zero deltas", "session", s.sess.ID, "err", usageErr)
		} else {
			deltaTokens = freshSess.PromptTokens + freshSess.CompletionTokens - s.tokensBefore
			deltaCost = freshSess.Cost - s.costBefore
			if deltaTokens < 0 {
				slog.Warn("run: session token usage moved backwards; reporting zero delta", "session", s.sess.ID, "before", s.tokensBefore, "after", freshSess.PromptTokens+freshSess.CompletionTokens)
				deltaTokens = 0
			}
			if deltaCost < 0 {
				slog.Warn("run: session cost moved backwards; reporting zero delta", "session", s.sess.ID, "before", s.costBefore, "after", freshSess.Cost)
				deltaCost = 0
			}
		}
		// Fork patch (orchestrator UX): when the caller asked
		// for JSON, defang the persistent "model wrapped its
		// final JSON in a ```json fence and added prose" case
		// here so wrappers can pipe final_text straight into
		// jq. The original is preserved in assistant_notes.
		//
		// Fork patch (orchestrator UX): stripAndExtractJSON handles
		// the common fast-model failure mode: prose preamble + JSON,
		// or even multiple JSON values separated by prose (observed
		// with GLM-5-turbo). Returns a wrapped JSON array when N≥2
		// valid values are found, a single value for N=1, and
		// ErrInvalidStripJSON for N=0 (original text preserved in
		// final_text so the orchestrator can inspect what the model
		// actually said).
		finalTextOut := s.finalText
		assistantNotes := ""
		strippedBytes := 0
		stripErr := ""
		stripErrReason := ""
		if s.overrides.StripJSONFences && s.finalReason != "error" && s.finalReason != "canceled" {
			cleaned, notes, vErr := stripAndExtractJSON(s.finalText)
			finalTextOut = cleaned
			assistantNotes = notes
			strippedBytes = len(s.finalText) - len(cleaned)
			if strippedBytes < 0 {
				strippedBytes = 0
			}
			if vErr != nil {
				stripErr = vErr.Error()
				stripErrReason = "invalid_json"
			}
		}
		// Fork patch (orchestrator UX): sub-agent aggregation.
		// session-#3 (2026-05-17) feedback measured a 7×
		// reduction where parent collapsed sub-agent outputs
		// into a one-paragraph wrap-up. Two responses:
		//
		// 1. ALWAYS-ON warning when reduction ratio is bad
		//    (≥3 sub-agents emitted output AND final_text is
		//    <40% of their combined chars). Operator sees it
		//    in envelope.warnings without flipping a flag.
		// 2. OPT-IN --aggregation=attach: collect each
		//    sub-agent's last assistant text into
		//    envelope.SubAgentOutputs so the orchestrator
		//    recovers the lost detail.
		var subOutputs []SubAgentOutput
		var reductionWarning string
		subAgentCalls := s.toolCallCounts["agent"] + s.toolCallCounts["agentic_fetch"]
		if subAgentCalls > 0 {
			count, totalChars := s.app.subAgentSummaryStats(finalCtx, s.sess.ID)
			if count >= 2 && totalChars > 0 {
				parentChars := len(finalTextOut)
				ratio := float64(parentChars) / float64(totalChars)
				if ratio < 0.4 {
					reductionWarning = fmt.Sprintf(
						"reduction-loss: final_text is %d chars (%.0f%% of %d combined sub-agent chars across %d sub-session(s)). The parent likely summarised away detail. Re-run with --aggregation=attach or --aggregation=concat to recover; or query the sub-sessions directly.",
						parentChars, ratio*100, totalChars, count,
					)
				}
			}
		}
		if s.overrides.AggregationMode == "attach" {
			subOutputs = s.app.collectSubAgentOutputs(finalCtx, s.sess.ID)
		}
		summary := buildRunResult(
			s.sess.ID, finalTextOut, assistantNotes, s.finalReason, runErr, isCanceled,
			s.toolCallCounts,
			deltaTokens,
			deltaCost,
			time.Since(s.runStart),
			s.finalErrTitle, s.finalErrDetails,
			strippedBytes, stripErr, stripErrReason,
			subOutputs, reductionWarning,
		)
		if s.reconciliationDiagnostic != "" {
			summary.Warnings = append(summary.Warnings, s.reconciliationDiagnostic)
		}
		// Per-message token/cache accounting for the session (task
		// #480). Best-effort: an orchestrator losing statistics must
		// never turn a successful run into a failed one.
		if report, uErr := s.app.Messages.UsageBySession(finalCtx, s.sess.ID); uErr != nil {
			slog.Warn("run: failed to read per-message usage for the JSON envelope", "session", s.sess.ID, "err", uErr)
		} else {
			summary.Usage.Session = buildSessionUsageInfo(report)
		}
		// Fork patch: batch 8 — surface orphan partial text.
		if partial := s.app.findOrphanPartial(finalCtx, s.sess.ID); partial != nil {
			summary.RecoveredPartial = partial
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"recovered %d chars of partial assistant text from session %s — model run was interrupted",
				partial.Chars, s.sess.ID,
			))
		}
		*s.hookExitReason = summary.ExitReason
		if runFailed(s.finalReason, runErr, isCanceled) {
			return &summary, &runIncompleteError{reason: summary.ExitReason, detail: summary.Error, cause: runErr}
		}
		return &summary, nil
	}

	if runErr != nil {
		if guidance := sessionBusyGuidance(s.sess.ID, runErr); guidance != "" {
			slog.Warn("Non-interactive run rejected because session is already locked",
				"session_id", s.sess.ID,
				"guidance", guidance,
				"err", runErr)
			fmt.Fprintf(s.stderr, "\n%s\n\n", guidance)
		}
		// Peak-hours refusal carries multiline orchestrator
		// guidance (RESUME AT + don't-retry instructions) that
		// fang's ERROR box truncates at the first newline. Print
		// the guidance to stderr separately BEFORE the ERROR box
		// so the operator / orchestrator actually sees it.
		// Reuses agent.PeakHoursGuidance so the stderr text stays
		// identical to the DB finish-message details recorded by
		// peakHoursStoppedFinishText (sessions why / diff, etc.).
		var peakErr *agent.PeakHoursError
		if errors.As(runErr, &peakErr) {
			fmt.Fprintf(s.stderr, "\n%s\n\n", agent.PeakHoursGuidance(peakErr))
		}
		if isCanceled {
			slog.Debug("Non-interactive: agent processing cancelled", "session_id", s.sess.ID)
			*s.hookExitReason = "cancelled"
			return nil, cancelledRunError(runErr, s.finalReason, s.finalErrTitle, s.finalErrDetails)
		}
		*s.hookExitReason = "error"
		return nil, fmt.Errorf("agent processing failed: %w", runErr)
	}
	// runErr == nil, but the turn may still have ended in-band on an
	// error / canceled / max_tokens finish — not a clean completion,
	// so exit non-zero (the final text is already on stdout).
	if runFailed(s.finalReason, runErr, isCanceled) {
		reason := s.finalReason
		if reason == "" {
			reason = "error"
		}
		*s.hookExitReason = reason
		detail := s.finalErrTitle
		if s.finalErrDetails != "" {
			if detail != "" {
				detail += ": "
			}
			detail += s.finalErrDetails
		}
		return nil, &runIncompleteError{reason: reason, detail: detail}
	}
	*s.hookExitReason = "stop"
	return nil, nil
}

// resetForReviewerPass re-arms the per-phase tracking state for the
// review turn: the tracking maps and final-envelope strings go back to
// zero values, and baselineIDs is re-snapshotted from a FRESH
// Messages.List so reconcileTerminalMessage treats the review turn's own
// assistant message as the run's new terminal one (the primary turn's
// messages are all in the baseline now).
//
// ctx IS the review turn's context (buildReviewerPassTurn's reviewCtx,
// carrying ModelRole=reviewer / DisableSubAgents=true and cleared model
// persistence plus reserved ownership), and it must also become s.ctx:
// runTurnPhase launches its turn with `go runAgentTurnRecovered(s.ctx,
// ...)`, so without this assignment the review turn would execute under
// the PRIMARY phase's context — keeping the orchestrator's smart-role
// CallOptions (worker-delegation toolset) and the stale reserved-
// ownership token instead of the reviewer's isolation.
//
// The reassignment is race-free: runTurnPhase is synchronous and has
// already returned when this runs (the caller reaches the reviewer gate
// only after the primary runTurnPhase returned), and the turn goroutine
// reads s.ctx exactly once, at `go` statement evaluation time — every
// later read of s.ctx happens on this same goroutine, strictly after.
// The primary phase's message subscription (created under the old ctx)
// stays valid: it is canceled only by ExecuteRun's own defer, and the
// run ctx is an ancestor of reviewCtx.
//
// runStart/tokensBefore/costBefore are deliberately NOT reset: the final
// envelope's cost/token/duration numbers must cover the WHOLE invocation
// (primary turn + review turn combined), not just the review phase.
func (s *executeRunLoop) resetForReviewerPass(ctx context.Context) {
	s.ctx = ctx
	// R2-4: the cached terminal reconciliation belongs to the phase
	// that built it. Its probe context derives from that phase's own
	// ctx, and finish()'s defer has already fired its cancel, so a
	// triple carried across the phase boundary would make the next
	// finish() mistake the previous phase's terminal message for this
	// one's and read usage off a dead context. Release and drop all
	// three as one unit.
	if s.cachedTerminalCancel != nil {
		s.cachedTerminalCancel()
	}
	s.cachedTerminal = nil
	s.cachedTerminalCtx = nil
	s.cachedTerminalCancel = nil
	s.finalText = ""
	s.finalReason = ""
	s.finalErrTitle = ""
	s.finalErrDetails = ""
	s.toolCallCounts = make(map[string]int)
	s.seenToolCalls = make(map[string]bool)
	// invocationToolCalls/invocationToolCallIDs deliberately survive the
	// reset: the final envelope's tool_calls and the sub-agent reduction
	// warning must span both phases (F8, 2026-09-22 audit).
	s.messageReadBytes = make(map[string]int)
	s.reconciliationDiagnostic = ""
	s.baselineIDs = make(map[string]struct{})
	s.baselineKnown = true
	if existing, listErr := s.app.Messages.List(ctx, s.sess.ID); listErr != nil {
		s.baselineKnown = false
		slog.Warn("reviewer pass: failed to snapshot pre-review messages; terminal reconciliation will use live events", "session", s.sess.ID, "err", listErr)
	} else {
		for _, msg := range existing {
			s.baselineIDs[msg.ID] = struct{}{}
		}
	}
}

// buildReviewerPassTurn assembles the review turn: the CallOptions of a
// plain `rush run --role reviewer` invocation (reviewer model role,
// sub-agents disabled — the same default an explicit --role reviewer gets
// when --agents is unset) with the original call's budget/scope/timeout
// constraints carried over so the review turn cannot escape the run's
// declared cost/token/timeout ceiling, plus the runFn that invokes
// RunWithOverrides with the configured Reviewer model. The fast override
// stays nil: RunWithOverrides inherits the session's recorded fast slot
// by itself.
//
// No further toolset restriction: ModelRole=reviewer does not trigger the
// orchestrator's edit/multiedit/write stripping (that fires only for
// ModelRole=smart with a worker configured, see workerSubAgentActiveForCall
// in coordinator_tools.go), so the review turn gets exactly the same
// read+write+bash toolset a manual --role reviewer invocation gets.
//
// The returned context is the run ctx with the review CallOptions
// attached (shadowing the primary call's), and with two inherited values
// deliberately cleared:
//   - WithSessionModelPersistence: the review is a one-off — a later
//     `rush run --session <same-id>` must still resolve the session's
//     normal smart model, so the reviewer override must never persist.
//   - the reserved-ownership era token (fail-fast callers): the primary
//     turn consumed or released that era, and the token is one-shot; an
//     unclaimed leftover must not be stale-claimed into a dead epoch by
//     the review turn — it is cleared so the review turn re-claims like
//     any fresh call. R2-3: a fail-fast caller keeps that contract on
//     the review turn too — FailIfSessionBusy is carried over from the
//     primary options, so if another caller claims the session between
//     the primary phase ending and the review turn being admitted, the
//     mailbox's atomic submit check refuses the review call outright
//     instead of queueing it behind the other owner for later execution,
//     and ExecuteRun fails fast with an error wrapping
//     agent.ErrSessionBusy rather than returning ErrRunQueued.
func (app *App) buildReviewerPassTurn(ctx context.Context, primary *agent.CallOptions) (turnRunFunc, context.Context) {
	reviewerCfg := app.config.Config().Models[config.SelectedModelTypeReviewer]
	reviewerOverride := &agent.ModelOverride{
		Provider:        reviewerCfg.Provider,
		Model:           reviewerCfg.Model,
		ReasoningEffort: reviewerCfg.ReasoningEffort,
	}
	reviewCallOpts := &agent.CallOptions{
		ModelRole:                config.SelectedModelTypeReviewer,
		DisableSubAgents:         true,
		TimeoutExtendsOnProgress: primary.TimeoutExtendsOnProgress,
		TimeoutHardCap:           primary.TimeoutHardCap,
		TimeoutOptionsSet:        primary.TimeoutOptionsSet,
		IdleTimeout:              primary.IdleTimeout,
		MaxCost:                  primary.MaxCost,
		MaxTokens:                primary.MaxTokens,
		AllowPeakHours:           primary.AllowPeakHours,
		// R2-3: honor the caller's fail-fast busy policy on the review
		// turn too. The decision itself stays at the mailbox reservation
		// (sessionAgent.Run -> mailbox.submit), which is atomic under
		// mb.mu: a competing claim between the phases is rejected for
		// this call with nothing left in the other owner's queue.
		FailIfSessionBusy: primary.FailIfSessionBusy,
		FolderScope:       primary.FolderScope,
		DiskProvider:      primary.DiskProvider,
	}
	reviewCtx := agent.WithCallOptions(ctx, reviewCallOpts)
	// R5-1: carry the temporary reviewer override on the review turn's
	// ctx so the 401 rebuild path (coordinator.resolveCallModels)
	// re-applies the SAME override instead of silently resetting the
	// model identity to the session/config smart slot after a
	// credential refresh. Persistence stays cleared below, so the
	// override still never lands in the durable session slots.
	reviewCtx = agent.WithModelOverrides(reviewCtx, reviewerOverride, nil)
	reviewCtx = agent.WithSessionModelPersistence(reviewCtx, nil, nil)
	reviewCtx = agent.ClearReservedOwnership(reviewCtx)
	reviewRunFn := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return app.AgentCoordinator.RunWithOverrides(ctx, sessionID, prompt, reviewerOverride, nil)
	}
	return reviewRunFn, reviewCtx
}
