package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// Regression (owned timeout vs a committed turn): the Drain commits its
// answer, and the run's owned deadline is noticed before ExecuteRun returns;
// classifyRunTimeout attaches a *RunTimeoutError to the committed result, and
// the loop's turnCanceled must still adopt the committed answer as the run's
// final one while keeping the timeout exit/error metadata. Before the fix the
// non-nil error made turnCanceled drop the newer committed envelope and the
// run reported the stale first-turn answer.
//
// Static trace: production entry cliLoop.drainPhase (app_run_async.go) ->
// runTurn -> ExecuteRun, whose done-case defer classifies the now-ended owned
// ctx into the timeout error; first assertion is require.ErrorAs on h.run's
// error (proves the deadline actually landed); the single production mutation
// under test is turnCanceled's adoption branch -- reverting it leaves l.final
// at the first turn's envelope, so the FinalText assertions below fail
// ("first answer" instead of "drain committed answer").
func TestRunTimeoutCommittedDrainAnswerAdopted(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "drain", "drain committed answer", 2, 2)
			return
		}
		loopText(w, "a", "first answer", 1, 1)
	})
	runCtx, cancelRun := context.WithCancelCause(t.Context())
	defer cancelRun(nil)
	h.afterFirstTurn(func() {
		// An owed reaction makes decidePhase enter the drain phase; the
		// owned deadline then lands inside the drain's ExecuteRun, between
		// its committed done result and its return: no sleeps, the cause
		// is installed synchronously at the done-case seam.
		h.seedDebt()
		executeRunDoneCaseSeam = func() {
			cancelRun(&agent.RunTimeoutCause{Duration: time.Minute, Source: "--timeout"})
		}
	})
	t.Cleanup(func() { executeRunDoneCaseSeam = nil })

	res, out, err := h.run(runCtx, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	var timeoutErr *RunTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	// The returned error is finish's reclassification (CommittedResult is
	// only meaningful on the turn-level error the loop adopted); the flag
	// itself is pinned by TestRunTimeoutCommittedFlag_Table.
	require.Equal(t, "timeout", res.ExitReason)
	require.Equal(t, "rush run --role smart --session "+h.sessionID, res.ResumeCommand)
	require.Equal(t, "drain committed answer", res.FinalText)

	var wire RunResult
	require.NoError(t, json.Unmarshal([]byte(out), &wire))
	require.Equal(t, "drain committed answer", wire.FinalText)
	require.Equal(t, "timeout", wire.ExitReason)
	require.NotEmpty(t, wire.ResumeCommand)
}

// classifyRunTimeout flags CommittedResult only for a clean invocation
// outcome: an envelope with no error. Literal expectations, independent of
// any production helper.
func TestRunTimeoutCommittedFlag_Table(t *testing.T) {
	cases := []struct {
		name   string
		final  *RunResult
		err    error
		wantOK bool
	}{
		{"committed", &RunResult{FinalText: "answer"}, nil, true},
		{"failed turn", &RunResult{FinalText: "partial"}, errors.New("provider 500"), false},
		{"no envelope", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := &agent.RunTimeoutCause{Duration: time.Minute, Source: "--timeout"}
			cancel(cause)
			defer cancel(nil)
			_, err := classifyRunTimeout(ctx, tc.final, tc.err, "sess", "smart")
			var timeoutErr *RunTimeoutError
			require.ErrorAs(t, err, &timeoutErr)
			require.Equal(t, tc.wantOK, timeoutErr.CommittedResult)
		})
	}
}

// runTimeoutCommitted admits only the committed flavour of the timeout; a
// plain interrupted-turn timeout (no envelope, or an error outcome) must never
// trade a partial result for the run's answer.
func TestRunTimeoutCommittedHelper_Table(t *testing.T) {
	require.False(t, runTimeoutCommitted(nil))
	require.False(t, runTimeoutCommitted(errors.New("boom")))
	require.False(t, runTimeoutCommitted(&RunTimeoutError{Cause: &agent.RunTimeoutCause{Duration: time.Minute, Source: "--timeout"}}))
	require.True(t, runTimeoutCommitted(&RunTimeoutError{
		Cause:           &agent.RunTimeoutCause{Duration: time.Minute, Source: "--timeout"},
		CommittedResult: true,
	}))
}
