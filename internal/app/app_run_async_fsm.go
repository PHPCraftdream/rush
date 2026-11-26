package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// The `rush run` loop as an explicit state machine
// (docs/plans/2026-10-01-structural-refactor.md sec. R-LOOP): the phases are
// named, the transition table is one function, the retry state between Drains
// belongs to one phase run and is recreated -- not mutated back to zero --
// when a scope closes, and every exit funnels through one finish function.

// loopTotals accumulates usage and diagnostics across all turns in one CLI run.
type loopTotals struct {
	tokens int64
	cost   float64
	counts map[string]int
	turns  []loopTurn
	// subAgentOutputs holds each child once (by session id, in first-seen
	// order) with its latest text: every turn lists ALL of the parent's
	// sub-sessions, so concatenating turns would repeat them (R3C-7).
	subAgentOutputs []SubAgentOutput
	subAgentIndex   map[string]int
	// extraWarnings carries run-level diagnostics that belong to no turn
	// (the reaction chain guard, #1113).
	extraWarnings []string
}

// loopTurn is one real turn's warnings, split so the ones that describe its
// final_text can be dropped once another turn supplies the answer (R3C-7).
type loopTurn struct {
	result       *RunResult
	warnings     []string
	textWarnings []string
}

func (t *loopTotals) add(r *RunResult) {
	if r == nil {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	t.tokens += r.Usage.DeltaTokens
	t.cost += r.Usage.DeltaCostUSD
	t.turns = append(t.turns, loopTurn{
		result:       r,
		warnings:     slices.Clone(r.Warnings),
		textWarnings: slices.Clone(r.FinalTextWarnings),
	})
	for _, out := range r.SubAgentOutputs {
		if i, ok := t.subAgentIndex[out.SessionID]; ok {
			t.subAgentOutputs[i] = out
			continue
		}
		if t.subAgentIndex == nil {
			t.subAgentIndex = make(map[string]int)
		}
		t.subAgentIndex[out.SessionID] = len(t.subAgentOutputs)
		t.subAgentOutputs = append(t.subAgentOutputs, out)
	}
	for _, stat := range r.ToolCalls {
		t.counts[stat.Name] += stat.Count
	}
}

// warningsFor is the run's warning list: every turn's diagnostics, but the
// warnings about a turn's final_text only for the turn that is the answer.
func (t *loopTotals) warningsFor(final *RunResult) []string {
	var out []string
	out = append(out, t.extraWarnings...)
	for _, turn := range t.turns {
		for _, w := range turn.warnings {
			if turn.result != final && slices.Contains(turn.textWarnings, w) {
				continue
			}
			out = append(out, w)
		}
	}
	return out
}

func (t *loopTotals) applyTo(final *RunResult, started time.Time) {
	final.Usage.DeltaTokens = t.tokens
	final.Usage.DeltaCostUSD = t.cost
	// Keep warnings appended to final after its snapshot (the loop's reviewer
	// pass); skip ones the rebuild already has, so re-application stays idempotent.
	var late []string
	for i := range t.turns {
		if t.turns[i].result == final && len(final.Warnings) > len(t.turns[i].warnings) {
			late = final.Warnings[len(t.turns[i].warnings):]
			break
		}
	}
	out := t.warningsFor(final)
	for _, w := range late {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	final.Warnings = out
	final.SubAgentOutputs = t.subAgentOutputs
	final.DurationMs = time.Since(started).Milliseconds()
	final.ToolCalls = final.ToolCalls[:0]
	for name, count := range t.counts {
		final.ToolCalls = append(final.ToolCalls, ToolCallStat{Name: name, Count: count})
	}
	slices.SortFunc(final.ToolCalls, func(a, b ToolCallStat) int { return cmpName(a.Name, b.Name) })
}

func cmpName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// flushQueuedUsage folds the TOKENS accrued since the first queued
// iteration into the run's totals. The run's cost needs no folding: the
// window is a subtree difference (startMark vs the exit reading), so spend
// between turns — whoever made it — is already inside (#1130).
func (l *cliLoop) flushQueuedUsage(now usageMark) {
	if l.queuedMark != nil {
		l.tot.addSinceTokens(*l.queuedMark, now)
		l.queuedMark = nil
	}
}

// applyTotals writes the run's totals into the envelope about to be flushed.
func (l *cliLoop) applyTotals(final *RunResult) {
	if l.queuedMark != nil {
		// Token folding for queued iterations: their spend needs no folding —
		// the subtree window below already contains it (#1130).
		l.flushQueuedUsage(l.sessionUsage())
	}
	l.tot.applyTo(final, l.started)
	// The run's cost is the SUBTREE spend from the claim to now (#1130):
	// sum of cost_self over the delegation subtree minus nothing else — the
	// difference covers what happened between turns (a delegated child's
	// spend, a human turn on the same session), which the turns' own deltas
	// miss. The per-turn sum stays as the fallback when a read failed.
	// Tokens are last-snapshot counters, summed per turn.
	if end := l.sessionUsage(); l.startMark.ok && end.ok {
		final.Usage.DeltaCostUSD = max(end.cost-l.startMark.cost, 0)
	}
}

// usageMark is the session's token and cost totals at one instant.
type usageMark struct {
	tokens int64
	cost   float64
	ok     bool
}

// sessionUsage reads the run's usage marks on a context that survives
// Ctrl-C: tokens are the root row's snapshot counters, cost is the SUBTREE
// spend (sum of cost_self over the delegation subtree, #1130).
func (l *cliLoop) sessionUsage() usageMark {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	sess, err := l.env.Sessions().Get(ctx, l.sessionID)
	if err != nil {
		return usageMark{}
	}
	spent, err := l.env.Sessions().SubtreeSpent(ctx, l.sessionID)
	if err != nil {
		return usageMark{}
	}
	return usageMark{tokens: sess.PromptTokens + sess.CompletionTokens, cost: spent, ok: true}
}

// addSince adds what the session totals gained between two marks: the
// interrupted-turn path, whose own deltas never landed anywhere else.
func (t *loopTotals) addSince(before, after usageMark) {
	if !before.ok || !after.ok {
		return
	}
	t.tokens += max(after.tokens-before.tokens, 0)
	t.cost += max(after.cost-before.cost, 0)
}

// addSinceTokens folds only the token counters between two marks: the
// queued-iteration path (#1130 — the run's cost is the subtree window, so
// folding queued spend here would double-count it at the exit).
func (t *loopTotals) addSinceTokens(before, after usageMark) {
	if !before.ok || !after.ok {
		return
	}
	t.tokens += max(after.tokens-before.tokens, 0)
}

// cliPhase names the loop's explicit phase-state transitions.
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

func setReviewerResumeSession(result *RunResult, err error, parentID, role string) {
	var timeoutErr *RunTimeoutError
	if errors.As(err, &timeoutErr) && timeoutErr.ResumeCommand != "" {
		timeoutErr.ResumeCommand = fmt.Sprintf("rush run --role %s --session %s", runResumeRole(role), parentID)
	}
	if result != nil && result.ResumeCommand != "" {
		result.ResumeCommand = fmt.Sprintf("rush run --role %s --session %s", runResumeRole(role), parentID)
	}
}

func mergeReviewerWarnings(final *RunResult, warnings []string) {
	if final == nil {
		return
	}
	for _, warning := range warnings {
		if !slices.Contains(final.Warnings, warning) {
			final.Warnings = append(final.Warnings, warning)
		}
	}
}

func (l *cliLoop) closeReviewerError(result *RunResult) cliStepResult {
	l.tot.add(result)
	if stopErr := l.stopError(); stopErr != nil {
		final, err := l.exitPrecheck(stopErr)
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	}
	if l.final != nil {
		l.final.ReviewVerdict = "error"
		mergeReviewerWarnings(l.final, result.Warnings)
	}
	final, err := l.exit(nil, "")
	return cliStepResult{ev: evCloseEnded, final: final, err: err}
}

func (l *cliLoop) closeFreshReviewerFailure(turnErr error) cliStepResult {
	warning := fmt.Sprintf("reviewer pass failed: %v", turnErr)
	mergeReviewerWarnings(l.final, []string{warning})
	l.final.ReviewVerdict = "error"
	final, err := l.exit(nil, "")
	return cliStepResult{ev: evCloseEnded, final: final, err: err}
}

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
	if final != nil && err != nil {
		if final.ExitReason == "" {
			final.ExitReason = incompleteExitReason(err)
			if exitedCanceled(nil, err) || l.ctx.Err() != nil {
				final.ExitReason = "canceled"
			}
		}
		if final.Error == "" {
			final.Error = err.Error()
		}
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
	if l.final == nil && l.lastFailed != nil && errors.Is(err, l.runErr) {
		// A terminal Drain failure may be the only result produced by the run.
		// Copy it into the output envelope so recovery can complete the missing
		// answer without mutating or losing the original failed result.
		failed := *l.lastFailed
		if failed.ExitReason == "" && err != nil {
			failed.ExitReason = incompleteExitReason(err)
		}
		if failed.Error == "" && err != nil {
			failed.Error = err.Error()
		}
		l.final = &failed
	}
	l.final, err = classifyRunTimeout(l.ctx, l.final, err, l.sessionID, l.resumeRole())
	if l.final != nil && err != nil {
		if l.final.ExitReason == "" {
			l.final.ExitReason = incompleteExitReason(err)
			if exitedCanceled(nil, err) || l.ctx.Err() != nil {
				l.final.ExitReason = "canceled"
			}
		}
		if l.final.Error == "" {
			l.final.Error = err.Error()
		}
	}
	if l.final != nil {
		failed := l.final
		if l.lastFailed != nil && errors.Is(err, l.runErr) {
			failed = l.lastFailed
		}
		if l.shouldRecoverEmptyReport(failed, err) {
			originalErr := err
			originalReason := l.final.ExitReason
			originalError := l.final.Error
			report, buffered, reportErr := l.runReportRecoveryTurn()
			if reportErr == nil && report != nil && strings.TrimSpace(report.FinalText) != "" {
				l.tot.add(report)
				l.final.FinalText = report.FinalText
				l.lastBuffered = buffered
				l.recoveredReport = true
				err = originalErr
				l.final.ExitReason, l.final.Error = originalReason, originalError
				// Report recovery completes the missing answer, so honor the
				// configured automatic reviewer pass just as for a clean answer.
				if shouldRunReviewerPass(l.overrides.ModelRole, l.env.Config.Config()) {
					review, _, reviewErr := l.runReviewerTurn()
					l.tot.add(review)
					if review != nil {
						mergeReviewerWarnings(l.final, review.Warnings)
					}
					if l.ctx.Err() != nil {
						if review != nil && reviewErr == nil &&
							(reviewerSessionMode(l.env.Config.Config()) != "fresh" || review.ReviewerSessionID != "") {
							mergeReviewerWarnings(l.final, review.Warnings)
							l.host.attachReview(l.ctx, l.final, reviewResultSessionID(review, l.sessionID), review.FinalText, l.errOut())
							if review.ReviewVerdict == "error" {
								l.final.ReviewVerdict = "error"
							}
						} else if reviewerSessionMode(l.env.Config.Config()) == "fresh" {
							failure := reviewErr
							if failure == nil {
								failure = errors.New("fresh reviewer pass did not produce a completed reviewer session")
							}
							mergeReviewerWarnings(l.final, []string{fmt.Sprintf("reviewer pass failed: %v", failure)})
							l.final.ReviewVerdict = "error"
						} else if reviewErr != nil {
							recordReviewFailure(l.final, l.errOut(), reviewErr)
						}
						l.final.ExitReason = "canceled"
						l.final.Error = l.ctx.Err().Error()
						err = l.ctx.Err()
						l.reviewerDone = true
					} else if review != nil && l.final != nil {
						for _, warning := range review.Warnings {
							if !slices.Contains(l.final.Warnings, warning) {
								l.final.Warnings = append(l.final.Warnings, warning)
							}
						}
					}
					if l.ctx.Err() == nil && reviewErr == nil && review != nil && l.final != nil {
						if reviewerSessionMode(l.env.Config.Config()) == "fresh" && review.ReviewVerdict == "error" {
							l.final.ReviewVerdict = "error"
							l.reviewerDone = true
						} else if review.ReviewerSessionID == "" && reviewerSessionMode(l.env.Config.Config()) == "fresh" {
							l.reviewerDone = true
						} else {
							l.host.attachReview(l.ctx, l.final, reviewResultSessionID(review, l.sessionID), review.FinalText, l.errOut())
							l.reviewerDone = true
						}
					}
					if l.ctx.Err() == nil && reviewErr != nil && l.final != nil {
						if reviewerSessionMode(l.env.Config.Config()) == "fresh" && (errors.Is(reviewErr, ErrRunQueued) || errors.Is(reviewErr, agent.ErrSessionBusy) || turnRefusedByOwner(reviewErr)) {
							l.reviewerDone = true
						} else if reviewerSessionMode(l.env.Config.Config()) == "fresh" {
							warning := fmt.Sprintf("reviewer pass failed: %v", reviewErr)
							if !slices.Contains(l.final.Warnings, warning) {
								l.final.Warnings = append(l.final.Warnings, warning)
							}
							l.final.ReviewVerdict = "error"
						} else {
							recordReviewFailure(l.final, l.errOut(), reviewErr)
						}
						l.reviewerDone = true
					}
					// Keep the report turn's buffer authoritative; review output is
					// metadata, not the recovered answer shown in terse/stream mode.
				}
			} else {
				if reportErr == nil {
					reportErr = errors.New("report recovery turn completed without a final answer")
				}
				warning := fmt.Sprintf("report recovery failed: %v", reportErr)
				fmt.Fprintf(l.errOut(), "rush run: %s\n", warning)
				if !slices.Contains(l.final.Warnings, warning) {
					l.final.Warnings = append(l.final.Warnings, warning)
				}
				l.reportRecoveryTried = true
				l.final.ExitReason, l.final.Error = originalReason, originalError
				err = originalErr
			}
		}
	}
	if l.lastBuffered == nil {
		l.lastBuffered = &bytes.Buffer{}
	}
	if l.final != nil {
		l.applyTotals(l.final)
		if l.reportRecoveryTried && !l.recoveredReport {
			l.final.Recovered = false
			if !slices.Contains(l.final.Warnings, "report recovery failed after a non-replayable provider stream failure") {
				l.final.Warnings = append(l.final.Warnings, "report recovery failed after a non-replayable provider stream failure")
			}
		}
		if l.recoveredReport {
			l.final.Recovered = true
			l.final.Warnings = append(l.final.Warnings, "recovered empty final answer with a new report-composition user turn after a non-replayable provider stream failure")
		}
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
