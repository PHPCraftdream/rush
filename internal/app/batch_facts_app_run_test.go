// App-run regression for the R12-H fact-source resolution (item e): the
// binding test (batch_facts_binding_test.go) calls CLIScope directly and the
// terminal tests (batch_facts_terminal_test.go) inject a scripted sentinel,
// so neither can see breakage between the REAL `rush run` loop entry, the
// production fact-source resolution on the coordinator and the final typed
// error. These two tests drive the same production path the loop fixtures
// use (RunNonInteractiveWithResult -> runNonInteractiveWithAsyncResults ->
// cliLoop -> nextStep -> source.CLIScope).
//
// The loop's fact source is NOT a test seam: it is type-asserted from
// app.AgentCoordinator at the top of every run (app_run_async.go:286
// `source, isReactionSource := app.AgentCoordinator.(agent.ReactionDebtSource)`),
// so in the unavailable test below the production coordinator is wrapped in
// a test-local countingScopeSource decorator (embedding the real coordinator
// and overriding only CLIScope to count calls, then delegating) and assigned
// back to app.AgentCoordinator before the run -- the same swap shape the
// existing fakes use (app_run_round4_misc_test.go, coordinatorWithoutOptionalInterfaces).
//
// No sleeps: the shrunk retry bounds (below) and the between-turns seam
// (cliLoopTurnDoneSeam via h.afterFirstTurn) are the only synchronization.
// No t.Parallel(): shared package binding state.
package app

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// countingScopeSource wraps the production coordinator for the unavailable
// test: it counts CLIScope calls and delegates every call (including the
// retries) to the real coordinator, so the retry path runs against the REAL
// unavailable fact source. It embeds both agent.Coordinator and
// agent.ReactionDebtSource (disjoint method sets) so the run entry's
// type assertion at app_run_async.go:286 keeps passing; optional
// interfaces beyond those two (WakeScheduleController, RateLimitNotifier,
// WakeSchedulerNotify) are intentionally absent -- this test runs no wake
// schedules and no rate-limited waits, so the failed optional assertions
// are behavior-neutral.
type countingScopeSource struct {
	agent.Coordinator
	agent.ReactionDebtSource

	mu    sync.Mutex
	calls int
}

func (c *countingScopeSource) CLIScope(ctx context.Context, sessionID string) (agent.CLIScopeState, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.ReactionDebtSource.CLIScope(ctx, sessionID)
}

func (c *countingScopeSource) scopeCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestAppRun_BoundFactSource_CompletesEndToEnd runs one plain-text turn
// through the production run loop on a flag-ON app, then asserts the FINAL
// result of the whole invocation.
//
// The node-backed facts binding is NOT installed here by the test: the
// harness construction (newLoopHarness ->
// newLockBusyCLITestApp -> config.Init/app.New) already fires the PRODUCTION
// binding (app_agent_setup.go, BindCoordinatorBatchCutoverFacts with the
// node-backed source) inside New.
//
// Revert-check: deleting the production binding block leaves the
// coordinator on the conservative unknownBatchCutoverFacts fail-safe, whose
// LiveScopeForOwners returns entries with agent.ErrBatchFactsSourceUnavailable
// (batch_cutover_facts.go:146-152) -> runningWorkOpen errors
// (coordinator_reaction_source.go:320-327) -> CLIScope fails -> the loop
// exits with the "batch fact source unavailable" terminal error and the
// NoError/final-text oracles below go red. (The fail-safe's
// SelectedDebtExists returns (true, nil), but the loop's scope read hits the
// LIVE read first, which is the one that fails.)
func TestAppRun_BoundFactSource_CompletesEndToEnd(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 1, 1)
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err, "a closed scope over the production-bound fact source must complete the run")
	require.NotNil(t, res)
	require.Equal(t, h.sessionID, res.SessionID)
	require.Equal(t, "end_turn", res.ExitReason, "the clean first turn is the run's exit")
	require.Equal(t, "first answer", res.FinalText, "the bound facts must not swallow the turn's answer")
	require.EqualValues(t, 1, h.requests.Load(), "a closed scope ends the loop after the first turn; no Drain")
}

