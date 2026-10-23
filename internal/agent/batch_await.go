package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// batchAwaitPollInterval is the batch worker await's fallback re-read cadence;
// level-triggered hints are the primary wake. A new-file var so tests can
// shrink it without touching existing files.
var batchAwaitPollInterval = time.Second

// batchAwaitWorkerCap bounds every batch in-tool wait, mirroring the old 40m
// workerAwaitBlockCap; it must stay under the single-tool watchdog.
var batchAwaitWorkerCap = 40 * time.Minute

// batchAwaitMinWaitSeconds is the injected bounds' default floor of 60
// seconds, mirroring awaitTasksMinWaitSeconds. A var so tests can shrink it.
var batchAwaitMinWaitSeconds = 60

const (
	// batchAwaitMaxWaitSeconds is the injected bounds' default ceiling,
	// mirroring awaitTasksMaxWaitSeconds (six hours).
	batchAwaitMaxWaitSeconds = 21600

	// batchAwaitRefusal mirrors awaitTasksWorker's empty-read refusal.
	batchAwaitRefusal = "nothing is running; continue or finish"

	batchAwaitModeAny = "any"
	batchAwaitModeAll = "all"

	// batchNotifyAll and batchNotifyEach are the per-root notify modes.
	batchNotifyAll  = "all"
	batchNotifyEach = "each"
)

// BatchAwaitUnit names one captured root's await unit: the root under
// NotifyAll, or the root plus its direct children under NotifyEach.
type BatchAwaitUnit struct {
	Ref        session.BatchLiveRef
	ToolCallID string
	// Disappeared marks a captured root whose complete level read reports
	// deletion (unannounced-root recovery or retention). The unit is
	// Undetermined: never Finished, never a success.
	Disappeared bool
	// ChildFinished marks a NotifyEach unit settled by a terminal direct
	// child while the root itself is still nonterminal.
	ChildFinished bool
}

// BatchAwaitResult is the single struct returned by BatchAwaitWorker's one
// return point. Timeout reports remaining work without stopping it.
type BatchAwaitResult struct {
	Mode          string
	TimedOut      bool
	FinishedCount int
	Finished      []BatchAwaitUnit
	Remaining     []BatchAwaitUnit
}

// BatchAwaitParams carries every dependency; the function holds no globals,
// no admission token and has no production caller in this package.
type BatchAwaitParams struct {
	Mode           string // "any" or "all"
	MaxWaitSeconds int    // 0 = omitted
	// Capture reads every nonterminal root of the session scope ONCE.
	Capture func(context.Context) session.BatchCapturedRoots
	// Levels rereads only the captured refs (BatchCapturedLevel contract).
	Levels func(context.Context) session.BatchCapturedLevels
	// Hints returns the level-triggered hint channel and its cancel.
	Hints func() (<-chan struct{}, func())
	// NotifyModeOf classifies a captured root: batchNotifyAll or
	// batchNotifyEach. Chosen as a lookup so the capture stays a value.
	NotifyModeOf func(session.BatchLiveRef) string
	// PollInterval is the fallback tick; zero uses batchAwaitPollInterval.
	PollInterval time.Duration
	// WaitCap bounds every wait; zero uses batchAwaitWorkerCap.
	WaitCap time.Duration
	// MinWaitSeconds and MaxWaitBounds are the injected validation bounds so
	// tests can exercise them; both zero falls back to the 60..21600
	// defaults (batchAwaitMinWaitSeconds/batchAwaitMaxWaitSeconds).
	MinWaitSeconds int
	MaxWaitBounds  int
}

// batchAwaitTarget is one captured root with its per-capture notify mode.
type batchAwaitTarget struct {
	ref       session.BatchLiveRef
	notifyAll bool
}

