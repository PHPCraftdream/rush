package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	batchsession "github.com/PHPCraftdream/rush/internal/session"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// cliOpenWorkNamed bounds how many running rows one heartbeat line names.
const cliOpenWorkNamed = 3

// describeOpenWork names what the wait heartbeat is waiting on (ASYNC-10: any
// "why is it waiting" answer names the concrete task): the session's own
// running rows -- tool, tool_call_id, the delegated child and the host that
// runs it (this process, or another one with its pid and liveness). A read
// failure or an empty answer (the row finished a moment ago) falls back to
// the generic wording; it is display only.
func (l *cliLoop) describeOpenWork() string {
	const generic = "a running job/delegation, a host that is not provably dead, or an open wake schedule"
	// Stage 5a: the once schedule the run is held open for (empty without).
	var schedule []string
	if clause := l.describeOnceWake(); clause != "" {
		schedule = append(schedule, clause)
	}
	if l.host == nil {
		if len(schedule) > 0 {
			return strings.Join(schedule, "; ")
		}
		return generic
	}
	return l.describeOpenWorkBatch(schedule, generic)
}

// describeOpenWorkBatch renders the wait heartbeat's open-work answer: live
// work is read from batch_nodes only (BatchLiveForOwner) and host liveness
// from the shared, non-acquiring lock probe. Display only, with the same
// generic-wording fallback.
func (l *cliLoop) describeOpenWorkBatch(schedule []string, generic string) string {
	writer := l.host.batchCutoverWriter()
	if writer == nil {
		return joinOpenWork(schedule, generic)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 2*time.Second)
	defer cancel()
	readQ := db.New(l.host.batchCutoverReader(writer))
	reader := batchsession.NewBatchLiveReader(readQ, l.env.DataDir())
	// TxStarter: every live read runs in ONE read-only tx on the writer.
	reader.TxStarter = writer
	res := reader.BatchLiveForOwner(ctx, l.sessionID)
	if res.Err != nil || len(res.Own) == 0 {
		return joinOpenWork(schedule, generic)
	}
	parts := append(schedule, make([]string, 0, cliOpenWorkNamed)...)
	for i, n := range res.Own {
		if i == cliOpenWorkNamed {
			parts = append(parts, fmt.Sprintf("%d more", len(res.Own)-cliOpenWorkNamed))
			break
		}
		what := pt2SoleTaskLabel(ctx, readQ, n)
		if what == "" {
			what = n.TaskKind
		}
		if what == "" {
			what = n.Kind
		}
		part := fmt.Sprintf("%s %q", what, n.ToolCallID)
		if n.ChildSessionID != "" {
			part += fmt.Sprintf(" (child session %s)", n.ChildSessionID)
		}
		part += " on " + describeBatchHost(ctx, readQ, l.env.DataDir(), n.HostID, n.HostStatus)
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// describeBatchHost renders a batch node's host: this process, or "host
// <id> (PID n, <status>)" for another one -- the PID is the retained
// display-only async_hosts row (written by every host registration,
// flag-independent), "host <id> (<status>)" when no row is readable. The
// reader's probe verdict is re-probed only if it could not decide. Display
// only; a host-read failure never changes the verdict.
func describeBatchHost(ctx context.Context, q *db.Queries, dataDir, hostID string, st sessionhosts.HostLockStatus) string {
	if sessionhosts.IsOwnHostID(hostID) {
		return "this process"
	}
	if st == batchsession.HostStatusUnknown && dataDir != "" {
		if s, err := sessionhosts.ProbeHostShared(dataDir, hostID); err == nil {
			st = s
		}
	}
	desc := "host " + hostID
	pid := int64(0)
	if h, err := q.GetAsyncHostRow(ctx, hostID); err == nil {
		pid = h.Pid
	}
	if pid > 0 {
		return fmt.Sprintf("%s (PID %d, %s)", desc, pid, st)
	}
	return fmt.Sprintf("%s (%s)", desc, st)
}

// joinOpenWork is the shared schedule-plus-fallback join.
func joinOpenWork(schedule []string, generic string) string {
	if len(schedule) > 0 {
		return strings.Join(schedule, "; ")
	}
	return generic
}

// pt2SoleTaskLabel resolves the meaningful label of one live batch root:
// the sole direct task child's task kind when the root wraps exactly one
// task (the one-task delegation wrapper), the empty string otherwise.
// Multi-task and nested-batch roots keep the structural label; a read
// failure falls back to it too. Read only, display only.
func pt2SoleTaskLabel(ctx context.Context, q *db.Queries, n batchsession.BatchLiveNode) string {
	children, err := q.ListBatchChildren(ctx, db.ListBatchChildrenParams{
		RootID:         n.Ref.RootID,
		ParentID:       sql.NullString{String: n.Ref.RootID, Valid: true},
		OwnerSessionID: n.SessionID,
	})
	if err != nil {
		return ""
	}
	label := ""
	for _, c := range children {
		if c.Kind != "task" || label != "" {
			return ""
		}
		label = c.TaskKind.String
	}
	return label
}

// stopError is the run's end by its own limits or by the operator (nil when
// neither applies): --max-cost compares the SUBTREE BUDGET (#1130: own +
// delegation children, minus the reset base — the same query the agent's
// enforceRunawayCaps runs), --max-tokens compares the root row's token
// snapshots (per-node counters, not tree-summed). Both are read before every
// paid turn and at every wake of a wait on running work or on the launch
// gate (at least every 5s), so a crossed cap or a cancel takes effect within
// one step, not when the awaited work ends (R7C-1, R8B-3): a delegated child
// charges its own cost_self the moment it spends, and the next budget read
// sees it. The exit goes through exitPrecheck: envelope, ended_reason,
// --on-finish, and the caller's Shutdown cancels the run's jobs as Ctrl-C does.
func (l *cliLoop) stopError() error {
	if l.overrides.MaxCost > 0 || l.overrides.MaxTokens > 0 {
		if sess, err := l.env.Sessions().Get(l.ctx, l.sessionID); err == nil {
			if capErr := l.capExceeded(sess); capErr != nil {
				return capErr
			}
			if sess.CancelRequested {
				return l.cancelRequestedError()
			}
			return nil
		}
	}
	return l.cancelError()
}

// cancelError is the run's end by `sessions cancel` (nil when none is pending).
func (l *cliLoop) cancelError() error {
	if canceled, err := l.env.Sessions().IsCancelRequested(l.ctx, l.sessionID); err == nil && canceled {
		return l.cancelRequestedError()
	}
	return nil
}

func (l *cliLoop) cancelRequestedError() error {
	return &runIncompleteError{Reason: "canceled", Detail: fmt.Sprintf("session %s cancelled by user", l.sessionID)}
}

// capError reports the run's --max-cost/--max-tokens as exceeded, comparing
// the same quantities as the agent's enforceRunawayCaps (subtree budget for
// cost, root-row snapshots for tokens). A single-step turn that crosses a
// cap is not cut short (fantasy ignores the step callback's error), so the
// loop checks after every Drain too.
func (l *cliLoop) capError() error {
	if l.overrides.MaxCost <= 0 && l.overrides.MaxTokens <= 0 {
		return nil
	}
	sess, err := l.env.Sessions().Get(l.ctx, l.sessionID)
	if err != nil {
		return nil
	}
	return l.capExceeded(sess)
}

func (l *cliLoop) capExceeded(sess session.Session) error {
	if l.overrides.MaxCost > 0 {
		if budget, err := l.env.Sessions().SubtreeBudget(l.ctx, l.sessionID); err == nil && budget > l.overrides.MaxCost {
			return &runIncompleteError{Reason: "error", Detail: fmt.Sprintf(
				"session %s aborted: cost $%.4f exceeds max $%.4f", l.sessionID, budget, l.overrides.MaxCost)}
		}
		// A failed budget read is not an abort: like stopError's snapshot
		// read, a transient DB error is no evidence the cap is crossed.
	}
	if total := sess.PromptTokens + sess.CompletionTokens; l.overrides.MaxTokens > 0 && total > l.overrides.MaxTokens {
		return &runIncompleteError{Reason: "error", Detail: fmt.Sprintf(
			"session %s aborted: %d tokens exceeds max %d", l.sessionID, total, l.overrides.MaxTokens)}
	}
	return nil
}

// exitedCanceled reports that the run is ending "canceled": the envelope says
// so, or the error is an operator cancel, Ctrl-C or a deadline.
func exitedCanceled(final *RunResult, err error) bool {
	if final != nil && (final.ExitReason == "canceled" || final.ExitReason == "cancelled") {
		return true
	}
	var inc *runIncompleteError
	if errors.As(err, &inc) && (inc.Reason == "canceled" || inc.Reason == "cancelled") {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// clearHonouredCancel makes the operator's cancel request one-shot on the way
// out (R8A-2): when the run ends "canceled" the request has been honoured, and
// a flag left behind would abort the next web turn or takeover Drain on the
// session after its first step. It runs before the exit flush, on a context
// detached from the run's, best effort. A run that ends for any other reason
// (a refused first turn included) honoured nothing and leaves the flag to
// whoever owns it; Drain turns never clear it (TestRunNonInteractive_
// DrainKeepsCancelRequest), so the loop still sees a request that lands mid-Drain.
func (l *cliLoop) clearHonouredCancel(final *RunResult, err error) {
	if l.sessionID == "" || !exitedCanceled(final, err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	if pending, readErr := l.env.Sessions().IsCancelRequested(ctx, l.sessionID); readErr != nil || !pending {
		return
	}
	if clearErr := l.env.Sessions().ClearCancelRequest(ctx, l.sessionID); clearErr != nil {
		slog.Warn("Failed to clear the honoured cancel request at loop exit", "session_id", l.sessionID, "err", clearErr)
	}
}

// cliTodoNudgeLimit bounds how many unfinished-todos reminders the loop may
// fire in a row without the todos changing (#A18), by analogy with the
// reaction chain guard's fuse (#1113): a model that answers every reminder
// with another announcement must not be prompted forever. The budget resets
// whenever the todos' (content,status) fingerprint changes.
const cliTodoNudgeLimit = 2

// todoNudgePrompt is the fixed user prompt of the unfinished-todos reminder
// turn (#A18). Like the reviewer pass's prompt it must steer to a concrete
// outcome, never to more announcements.
const todoNudgePrompt = `Your todo list still has unfinished items (pending or in_progress). Continue working on them now, or mark each finished item completed and remove each dropped item from the list, then end your turn with a summary of what is done and what remains. Do not end the turn while actionable work remains.`

// openTodos reads the session's todos and returns how many are pending or
// in_progress plus a fingerprint of their (content,status) pairs; a read
// failure is (0, "", err) and never fires a nudge.
func (l *cliLoop) openTodos() (count int, fingerprint string, err error) {
	sess, err := l.env.Sessions().Get(l.ctx, l.sessionID)
	if err != nil {
		return 0, "", err
	}
	var sb strings.Builder
	for _, td := range sess.Todos {
		if td.Status == session.TodoStatusCompleted {
			continue
		}
		count++
		sb.WriteString(td.Content)
		sb.WriteByte(0)
		sb.WriteString(string(td.Status))
		sb.WriteByte(0)
	}
	return count, sb.String(), nil
}

// todoNudgeDue decides whether the close path must first fire one
// unfinished-todos reminder turn (#A18). The budget resets when the todos
// changed since the previous nudge; once the budget is spent without
// progress the run is allowed to end, with a warning recorded at the exit.
func (l *cliLoop) todoNudgeDue() bool {
	// A reminder turn already failed and was dropped (C9-4): no further
	// reminders fire.
	if l.nudgeFailed {
		return false
	}
	// The last turn ended on a question for the caller: the run exits with
	// awaiting_answer, and a reminder turn would bury the question under a
	// prompt the caller never sent.
	var asked *agent.AwaitingAnswerError
	if errors.As(l.runErr, &asked) {
		return false
	}
	// A peak-hours pause must not fire a reminder turn: the same refusal
	// would kill it and surface as a "todo reminder turn failed" error.
	// The reminder is skipped with a stderr note; the todos stay for the
	// resumed run.
	var peak *agent.PeakHoursError
	if errors.As(l.runErr, &peak) {
		fmt.Fprintf(l.errOut(), "rush run: session %q: todo reminder skipped — the provider is in its peak-hours window (%v); the todos stay for the resumed run\n", l.sessionID, peak)
		return false
	}
	n, fp, err := l.openTodos()
	if err != nil || n == 0 {
		return false
	}
	if fp != l.todoFingerprint {
		l.todoFingerprint = fp
		l.todoNudges = 0
	}
	if l.todoNudges >= cliTodoNudgeLimit {
		if !l.todosGaveUp {
			l.todosGaveUp = true
			fmt.Fprintf(l.errOut(), "rush run: session %q still has %d unfinished todo(s) after %d reminder(s); ending the run (see the envelope warning)\n", l.sessionID, n, l.todoNudges)
		}
		return false
	}
	l.todoNudges++
	return true
}

// nudgePhase runs one unfinished-todos reminder turn (#A18): a plain
// prompted turn on the executor's model, then back to decide -- the closed
// scope is re-read there, and the nudge fires again only if todos are still
// open and the budget allows. A reminder that failed for its own reason is
// dropped, never the run's outcome (C9-4); cancellation, stop, a queued
// reminder and a question keep their own paths.
func (l *cliLoop) nudgePhase() cliStepResult {
	if err := l.stopError(); err != nil {
		final, exitErr := l.exitPrecheck(err)
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
	}
	usageBefore := l.sessionUsage()
	l.flushQueuedUsage(usageBefore)
	result, buffered, err := l.runTodoNudgeTurn()
	if l.ctx.Err() != nil {
		final, exitErr := l.turnCanceled(result, buffered, err, usageBefore)
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
	}
	// C9-4: an optional reminder that failed for its own reason is dropped
	// -- the executor's answer stands and no further reminders fire.
	var awaiting *agent.AwaitingAnswerError
	if err != nil && !errors.Is(err, ErrRunQueued) && !errors.As(err, &awaiting) {
		l.tot.add(result)
		if result == nil {
			l.tot.addSince(usageBefore, l.sessionUsage())
		}
		// A pending operator cancel or a crossed cap beats the drop: the run ends
		// through the normal stop exit, never as a success.
		if stopErr := l.stopError(); stopErr != nil {
			final, exitErr := l.exitPrecheck(stopErr)
			return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
		}
		l.nudgeFailed = true
		if l.final != nil {
			l.final.Warnings = append(l.final.Warnings, fmt.Sprintf("todo reminder turn failed: %v", err))
		}
		fmt.Fprintf(l.errOut(), "rush run: todo reminder turn failed: %v; the run keeps its last answer\n", err)
		l.streak = drainStreak{}
		return cliStepResult{ev: evNudgeAgain}
	}
	if done, exitErr := l.afterDrain(result, err, buffered, usageBefore); done {
		final, exitFinal := l.exit(exitErr, incompleteExitReason(exitErr))
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitFinal}
	}
	return cliStepResult{ev: evNudgeAgain}
}

// runTodoNudgeTurn runs the reminder as an ordinary prompted turn (not a
// Drain: the reminder is a real user message, on the executor's model
// unless the reviewer pass already ran).
func (l *cliLoop) runTodoNudgeTurn() (*RunResult, *bytes.Buffer, error) {
	buffered, turnOutput := l.turnSink()
	result, err := l.host.executeRun(l.ctx, RunRequest{
		Prompt: todoNudgePrompt, Overrides: l.turnOverrides, Mode: l.mode,
		ContinueSessionID: l.sessionID,
		Origin:            l.overrides.Origin, Stdout: turnOutput, Stderr: l.errOut(),
		HideSpinner:       l.hideSpinner,
		captureResult:     true,
		loopTurn:          true,
		nudgeTurn:         true,
		deferReviewer:     true,
		reviewerConfig:    l.reviewerDone,
		onSessionResolved: l.claim,
	})
	if cliLoopTurnDoneSeam != nil {
		cliLoopTurnDoneSeam()
	}
	return result, buffered, err
}

// Stage 5a: the wake-schedule side of the CLI lifetime (operator decisions,
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md sec.5): an
// ACTIVE once schedule holds the run open (CLIScopeState.OnceWakeOpen ->
// WorkOpen), an ACTIVE loop schedule never does -- when the loop's scope
// closes with nothing else open, the session's loop schedules are cancelled
// so the process cannot be kept alive by, or leave behind, an endless timer.
// A Ctrl-C/--timeout/`sessions cancel` exit is NOT a scope close: the
// schedules stay in the DB and fire when any host of that DB runs.

// cancelLoopSchedulesAtClose cancels the session's ACTIVE loop schedules at
// the ordinary scope-closed exit: one stderr line, one envelope warning
// (loopTotals.extraWarnings, so the flush carries it). Best effort on a
// detached context: a cancel failure is logged and never turns a clean end
// into an error. Once schedules are never touched (they hold the process,
// so a scope close cannot be happening while one is open) and other
// sessions' schedules are out of reach (the store API is owner-checked).
//
// It runs at EVERY scope close, not once per run: a reminder or reviewer turn
// that follows a close can create a loop schedule of its own (C9-3), and the
// next close must cancel that one too. A close with nothing to cancel is
// silent.
func (l *cliLoop) cancelLoopSchedulesAtClose() {
	canceler, ok := l.source.(agent.LoopWakeCanceler)
	if !ok || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	n, err := canceler.CancelLoopWakeSchedules(ctx, l.sessionID)
	if err != nil {
		fmt.Fprintf(l.errOut(), "rush run: session %q: cancelling the loop schedules at run end failed: %v\n", l.sessionID, err)
		return
	}
	if n == 0 {
		return
	}
	text := fmt.Sprintf("%d loop schedule(s) cancelled at run end", n)
	fmt.Fprintf(l.errOut(), "rush run: session %q: %s\n", l.sessionID, text)
	l.tot.extraWarnings = append(l.tot.extraWarnings, text)
}

// describeOnceWake renders the schedule clause of the wait heartbeat:
// what fires, when, in how long -- the same concrete-naming rule as the
// running-work clause (ASYNC-10).
func (l *cliLoop) describeOnceWake() string {
	s := l.lastScope
	if !s.OnceWakeOpen {
		return ""
	}
	in := "now"
	if d := time.Until(s.OnceWakeAt); d > 0 {
		in = "in " + d.Round(time.Second).String()
	}
	return fmt.Sprintf("wake schedule %s at %s (%s)", s.OnceWakeID,
		s.OnceWakeAt.Local().Format("15:04:05"), in)
}

// Тестовый барьер после scope read, перед ожиданием hint; production оставляет nil.
var cliLoopInterruptWaitSeam func()

// Queue остаётся единственным владельцем исполнения; CLI забирает его результат.
func (l *cliLoop) runWaitingInterrupt(messageID string) error {
	if l.env.Pump() == nil {
		return fmt.Errorf("rush run: interrupt requires the durable run queue")
	}
	before := l.sessionUsage()
	if err := l.host.drainPendingBeforeRun(l.ctx, l.sessionID); err != nil {
		return err
	}
	if err := l.host.waitLocalOwnerReleased(l.ctx, l.sessionID); err != nil {
		return err
	}
	msgs, err := l.env.Messages().List(l.ctx, l.sessionID)
	if err != nil {
		return fmt.Errorf("rush run: read interrupt outcome: %w", err)
	}
	anchor := -1
	for i := range msgs {
		if msgs[i].ID == messageID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return fmt.Errorf("rush run: accepted interrupt message is unavailable")
	}
	var final *message.Message
	counts := make(map[string]int)
	for i := anchor + 1; i < len(msgs); i++ {
		if msgs[i].Role != message.Assistant {
			continue
		}
		for _, call := range msgs[i].ToolCalls() {
			counts[call.Name]++
		}
		if msgs[i].IsFinished() {
			final = &msgs[i]
		}
	}
	if final == nil {
		return fmt.Errorf("rush run: interrupt finished without a completed assistant message")
	}
	text, notes := final.FullText(), ""
	stripped, stripError, stripReason := 0, "", ""
	if l.overrides.StripJSONFences {
		var stripErr error
		text, notes, stripErr = stripAndExtractJSON(text)
		stripped = max(len(final.FullText())-len(text), 0)
		if stripErr != nil {
			stripError, stripReason = stripErr.Error(), "invalid_json"
		}
	}
	after := l.sessionUsage()
	tokens, cost := int64(0), float64(0)
	if before.ok && after.ok {
		tokens = max(after.tokens-before.tokens, 0)
		cost = max(after.cost-before.cost, 0)
	}
	var subOutputs []SubAgentOutput
	if l.overrides.AggregationMode == "attach" {
		subOutputs = l.host.collectSubAgentOutputs(l.ctx, l.sessionID)
	}
	result := buildRunResult(l.sessionID, text, notes, string(final.FinishReason()), nil, false, counts, tokens, cost, time.Since(l.started), "", "", stripped, stripError, stripReason, subOutputs, "")
	if usage, err := l.env.Messages().UsageBySession(l.ctx, l.sessionID); err == nil {
		result.Usage.Session = buildSessionUsageInfo(usage)
	}
	l.flushQueuedUsage(before)
	l.tot.add(&result)
	l.final, l.lastBuffered = &result, bytes.NewBufferString(text)
	if l.mode == RunModeStream {
		if _, err := fmt.Fprint(l.output, text); err != nil {
			return err
		}
	}
	l.runErr, l.lastFailed = nil, nil
	l.streak = drainStreak{}
	l.reviewerDone = false
	l.reviewBasis = l.host.captureReviewBasis(l.ctx, l.overrides.ModelRole, msgs[anchor].FullText(), l.started)
	return nil
}
