// App-run regression for the R12-H fact-source resolution (item e): the
// binding test (batch_facts_binding_test.go) calls CLIScope directly and the
// terminal tests (batch_facts_terminal_test.go) inject a scripted sentinel,
// so neither can see breakage between the REAL `rush run` loop entry, the
// production fact-source resolution on the coordinator and the final typed
// error. These two tests drive the same production path the loop fixtures
// use (RunNonInteractiveWithResult -> runNonInteractiveWithAsyncResults ->
// cliLoop -> nextStep -> source.CLIScope):
//
//   - bound fact source: the node-backed facts bound beside the batch
//     runtime answer the loop's scope read, the scope is closed, and the
//     run completes end to end with the first turn's final result;
//   - unavailable fact source: a node-backed source whose store dependency
//     is missing (NewBatchCutoverNodeFacts(nil, nil, nil), the exact
//     "missing node-source dependency" condition of
//     agent.ErrBatchFactsSourceUnavailable) makes the loop give up
//     terminally on the FIRST scope read -- the typed sentinel comes out of
//     the run entry and the provider is never asked for a retry turn.
//
// No sleeps: the loop fixtures' provider-counting barrier and the
// between-turns seam (cliLoopTurnDoneSeam) are the only synchronization.
// No t.Parallel(): t.Setenv and shared package binding state.
package app

import (
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestAppRun_BoundFactSource_CompletesEndToEnd runs one plain-text turn
// through the production run loop on a flag-ON app whose coordinator is
// bound to the node-backed fact source (the R12-H wiring shape), then
// asserts the FINAL result of the whole invocation.
//
// Revert-check: dropping the binding (or making the loop resolve its source
// through the conservative unknownBatchCutoverFacts fail-safe) turns the
// scope read into agent.ErrBatchFactsSourceUnavailable -- the loop exits
// with the "batch fact source unavailable" error and the NoError/final-text
// oracles below go red.
func TestAppRun_BoundFactSource_CompletesEndToEnd(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 1, 1)
	})

	// The production binding shape (app_agent_setup.go / the neighbouring
	// fixtures): node-backed facts over the App's own batch writer and live
	// reader, bound onto the App's real coordinator.
	cutoverWriter := h.app.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(h.app.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(cutoverWriter),
			h.app.batchLiveReader, cutoverWriter))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err, "a closed scope over a bound fact source must complete the run")
	require.NotNil(t, res)
	require.Equal(t, h.sessionID, res.SessionID)
	require.Equal(t, "stop", res.ExitReason, "the clean first turn is the run's exit")
	require.Equal(t, "first answer", res.FinalText, "the bound facts must not swallow the turn's answer")
	require.EqualValues(t, 1, h.requests.Load(), "a closed scope ends the loop after the first turn; no Drain")
}

// TestAppRun_UnavailableFactSource_TypedErrorEndToEnd runs the SAME
// production loop, but swaps the bound node-backed facts for one whose node
// store dependency is missing -- inside the between-turns seam, so the first
// turn completes against the healthy facts and the LOOP's scope read is the
// first unavailable read (deterministic barrier, no sleeps). The typed
// sentinel must come out of the run entry terminally: no retry turn is ever
// requested from the provider.
//
// Revert-check: losing the terminal classification in the loop
// (app_run_async.go's errors.Is(ErrBatchFactsSourceUnavailable) branch)
// turns the sentinel into an ordinary retryable DB error -- the run retries
// for the full DB bound, the request-count oracle below exceeds 1 (or the
// run outlives its context), and the typed-error oracle goes red.
func TestAppRun_UnavailableFactSource_TypedErrorEndToEnd(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 1, 1)
	})

	// Healthy facts for the first turn (the run must reach the loop's
	// decision point, not die inside the turn).
	cutoverWriter := h.app.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(h.app.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(cutoverWriter),
			h.app.batchLiveReader, cutoverWriter))

	// After the first turn's ExecuteRun returned and BEFORE the loop
	// decides: rebind the SAME coordinator to a fact source whose node
	// store is missing -- the production missing-dependency condition the
	// typed sentinel marks. BindCoordinatorBatchCutoverFacts replaces the
	// previous binding atomically (a sync.Map store on the same key).
	h.afterFirstTurn(func() {
		agent.BindCoordinatorBatchCutoverFacts(h.app.AgentCoordinator,
			agent.NewBatchCutoverNodeFacts(nil, nil, nil))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err, "an unavailable fact source must fail the run terminally")
	require.ErrorIs(t, err, agent.ErrBatchFactsSourceUnavailable,
		"the typed sentinel must propagate out of the run entry end to end")
	require.EqualValues(t, 1, h.requests.Load(),
		"the terminal error must not be retried: no second provider turn")
	// exitWait keeps the first turn's envelope but reclassifies the exit:
	// the result is no longer a clean "stop".
	require.NotNil(t, res)
	require.Equal(t, "error", res.ExitReason, "a terminal fact-source failure is an error exit, never stop")
	require.Contains(t, res.Error, "batch fact source unavailable",
		"the final envelope carries the terminal fact-source failure")
}
