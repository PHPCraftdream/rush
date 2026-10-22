package agent

// run_owned_carry_behavior_test.go: behavioral pins for runOwned's carry rule
// (agent_run.go finalTurnOutcome). The owner's Run returns the LAST REAL
// TURN's outcome; a Drain queued behind the winner's turn either
//
//   - is REFUSED as a no-turn leg (no visible batch debt: the drainNoTurn
//     early return in agent_turn.go finishes the leg with (nil, nil)) — the
//     winner's result carries, and the provider is never reached again; or
//   - COMMITS and runs a real provider turn over its visible batch debt —
//     the DRAIN turn's result becomes the Run's documented observable.
//
// The test uses the real harness (drain_attempt_fixture_test.go): a real
// sessionAgent + coordinator + httptest provider, with node-backed cutover
// facts bound to the coordinator exactly like
// batch_drain_turn_entry_test.go.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// carryFixture is the common shape: a fixture whose provider holds the
// winner's first request on a gate (so the Drain can be queued while the
// winner's turn is still in flight) and answers later requests from
// answers[i].
type carryFixture struct {
	*attemptFixture
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
	answers []string
	// winnerRes/winnerErr hold the winner Run's outcome for the assertions.
	winnerRes *fantasy.AgentResult
	winnerErr error
}

func newCarryFixture(t *testing.T, answers ...string) *carryFixture {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	c := &carryFixture{
		attemptFixture: newAttemptFixture(t, "run-owned-carry", attemptFixtureOpts{noIdle: true}),
		gate:           make(chan struct{}),
		started:        make(chan struct{}),
		answers:        answers,
	}
	// History first, so no title generation adds provider requests.
	_, err := c.env.messages.Create(context.Background(), c.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier"}},
	})
	require.NoError(t, err)
	c.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		n := int(c.requests.Load())
		if n <= 1 {
			c.once.Do(func() { close(c.started) })
			<-c.gate
		}
		textFinishResponse(w, c.answers[min(n, len(c.answers))-1])
	})
	return c
}

// bindCutoverFacts binds the given env's node-backed facts to the fixture's
// coordinator (the batch_drain_turn_entry_test.go binding).
func (c *carryFixture) bindCutoverFacts(t *testing.T, env *cutoverEnv) {
	t.Helper()
	BindCoordinatorBatchCutoverFacts(c.coord, NewBatchCutoverNodeFacts(env.store, env.live, env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(c.coord) })
}

// runWinner runs the winner's call on a goroutine, waits until its request
// reaches the provider, then hands control back with the turn parked on the
// gate.
func (c *carryFixture) runWinner() chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.winnerRes, c.winnerErr = c.sa.Run(context.Background(),
			SessionAgentCall{SessionID: c.sessID, Prompt: "winner", MaxOutputTokens: 100})
	}()
	c.waitRequest()
	select {
	case <-c.started:
	case <-time.After(10 * time.Second):
		c.t.Fatal("the winner's request never reached the handler")
	}
	return done
}

// TestRunOwnedCarryWinnerResultPastRefusedDrainBehavior: a Drain queued
// behind the winner's turn with NO visible batch debt is refused as a no-turn
// leg ((nil, nil), agent_turn.go's drainNoTurn early return): the winner's
// result carries and the refused Drain never reaches the provider.
func TestRunOwnedCarryWinnerResultPastRefusedDrainBehavior(t *testing.T) {
	c := newCarryFixture(t, "WINNER_DONE")
	// Facts bound over an EMPTY cutover DB: CaptureVisibleDebt succeeds
	// empty, so the queued Drain is refused (nil, nil), never errored.
	c.bindCutoverFacts(t, newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, &countingTool{name: "view"}))

	done := c.runWinner()
	// The winner's turn is parked on the gate: queue the Drain so the
	// end-of-turn drainOrReleaseMerged pops it.
	c.sa.getMailbox(c.sessID).queue(newDrainCall(SessionAgentCall{SessionID: c.sessID}))
	close(c.gate)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		c.t.Fatal("the winner's Run never returned")
	}
	require.NoError(c.t, c.winnerErr)
	require.NotNil(c.t, c.winnerRes, "the winner's result is the Run's outcome")
	require.Contains(c.t, c.winnerRes.Response.Content.Text(), "WINNER_DONE")
	require.EqualValues(c.t, 1, c.requests.Load(),
		"the refused Drain never reached the provider")
}