// BatchAwaitWorker is the batch replacement of awaitTasksWorker (AW-a). It is
// an inert pure function: injected reads and timers, no runner or store
// admission, no arbiter access. One return point ends the select loop; the
// guard returns before the loop are validation/capture refusals only.
func BatchAwaitWorker(ctx context.Context, p BatchAwaitParams) (BatchAwaitResult, error) {
	if p.Mode != batchAwaitModeAny && p.Mode != batchAwaitModeAll {
		return BatchAwaitResult{}, fmt.Errorf("mode must be %q or %q", batchAwaitModeAny, batchAwaitModeAll)
	}
	minWait, maxWait := p.MinWaitSeconds, p.MaxWaitBounds
	if minWait <= 0 && maxWait <= 0 {
		minWait, maxWait = batchAwaitMinWaitSeconds, batchAwaitMaxWaitSeconds
	}
	// 0 = omitted; out of bounds is an error, exactly like the old path.
	if p.MaxWaitSeconds != 0 && (p.MaxWaitSeconds < minWait || p.MaxWaitSeconds > maxWait) {
		return BatchAwaitResult{}, fmt.Errorf(
			"max_wait_seconds must be between %d and %d, or omitted", minWait, maxWait)
	}
	if p.Capture == nil || p.Levels == nil || p.NotifyModeOf == nil {
		return BatchAwaitResult{}, errors.New("batch await: capture, levels and notify-mode deps are required")
	}
	captured := p.Capture(ctx)
	refs := captured.Refs()
	// A complete read with zero refs proves absence; refuse like the old
	// path. An INCOMPLETE capture with zero refs must NOT refuse: the read
	// may have lagged, so waiting conservatively is correct.
	if captured.Complete() && len(refs) == 0 {
		return BatchAwaitResult{}, errors.New(batchAwaitRefusal)
	}
	targets := make([]batchAwaitTarget, 0, len(refs))
	for _, ref := range refs {
		targets = append(targets, batchAwaitTarget{
			ref:       ref,
			notifyAll: p.NotifyModeOf(ref) != batchNotifyEach,
		})
	}
	pollInterval := p.PollInterval
	if pollInterval <= 0 {
		pollInterval = batchAwaitPollInterval
	}
	waitCap := p.WaitCap
	if waitCap <= 0 {
		waitCap = batchAwaitWorkerCap
	}
	// Deadline = min(effective max_wait, cap). A re-created deadline would
	// drift, so compute once.
	if p.MaxWaitSeconds > 0 {
		if d := time.Duration(p.MaxWaitSeconds) * time.Second; d < waitCap {
			waitCap = d
		}
	}
	var hintCh <-chan struct{}
	stopHints := func() {}
	if p.Hints != nil {
		hintCh, stopHints = p.Hints()
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	deadline := time.NewTimer(waitCap)
	defer deadline.Stop()

	var out BatchAwaitResult
	var outErr error
	evaluated := false
	// Exactly ONE return for the whole wait: every branch breaks to it.
loop:
	for {
		// Cancellation wins over same-tick hints and poll ticks.
		if err := ctx.Err(); err != nil {
			outErr = err
			break loop
		}
		if !evaluated {
			// Level-triggered initial evaluation so terminal state landing
			// between capture and the first wait is not lost.
			evaluated = true
			out.Finished, out.Remaining = batchAwaitEvaluateFor(p.Mode, targets, p.Levels(ctx))
			if batchAwaitSatisfied(p.Mode, out.Finished, out.Remaining) {
				out.Mode = p.Mode
				out.FinishedCount = len(out.Finished)
				break loop
			}
		}
		select {
		case <-ctx.Done():
			outErr = ctx.Err()
			break loop
		case <-hintCh:
			// Hint: re-evaluate captured levels only. New roots are not
			// captured (BatchCapturedLevel rereads captured refs alone).
			out.Finished, out.Remaining = batchAwaitEvaluateFor(p.Mode, targets, p.Levels(ctx))
		case <-poll.C:
			// Poll fallback re-read; identical semantics to a hint.
			out.Finished, out.Remaining = batchAwaitEvaluateFor(p.Mode, targets, p.Levels(ctx))
		case <-deadline.C:
			// Timeout reports remaining work without stopping it and
			// without an error.
			out.Finished, out.Remaining = batchAwaitEvaluateFor(p.Mode, targets, p.Levels(ctx))
			out.TimedOut = true
			out.Mode = p.Mode
			out.FinishedCount = len(out.Finished)
			break loop
		}
		if batchAwaitSatisfied(p.Mode, out.Finished, out.Remaining) {
			out.Mode = p.Mode
			out.FinishedCount = len(out.Finished)
			break loop
		}
	}
	out.Mode = p.Mode
	out.FinishedCount = len(out.Finished)
	stopHints()
	return out, outErr
}

// batchAwaitSatisfied reports whether the wait may break. A disappeared root
// breaks BOTH modes immediately and is never counted finished.
func batchAwaitSatisfied(mode string, finished, remaining []BatchAwaitUnit) bool {
	for _, unit := range remaining {
		if unit.Disappeared {
			return true
		}
	}
	if mode == batchAwaitModeAny {
		return len(finished) > 0
	}
	return len(remaining) == 0 && len(finished) > 0
}

// batchAwaitEvaluateFor evaluates one level read for the wait's mode. ALL
// needs every captured root Done, so a NotifyEach unit settled only by a
// terminal child stays remaining there; ANY counts it as the first unit.
func batchAwaitEvaluateFor(mode string, targets []batchAwaitTarget, levels session.BatchCapturedLevels) (finished, remaining []BatchAwaitUnit) {
	finished, remaining, _ = batchAwaitEvaluate(targets, levels)
	if mode != batchAwaitModeAll {
		return finished, remaining
	}
	kept := make([]BatchAwaitUnit, 0, len(finished))
	for _, unit := range finished {
		if unit.ChildFinished {
			remaining = append(remaining, unit)
			continue
		}
		kept = append(kept, unit)
	}
	return kept, remaining
}

// batchAwaitEvaluate classifies captured roots against one level read.
// An incomplete or failed read removes NOTHING: every unit stays remaining.
func batchAwaitEvaluate(targets []batchAwaitTarget, levels session.BatchCapturedLevels) (finished, remaining []BatchAwaitUnit, disappeared bool) {
	complete := levels.Complete && levels.Err == nil
	byRef := make(map[session.BatchLiveRef]session.BatchLevelResult, len(levels.Roots))
	for _, root := range levels.Roots {
		byRef[root.Ref] = root
	}
	for _, target := range targets {
		unit := BatchAwaitUnit{Ref: target.ref}
		level, ok := byRef[target.ref]
		if !complete || !ok || !level.Complete || level.Err != nil {
			// Incomplete read: conservative lag toward live.
			remaining = append(remaining, unit)
			continue
		}
		unit.ToolCallID = level.Node.ToolCallID
		if level.Disappeared {
			// Disappearance BREAKS the wait as Undetermined. Such a root can
			// never wake (recovery deleted it), so waiting the full cap is a
			// guaranteed deadlock with no possible state change; breaking
			// early reports the remaining work without stopping it and never
			// fabricates success.
			unit.Disappeared = true
			disappeared = true
			remaining = append(remaining, unit)
			continue
		}
		if level.Finished {
			finished = append(finished, unit)
			continue
		}
		if !target.notifyAll {
			for _, child := range level.Children {
				if child.State != "" && batchAwaitTerminalState(child.State) {
					unit.ChildFinished = true
					break
				}
			}
			if unit.ChildFinished {
				finished = append(finished, unit)
				continue
			}
		}
		remaining = append(remaining, unit)
	}
	return finished, remaining, disappeared
}

// batchAwaitTerminalState mirrors batchReadTerminal's vocabulary without
// reaching into the session package's unexported helper.
func batchAwaitTerminalState(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// BatchSleepBreakers are the inputs that may break an armed sleep: a pending
// child question, a fired wake_only wake, or an expired max_wait schedule.
type BatchSleepBreakers struct {
	QuestionDebt   bool
	WakeFired      bool
	MaxWaitExpired bool
}

func (b BatchSleepBreakers) any() bool {
	return b.QuestionDebt || b.WakeFired || b.MaxWaitExpired
}

// BatchArbiterDeps inject the two durable reads plus the per-row question
// classifier (nil uses the child_question default).
type BatchArbiterDeps struct {
	// Live reads the session scope's batch live facts (BatchLiveForOwner).
	Live func(context.Context) session.BatchLiveResult
	// Debt reads pending-inclusive selected-notice debt
	// (BatchStore.BatchReactionPendingInclusiveDebtRows).
	Debt func(context.Context) (session.BatchPendingDebt, error)
	// NoticeQuestion classifies a notice row as question-class.
	NoticeQuestion func(session.PendingNoticeDebt) bool
}

// BatchArbiterFacts are the batch replacements of turn_arbiter's rule 1 and
// rule 4b session/debt inputs. Incomplete or failed reads are conservative:
// liveness and debt flags stay TRUE (lag toward live, never falsely settled).
type BatchArbiterFacts struct {
	Complete bool
	Err      error
	// LiveRoots replaces RunningOwnJobs>0 || RunningDelegation: the R10a
	// read covers own roots and delegation descendants (child_session_id
	// node links), and HasLive keeps Pending, Cancelling and held-report
	// roots live because a held report never settles the row.
	LiveRoots bool
	// DebtPendingIncl is rule 1's pending-inclusive selected-notice debt.
	// Empty plus complete reads give false; unknown debt gives TRUE because
	// claiming "no debt" would produce a false VNone.
	DebtPendingIncl bool
	// CompletionOnly: every pending row is completion-class. Selected batch
	// completion notices are completion-class; child_question is not.
	CompletionOnly bool
}

// StopIntentIsNotCompletion is a documented constant fact (AW-b): stop intent
// creates no notice row by construction in R8, so debt readers never see it,
// and a stop acknowledgment is neither a completion nor an exception to
// selected-notice debt.
func (BatchArbiterFacts) StopIntentIsNotCompletion() bool { return false }

// SleepHolds is rule 4b's predicate: the armed sleep defers only while the
// debt is entirely completions, live roots remain, and no breaker fired.
func (f BatchArbiterFacts) SleepHolds(sleepAll bool, breakers BatchSleepBreakers) bool {
	return sleepAll && f.CompletionOnly && f.LiveRoots && !breakers.any()
}

// ReadBatchArbiterFacts computes the arbiter facts from injected reads. Old-fact
// mapping (see batch_await_doc.go): RunningOwnJobs/RunningDelegation ->
// LiveRoots via BatchLiveResult.HasLive; Debt.PendingIncl ->
// DebtPendingIncl; DebtFacts.CompletionOnly -> CompletionOnly.
func ReadBatchArbiterFacts(ctx context.Context, deps BatchArbiterDeps) (BatchArbiterFacts, error) {
	f := BatchArbiterFacts{}
	if deps.Live == nil || deps.Debt == nil {
		return f, errors.New("batch arbiter facts: live and debt deps are required")
	}
	var firstErr error
	live := deps.Live(ctx)
	if !live.Complete || live.Err != nil {
		// Incomplete live read: still live, conservatively.
		f.LiveRoots = true
		if live.Err != nil {
			firstErr = live.Err
		}
	} else {
		f.LiveRoots = live.HasLive()
	}
	debt, err := deps.Debt(ctx)
	if err != nil {
		// Unknown debt must NOT claim "no debt": rule 1 with no debt means
		// VNone, so unknown debt stays pending.
		f.DebtPendingIncl = true
		f.Complete = false
		if firstErr == nil {
			firstErr = err
		}
	} else {
		f.DebtPendingIncl = len(debt.Nodes) > 0 || len(debt.Notices) > 0
		question := func(n session.PendingNoticeDebt) bool {
			if deps.NoticeQuestion != nil {
				return deps.NoticeQuestion(n)
			}
			return n.Kind == session.NoticeKindChildQuestion
		}
		// Node rows are selected completion notices after section 4.1
		// elision: completion-class unconditionally, like job debt was.
		f.CompletionOnly = f.DebtPendingIncl
		for _, n := range debt.Notices {
			if question(n) {
				f.CompletionOnly = false
				break
			}
		}
	}
	// Read errors stay inside Err with Complete=false: the facts are
	// already conservative, so no extra error return is needed.
	if firstErr == nil {
		f.Complete = true
	} else {
		f.Err = firstErr
	}
	return f, nil
}

// BatchScopeDeps inject the scope's two reads.
type BatchScopeDeps struct {
	Live func(context.Context) session.BatchLiveResult
	Debt func(context.Context) (session.BatchPendingDebt, error)
}

// BatchScopeFacts answers "does this session scope still have live roots or
// open debt" for a CLI caller. WorkOpen is the batch counterpart of
// CLIScopeState.WorkOpen's work half. Scope lifetime stays an INDEPENDENT
// fact: OnceWakeOpen and completion holds are deliberately NOT folded in
// here; the CLI caller combines them (design requires independence).
type BatchScopeFacts struct {
	Complete  bool
	LiveRoots bool
	OpenDebt  bool
	// WorkOpen is LiveRoots || OpenDebt under the conservative-read rule.
	WorkOpen bool
}

// ReadBatchScopeFacts computes the CLI scope work facts. An incomplete or
// failed read keeps the corresponding flag true (conservative).
func ReadBatchScopeFacts(ctx context.Context, deps BatchScopeDeps) BatchScopeFacts {
	f := BatchScopeFacts{}
	if deps.Live == nil || deps.Debt == nil {
		f.LiveRoots = true
		f.OpenDebt = true
		f.WorkOpen = true
		return f
	}
	live := deps.Live(ctx)
	if !live.Complete || live.Err != nil {
		f.LiveRoots = true
	} else {
		f.LiveRoots = live.HasLive()
	}
	debt, err := deps.Debt(ctx)
	if err != nil {
		f.OpenDebt = true
	} else {
		f.OpenDebt = len(debt.Nodes) > 0 || len(debt.Notices) > 0
	}
	f.WorkOpen = f.LiveRoots || f.OpenDebt
	f.Complete = true
	return f
}
