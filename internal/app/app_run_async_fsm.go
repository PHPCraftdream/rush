package app

import (
	"errors"
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
	evScopeStuck   // a notice the loop stopped reacting to
	evScopeStop    // `sessions cancel` or a crossed cap while waiting
	evScopeWaitErr // the DB stayed unreadable past the limit, or the ctx died while waiting
	// phaseDrain outcomes.
	evDrainContinues // the outcome does not end the run
	evDrainGaveUp    // the refusal/setup streak exceeded its budget
	evDrainCanceled  // the ctx died while the Drain ran
	evDrainCapped    // the Drain crossed the run's budget
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
		case evScopeStuck, evScopeStop, evScopeWaitErr:
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
