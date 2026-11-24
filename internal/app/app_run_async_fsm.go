package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// The `rush run` loop as an explicit state machine
// (docs/plans/2026-10-01-structural-refactor.md sec. R-LOOP): the phases are
// named, the transition table is one function, the retry state between Drains
// belongs to one phase run and is recreated -- not mutated back to zero --
// when a scope closes, and every exit funnels through one finish function.

// cliPhase is one phase of the loop.
type cliPhase int

const (
	// phaseFirst runs the user's own turn (the only turn with a prompt).
	phaseFirst cliPhase = iota
	// phaseDecide asks the coordinator's CLIScope what to do next, waiting as
	// long as the answer says (paced retry, open work).
	phaseDecide
	// phaseDrain runs one Drain iteration: the pre-launch checks, the turn,
	// the classification of its outcome.
	phaseDrain
	// phaseNudge runs one unfinished-todos reminder turn (#A18): a prompted
	// turn fired when the scope closed while todos are still open, so the
	// executor finishes its list instead of the run ending on an announcement.
	phaseNudge
	// phaseClose handles a closed scope: the reviewer pass when due, then the
	// run's end.
	phaseClose
	// phaseExit is terminal: the run's result and error are fixed.
	phaseExit
)

func (p cliPhase) String() string {
	switch p {
	case phaseFirst:
		return "first"
	case phaseDecide:
		return "decide"
	case phaseDrain:
		return "drain"
	case phaseNudge:
		return "nudge"
	case phaseClose:
		return "close"
	default:
		return "exit"
	}
}

// cliEvent is what a phase reports when it returns; transition maps it to the
// next phase.
type cliEvent int

const (
	evBegin cliEvent = iota // run() starts: dispatch phaseFirst
	// phaseFirst outcomes.
	evFirstContinue // the first turn ran; the loop starts deciding
	evFirstDead     // setup failed before the turn was submitted: fail with no exit effects
	evFirstLockBusy // the first turn was refused: another owner holds the session
	evFirstCanceled // no session resolved, or the ctx died around the first turn
	// phaseDecide outcomes.
	evScopeDrain   // a Drain is owed
	evScopeClosed  // deferred or no debt, nothing running: the scope closed
	evTodosNudge   // the scope closed with unfinished todos: fire one reminder (#A18)
	evScopeStuck   // a notice the loop stopped reacting to
	evScopeStop    // `sessions cancel` or a crossed cap while waiting
	evScopeWaitErr // the DB stayed unreadable past the limit, or the ctx died while waiting
	// phaseDrain outcomes.
	evDrainContinues // the outcome does not end the run
	evDrainGaveUp    // the refusal/setup streak exceeded its budget
	evDrainCanceled  // the ctx died while the Drain ran
	evDrainCapped    // the Drain crossed the run's budget
	// phaseNudge outcomes.
	evNudgeAgain // the reminder turn ran; decide again
	evNudgeEnded // the reminder path ended the run (stop, cancel, gave-up)
	// phaseClose outcomes.
	evCloseAgain // the reviewer ran clean: decide again, on its options
	evCloseEnded // the close phase ended the run
)

// transition is the machine's whole transition table: (phase, event) -> next
// phase. An unmatched pair falls back to phaseExit, the only terminal.
func transition(phase cliPhase, ev cliEvent) cliPhase {
	switch phase {
	case phaseFirst:
		switch ev {
		case evBegin:
			return phaseFirst
		case evFirstContinue:
			return phaseDecide
		}
	case phaseDecide:
		switch ev {
		case evScopeDrain:
			return phaseDrain
		case evScopeClosed:
			return phaseClose
		case evTodosNudge:
			return phaseNudge
		case evScopeStuck, evScopeStop, evScopeWaitErr:
			return phaseExit
		}
	case phaseNudge:
		switch ev {
		case evNudgeAgain:
			return phaseDecide
		case evNudgeEnded:
			return phaseExit
		}
	case phaseDrain:
		switch ev {
		case evDrainContinues:
			return phaseDecide
		case evDrainGaveUp, evDrainCanceled, evDrainCapped:
			return phaseExit
		}
	case phaseClose:
		switch ev {
		case evCloseAgain:
			return phaseDecide
		case evCloseEnded:
			return phaseExit
		}
	}
	return phaseExit
}

// cliStepResult is one phase's outcome: the event for transition and, when the
// event ends the run, the result and error the run returns.
type cliStepResult struct {
	ev    cliEvent
	final *RunResult
	err   error
}

// cliScopeAction is the pure decision classifyScope reads out of one
// CLIScopeState: what the decide phase does next. The wait actions block
// inside nextStep; the mapping itself is table-tested.
type cliScopeAction int

const (
	scopeActDrain     cliScopeAction = iota // Owed: run a Drain
	scopeActWaitPaced                       // Paced: wait for the gate
	scopeActWaitOpen                        // work open: wait for a hint
	scopeActStuck                           // Stuck: give up
	scopeActExit                            // deferred/no debt, nothing running: the scope closed
)