// TestAppRun_UnavailableFactSource_TypedErrorEndToEnd runs the SAME
// production loop, but swaps the bound node-backed facts for one whose node
// store dependency is missing -- inside the between-turns seam, so the first
// turn completes and the LOOP's scope read is the first unavailable read
// (deterministic barrier, no sleeps).
//
// No healthy pre-binding is installed: the loop's first turn (phaseFirst)
// runs BEFORE the first scope read (phaseDecide -> nextStep ->
// source.CLIScope, app_run_async.go:557-580), so the first turn never
// touches the fact source and only the afterFirstTurn rebind to
// NewBatchCutoverNodeFacts(nil, nil, nil) -- the production
// missing-dependency condition the typed sentinel marks -- is needed.
//
// The retry bound is shrunk (cliDBErrorRetryOverallLimit and cliDBRetryPause
// are vars exactly so a test can shrink them, app_run_async.go:29/773, same
// override pattern as TestRunNonInteractive_FirstTurnSessionLockBusy_FailsFast)
// so that if the terminal classification is lost the run still fails fast
// instead of burning the production 30s bound.
//
// Two oracles catch the mutation "delete the errors.Is branch at
// app_run_async.go:792":
//   - the counting source: with the branch intact the FIRST unavailable
//     scope read exits the loop, so scopeCalls is exactly 1; with the branch
//     gone the loop takes the retry path (:796-808), so scopeCalls is >= 2;
//   - the error text: :794 wraps the sentinel with
//     "rush run: batch fact source unavailable:" while the retry exhaustion
//     at :803 returns the raw unavailable error unwrapped, so the Contains
//     assertion below goes red under the mutation. ErrorIs does NOT: the raw
//     error still wraps the same sentinel, so it only proves typed
//     propagation, not the classification branch.
func TestAppRun_UnavailableFactSource_TypedErrorEndToEnd(t *testing.T) {
	oldLimit := cliDBErrorRetryOverallLimit
	cliDBErrorRetryOverallLimit = 250 * time.Millisecond
	t.Cleanup(func() { cliDBErrorRetryOverallLimit = oldLimit })
	oldPause := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = oldPause })

	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 1, 1)
	})

	// Wrap the production coordinator (which satisfies agent.ReactionDebtSource,
	// batch_facts_binding_test.go:82-84) so the loop's scope reads are
	// counted while delegating to the real bound facts. The production
	// binding itself fired during harness construction, keyed on the real
	// coordinator -- the wrapper delegates CLIScope to it.
	real := h.app.AgentCoordinator
	rs, ok := real.(agent.ReactionDebtSource)
	require.True(t, ok, "the production coordinator must implement agent.ReactionDebtSource")
	counting := &countingScopeSource{Coordinator: real, ReactionDebtSource: rs}
	h.app.AgentCoordinator = counting

	// After the first turn's ExecuteRun returned and BEFORE the loop
	// decides: rebind the REAL coordinator (the one the wrapper delegates
	// to) to a fact source whose node store is missing -- the production
	// missing-dependency condition the typed sentinel marks.
	// BindCoordinatorBatchCutoverFacts replaces the previous binding
	// atomically (a sync.Map store on the same key).
	h.afterFirstTurn(func() {
		agent.BindCoordinatorBatchCutoverFacts(real,
			agent.NewBatchCutoverNodeFacts(nil, nil, nil))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err, "an unavailable fact source must fail the run terminally")
	require.ErrorIs(t, err, agent.ErrBatchFactsSourceUnavailable,
		"the typed sentinel must propagate out of the run entry end to end")
	require.Equal(t, 1, counting.scopeCalls(),
		"the terminal classification must exit on the FIRST unavailable scope read: no retry (a mutation that deletes the errors.Is branch retries, yielding >= 2 calls)")
	// exitWait keeps the first turn's envelope but reclassifies the exit:
	// the result is no longer a clean "end_turn".
	require.NotNil(t, res)
	require.Equal(t, "error", res.ExitReason, "a terminal fact-source failure is an error exit, never stop")
	require.Contains(t, res.Error, "batch fact source unavailable",
		"the final envelope carries the terminal fact-source failure (the retry-exhaustion path at app_run_async.go:803 returns the raw error WITHOUT this wrapper, so this catches the errors.Is-branch deletion; ErrorIs alone would not)")
}
