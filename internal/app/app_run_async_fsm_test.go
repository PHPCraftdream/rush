package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// transition's table: the loop's forward edges, and every other (phase,
// event) pair ends the run. Revert-check: an event falling out of its phase's
// switch lands on phaseExit, so a handler returning the wrong event ends the
// run instead of looping forever.
func TestCLILoopTransition_Table(t *testing.T) {
	t.Parallel()
	all := []cliEvent{
		evBegin, evFirstContinue, evFirstDead, evFirstLockBusy, evFirstCanceled,
		evScopeDrain, evScopeClosed, evScopeStuck, evScopeStop, evScopeWaitErr,
		evDrainContinues, evDrainGaveUp, evDrainCanceled, evDrainCapped,
		evTodosNudge, evNudgeAgain, evNudgeEnded, evCloseAgain, evCloseEnded,
	}
	phases := []cliPhase{phaseFirst, phaseDecide, phaseDrain, phaseNudge, phaseClose, phaseExit}
	for _, phase := range phases {
		for _, ev := range all {
			// The forward edges, spelled out per source phase; every other
			// pair ends the run.
			want := phaseExit
			switch phase {
			case phaseFirst:
				switch ev {
				case evBegin:
					want = phaseFirst
				case evFirstContinue:
					want = phaseDecide
				}
			case phaseDecide:
				switch ev {
				case evScopeDrain:
					want = phaseDrain
				case evScopeClosed:
					want = phaseClose
				case evTodosNudge:
					want = phaseNudge
				}
			case phaseDrain:
				if ev == evDrainContinues {
					want = phaseDecide
				}
			case phaseNudge:
				switch ev {
				case evNudgeAgain:
					want = phaseDecide
				case evNudgeEnded:
					want = phaseExit
				}
			case phaseClose:
				switch ev {
				case evCloseAgain:
					want = phaseDecide
				case evCloseEnded:
					want = phaseExit
				}
			}
			require.Equal(t, want, transition(phase, ev), "phase %v + %v", phase, ev)
		}
	}
}

// classifyScope is the decide phase's pure mapping (doc sec.3.5), in the
// doc's order.
func TestCLILoopClassifyScope_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state agent.CLIScopeState
		want  cliScopeAction
	}{
		{"owed", agent.CLIScopeState{Drain: agent.DrainOwed}, scopeActDrain},
		{"paced", agent.CLIScopeState{Drain: agent.DrainPaced}, scopeActWaitPaced},
		{"work open", agent.CLIScopeState{WorkOpen: true}, scopeActWaitOpen},
		{"stuck", agent.CLIScopeState{Drain: agent.DrainStuck}, scopeActStuck},
		{"deferred", agent.CLIScopeState{Drain: agent.DrainDeferred}, scopeActExit},
		{"nothing owed, nothing running", agent.CLIScopeState{}, scopeActExit},
		{"paced beats work open", agent.CLIScopeState{Drain: agent.DrainPaced, WorkOpen: true}, scopeActWaitPaced},
		{"stuck beats deferred", agent.CLIScopeState{Drain: agent.DrainStuck}, scopeActStuck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, classifyScope(tc.state))
		})
	}
}

// classifyDrainOutcome is the drain phase's pure verdict on one finished
// iteration; the verdict (plus the streak) decides retry, budget give-up, or
// the run's answer.
func TestCLILoopClassifyDrainOutcome_Table(t *testing.T) {
	t.Parallel()
	res := &RunResult{}
	cases := []struct {
		name   string
		result *RunResult
		err    error
		want   drainVerdict
	}{
		{"queued", nil, ErrRunQueued, drainQueued},
		{"queued with a result", res, ErrRunQueued, drainQueued},
		{"refused", nil, agent.ErrDrainNotAttempted, drainRefused},
		{"setup failed", nil, errors.New("no such model"), drainSetupFailed},
		{"completed", res, nil, drainCompleted},
		{"awaiting answer", res, &agent.AwaitingAnswerError{}, drainCompleted},
		{"ran and failed", res, errors.New("provider 500"), drainFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, classifyDrainOutcome(tc.result, tc.err))
		})
	}
}