// classifyScope maps one CLIScopeState to the decide phase's action
// (doc sec.3.5). The order is the doc's: Owed, Paced, WorkOpen, Stuck,
// Deferred, none.
func classifyScope(s agent.CLIScopeState) cliScopeAction {
	switch {
	case s.Drain == agent.DrainOwed:
		return scopeActDrain
	case s.Drain == agent.DrainPaced:
		return scopeActWaitPaced
	case s.WorkOpen:
		return scopeActWaitOpen
	case s.Drain == agent.DrainStuck:
		return scopeActStuck
	}
	return scopeActExit
}

// drainVerdict is the pure classification of one finished Drain iteration,
// before any state changes: what afterDrain does with it follows from this
// and from the streak alone.
type drainVerdict int

const (
	drainQueued      drainVerdict = iota // it queued behind another owner: ran no turn of its own
	drainRefused                         // not attempted: the launch gate refused it
	drainSetupFailed                     // failed in setup, before any turn
	drainCompleted                       // completed, or a question -- an answer of its own kind
	drainFailed                          // ran and failed
)

// classifyDrainOutcome maps one finished Drain's (result, err) to its verdict.
func classifyDrainOutcome(result *RunResult, err error) drainVerdict {
	var awaiting *agent.AwaitingAnswerError
	switch {
	case errors.Is(err, ErrRunQueued):
		return drainQueued
	case agent.IsDrainNotAttempted(err):
		return drainRefused
	case result == nil && err != nil && !errors.As(err, &awaiting):
		return drainSetupFailed
	case err == nil || errors.As(err, &awaiting):
		return drainCompleted
	}
	return drainFailed
}

// drainStreak is the retry state of one run of Drains between scope closes:
// the streak's start, the last failed attempt the paced notice names, and the
// gate closure the notice was last printed for. The whole struct is replaced
// with a zero value when a scope closes (R5C-2), so a streak cannot outlive
// the phase run it belongs to.
type drainStreak struct {
	since         time.Time
	failed        error
	pacedNoticeAt time.Time
}

// endedReasonWriteAttempts bounds the best-effort ended_reason write: each
// attempt gets a fresh cleanupTimeout budget detached from the run's context.
// A single attempt was observed dying on "context deadline exceeded" while a
// burst of concurrent loop exits contended for the same SQLite write lock --
// the exit envelope had already been flushed, nothing retried, and the row
// kept an empty ended_reason forever, which the session classifier then
// reports as a bare "idle: no live work and no recorded end". Three fresh
// budgets ride out a seconds-long contention window while staying far below
// the 30s busy_timeout a caller could observe as a hang.
const endedReasonWriteAttempts = 3

// writeEndedReasonWithRetry runs write up to endedReasonWriteAttempts times,
// each attempt on a fresh cleanupTimeout budget derived from parent (its
// cancellation and deadline never leak into the attempt). Returns nil once
// write succeeds, otherwise the last error.
func writeEndedReasonWithRetry(parent context.Context, write func(ctx context.Context) error) error {
	var err error
	for attempt := 0; attempt < endedReasonWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
		err = write(ctx)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// persistEndedReason is the ONLY writer of the session row's ended_reason for a
// `rush run` loop (R8A-1): the column is empty while the run is in progress
// (ExecuteRun's per-turn write is suppressed for the loop's turns, loopTurn)
// and holds the run's exit reason afterwards -- the envelope's exit_reason the
// loop is about to flush. Every loop exit (Ctrl-C or --timeout while waiting,
// a stuck debt, a cap or cancel caught by the precheck or while waiting, an
// unreadable DB, the ordinary close) goes through exit or flushed, which call
// it. The write is retried on fresh cleanupTimeout budgets (see
// writeEndedReasonWithRetry): a one-shot budget was observed dropping the
// write under concurrent loop-exit bursts. A run that crashed (kill -9) never
// reaches it and keeps the empty value.
//
// Without an envelope (the launched first turn returned none) the reason is
// derived from the error; a first turn that never launched (refused, lock
// busy) wrote nothing and records nothing.
func (l *cliLoop) persistEndedReason(final *RunResult, err error) {
	reason := l.endedReasonFor(final, err)
	if reason == "" || l.sessionID == "" {
		return
	}
	if writeErr := writeEndedReasonWithRetry(l.ctx, func(ctx context.Context) error {
		return l.recordEndedReason(ctx, reason)
	}); writeErr != nil {
		slog.Warn("Failed to persist ended_reason at loop exit", "session_id", l.sessionID, "reason", reason, "err", writeErr)
	}
}

// recordEndedReason writes reason unless the row already holds it. The
// per-attempt read keeps the idempotent skip when a retry follows a write
// that failed on the caller's side after landing.
func (l *cliLoop) recordEndedReason(ctx context.Context, reason string) error {
	if l.env.DB != nil {
		var current string
		qErr := l.env.DB().QueryRowContext(ctx,
			`SELECT COALESCE(ended_reason, '') FROM sessions WHERE id = ?`, l.sessionID).Scan(&current)
		if qErr == nil && current == reason {
			return nil
		}
	}
	return l.env.Sessions().SetEndedReason(ctx, l.sessionID, reason)
}

// endedReasonFor is the value persistEndedReason records ("" = record nothing).
func (l *cliLoop) endedReasonFor(final *RunResult, err error) string {
	switch {
	case l.refusedByOwner:
		return ""
	case final != nil && final.ExitReason != "":
		return final.ExitReason
	case final != nil:
		return "done"
	case !l.firstSubmitted || err == nil:
		return ""
	case exitedCanceled(nil, err) || l.ctx.Err() != nil:
		return "canceled"
	}
	var inc *runIncompleteError
	if errors.As(err, &inc) && inc.Reason != "" {
		return inc.Reason
	}
	return "error"
}

// The loop's exit path: every way a `rush run` loop ends goes through finish
// exactly once (split out of app_run_async.go, which was at the 1000-line limit).

// exit computes the exit envelope's reason for err (reason, when set, is the
// exit_reason to record; otherwise a failed last Drain's own classification is
// carried over) and finishes the run.
func (l *cliLoop) exit(err error, reason string) (*RunResult, error) {
	final := l.final
	// Turn-stall abort: the turn died past its stall timeout with the
	// finish part already written — surface a dedicated "stalled"
	// exit_reason (non-zero exit, since err is non-nil).
	if err != nil && reason == "" && errors.Is(err, agent.ErrTurnStalled) {
		reason = "stalled"
	}
	if final != nil {
		switch {
		case err != nil && reason != "":
			final.ExitReason = reason
			final.Error = err.Error()
		case err != nil && l.lastFailed != nil && errors.Is(err, l.runErr):
			final.ExitReason = l.lastFailed.ExitReason
			final.Error = l.lastFailed.Error
		}
	}
	return l.finish(err)
}

// finish is the loop's ONE exit path: totals of every real turn are applied
// to the final envelope, the operator's honoured cancel request is cleared,
// the session row's ended_reason is made to match the envelope
// (persistEndedReason), and the envelope is flushed through the one common
// path; the caller gets (final, err). Every loop exit -- refusal give-up,
// ctx cancellation, a wait error, a stuck debt, or the ordinary scope-closed
// end -- goes through here exactly once.
func (l *cliLoop) finish(err error) (*RunResult, error) {
	l.final, err = classifyRunTimeout(l.ctx, l.final, err, l.sessionID, l.resumeRole())
	if l.lastBuffered == nil {
		l.lastBuffered = &bytes.Buffer{}
	}
	if l.final != nil {
		l.applyTotals(l.final)
		// A18: the budget for unfinished-todos reminders was spent without
		// the todos moving -- the orchestrator must see why the run ended
		// with the list still open.
		if l.todosGaveUp {
			if n, _, todoErr := l.openTodos(); todoErr == nil && n > 0 {
				l.final.Warnings = append(l.final.Warnings, fmt.Sprintf(
					"ended with unfinished todos: %d item(s) still pending/in_progress after %d reminder(s)", n, l.todoNudges))
			}
		}
	}
	l.clearHonouredCancel(l.final, err)
	l.persistEndedReason(l.final, err)
	if flushErr := flushLoopExit(l.output, l.mode, l.final, l.lastBuffered); flushErr != nil {
		return l.final, errors.Join(err, flushErr)
	}
	return l.final, err
}

func (l *cliLoop) exitCanceled() (*RunResult, error) {
	err := l.runErr
	if err == nil {
		err = l.ctx.Err()
	}
	if l.final == nil && l.ctx.Err() != nil && l.sessionID != "" {
		// The first turn was cut short before it produced an envelope: the
		// run still owes its caller one (session, reason, usage so far).
		l.final = &RunResult{SessionID: l.sessionID, ToolCalls: []ToolCallStat{}}
		if l.lastBuffered == nil {
			l.lastBuffered = &bytes.Buffer{}
		}
	}
	if l.final != nil && l.ctx.Err() != nil {
		l.final.ExitReason = "canceled"
		l.final.Error = l.ctx.Err().Error()
	}
	return l.finish(err)
}

func (l *cliLoop) exitWait(waitErr error) (*RunResult, error) {
	if l.final != nil {
		l.final.ExitReason = "canceled"
		if !errors.Is(waitErr, context.Canceled) && !(l.ctx.Err() != nil && errors.Is(waitErr, context.DeadlineExceeded)) {
			// A persistent DB-read failure is not a cancellation.
			l.final.ExitReason = "error"
		}
		l.final.Error = waitErr.Error()
	}
	return l.finish(waitErr)
}

func (l *cliLoop) exitPrecheck(err error) (*RunResult, error) {
	var inc *runIncompleteError
	reason := "error"
	if errors.As(err, &inc) {
		reason = inc.Reason
	}
	return l.exit(err, reason)
}

// incompleteExitReason maps a run-terminating error onto the exit_reason of
// the give-up exits that used to hard-code "error". The error's own
// classification wins when it carries one (a peak-hours refusal carries
// "peak_hours"); everything else stays "error".
func incompleteExitReason(err error) string {
	var inc *runIncompleteError
	if errors.As(err, &inc) && inc.Reason != "" {
		return inc.Reason
	}
	return "error"
}