// Every exit path funnels through finish exactly once: one envelope on the
// output, one ended_reason on the session row, and the operator's cancel
// request cleared on the canceled paths only. The stuck path shares exit with
// the error path (its runIncompleteError carries the reason); the lock-busy
// first turn records nothing (R8A-1) and leaves the flag alone (R8A-2).
//
// Revert-check: a second finish call on any path would flush a second
// envelope (count == 2); dropping finish from a funnel would leave the row
// empty or the flag set.
func TestCLILoopExitFunnels_OneEnvelopeAndEndedReasonEach(t *testing.T) {
	cases := []struct {
		name string
		// The funnel invocation under test, on a loop whose first turn
		// produced an envelope.
		call      func(l *cliLoop) (*RunResult, error)
		refused   bool   // the lock-busy first turn: refusedByOwner
		cancelled bool   // the session row's cancel flag is set before the exit
		ctxDone   bool   // the run's ctx is cancelled before the exit
		wantEnded string // the row's ended_reason after the exit
		wantFlag  bool   // the cancel flag after the exit
	}{
		{
			name:      "clean scope close",
			call:      func(l *cliLoop) (*RunResult, error) { return l.exit(nil, "") },
			wantEnded: "done",
			wantFlag:  false,
		},
		{
			name:      "error exit",
			call:      func(l *cliLoop) (*RunResult, error) { return l.exit(errors.New("boom"), "error") },
			wantEnded: "error",
			wantFlag:  false,
		},
		{
			name: "stuck exit (incomplete error carries the reason)",
			call: func(l *cliLoop) (*RunResult, error) {
				return l.exitPrecheck(&runIncompleteError{reason: "error", detail: "stuck"})
			},
			wantEnded: "error",
			wantFlag:  false,
		},
		{
			name: "cap exit",
			call: func(l *cliLoop) (*RunResult, error) {
				return l.exitPrecheck(&runIncompleteError{reason: "error", detail: "cap"})
			},
			wantEnded: "error",
			wantFlag:  false,
		},
		{
			name:      "canceled exit clears the honoured flag",
			call:      func(l *cliLoop) (*RunResult, error) { return l.exitCanceled() },
			cancelled: true,
			ctxDone:   true,
			wantEnded: "canceled",
			wantFlag:  false,
		},
		{
			name:      "wait exit (DB unreadable) keeps the flag",
			call:      func(l *cliLoop) (*RunResult, error) { return l.exitWait(errors.New("disk on fire")) },
			cancelled: true,
			wantEnded: "error",
			wantFlag:  true,
		},
		{
			name:      "lock-busy first turn records nothing",
			call:      func(l *cliLoop) (*RunResult, error) { return l.exit(errors.New("busy"), "") },
			refused:   true,
			cancelled: true,
			wantEnded: "",
			wantFlag:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "a", "FSM", 1, 1)
			})
			if tc.cancelled {
				require.NoError(t, h.app.Sessions.RequestCancel(t.Context(), h.sessionID))
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tc.ctxDone {
				cancel()
			}
			defer cancel()
			var out bytes.Buffer
			l := &cliLoop{
				app: h.app, ctx: ctx, output: &out, mode: RunModeJSON,
				sessionID: h.sessionID, lastBuffered: &bytes.Buffer{},
				final: &RunResult{SessionID: h.sessionID},
			}
			l.refusedByOwner = tc.refused

			final, _ := tc.call(l)

			require.NotNil(t, final)
			lines := strings.Count(strings.TrimSpace(out.String()), "\n") + 1
			require.Equal(t, 1, lines, "exactly one envelope flushed")
			require.Equal(t, tc.wantEnded, sessionEndedReason(t, h.app, h.sessionID))
			pending, readErr := h.app.Sessions.IsCancelRequested(t.Context(), h.sessionID)
			require.NoError(t, readErr)
			require.Equal(t, tc.wantFlag, pending)
		})
	}
}

// Guard the compaction of the exit funnels: exit adjusts the reason, then
// every funnel lands on finish, which persists and flushes. A funnel that
// stopped calling finish would fail the table above; this test pins that
// exitPrecheck maps an incomplete error to its own reason (not always
// "error").
func TestCLILoopExitPrecheck_UsesIncompleteReason(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "FSM", 1, 1)
	})
	var out bytes.Buffer
	l := &cliLoop{
		app: h.app, ctx: t.Context(), output: &out, mode: RunModeJSON,
		sessionID: h.sessionID, lastBuffered: &bytes.Buffer{},
		final: &RunResult{SessionID: h.sessionID},
	}

	final, exitErr := l.exitPrecheck(&runIncompleteError{reason: "max_tokens", detail: "cap"})

	var inc *runIncompleteError
	require.ErrorAs(t, exitErr, &inc)
	require.Equal(t, "max_tokens", final.ExitReason)
	require.Equal(t, "max_tokens", sessionEndedReason(t, h.app, h.sessionID))
}
