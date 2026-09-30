// A transport timeout is a provider failure, not an operator stop (R3B-1): in
// Go 1.26 net/http timeout errors satisfy errors.Is(err,
// context.DeadlineExceeded), so the exemption is decided from the TURN's own
// context. Real SQLite, real *sessionAgent, an httptest server that stalls
// past the provider client's Client.Timeout.
package agent

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// stallUntilClientGone serves no response at all until the client gives up.
func stallUntilClientGone(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// lastAssistantFinishTitle is the finish title of the session's newest
// finished assistant message.
func (f *attemptFixture) lastAssistantFinishTitle(ctx context.Context) string {
	f.t.Helper()
	msgs, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(f.t, err)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Assistant {
			continue
		}
		if fp := msgs[i].FinishPart(); fp != nil {
			return fp.Message
		}
	}
	f.t.Fatal("no finished assistant message")
	return ""
}

// A black-holed provider: each Drain attempt is counted, paced behind the
// release hook, and K=3 closes the debt with one marker. The persisted finish
// is a provider error, not the "--timeout" text.
//
// Revert-check: deciding the exemption from errors.Is(err,
// context.DeadlineExceeded) alone (operatorStop ignoring turnCtxDone) exempts
// the attempt: wake_attempts stays 0 and this test goes red.
func TestDrainAttempt_TransportTimeoutIsCountedPacedAndSettledAtK(t *testing.T) {
	ctx := context.Background()
	const retry = 300 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "attempt-transport-timeout", attemptFixtureOpts{
		handler: stallUntilClientGone, clientTimeout: 150 * time.Millisecond,
	})
	f.seedDebt(ctx, "call-1", false)

	err := f.wake(ctx, true)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the premise: a net/http timeout satisfies errors.Is(context.DeadlineExceeded)")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.WakeAttempts, "a transport timeout is a counted attempt")
	require.EqualValues(t, 0, row.Reacted)
	require.Equal(t, "Provider Error", f.lastAssistantFinishTitle(ctx), "not the --timeout text")
	require.Equal(t, drainPaced, f.coord.drainPermitted(ctx, f.sessID).kind, "paced behind the failure")
	require.True(t, f.inRecheckSet())

	requests := f.requests.Load()
	time.Sleep(retry / 2)
	require.Equal(t, requests, f.requests.Load(), "the release of a failed attempt must not relaunch a paid turn")

	for want := int64(2); want <= 3; want++ {
		time.Sleep(retry)
		f.pass(ctx)
		require.Equal(t, want, f.row(ctx, "call-1").WakeAttempts, "the gate reopens for one attempt per pause")
	}
	row = f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.Reacted, "K=3 closes the debt")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx), "one marker on settle")
}

// The operator's own deadline (`rush run --timeout`) stays exempt: the turn's
// context is past its deadline, nothing is counted and the finish says so.
//
// Revert-check: treating every deadline error as a provider failure
// (turnCtxDone never set) counts the attempt and this test goes red.
func TestDrainAttempt_RunDeadlineIsExemptAndNamedAsTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-run-deadline", attemptFixtureOpts{noIdle: true, handler: stallUntilClientGone})
	f.seedDebt(ctx, "call-1", false)

	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err := f.drainRun(runCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	require.EqualValues(t, 0, f.row(ctx, "call-1").WakeAttempts, "the operator's deadline is not evidence about the debt")
	require.Equal(t, "Run timeout exceeded", f.lastAssistantFinishTitle(ctx))
	require.Zero(t, f.markers(ctx))
}

// An ordinary turn that ends in a transport timeout paces the gate like any
// other provider failure: no paid Drain right behind it.
//
// Revert-check: providerTurnFailed treating every deadline error as an
// operator stop leaves the gate open and this test goes red.
func TestOrdinaryTurn_TransportTimeoutPacesTheGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-ordinary-timeout", attemptFixtureOpts{
		noIdle: true, handler: stallUntilClientGone, clientTimeout: 150 * time.Millisecond,
	})

	_, err := f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "hello"})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	open, _, _ := f.ledger.drainGateOpen(f.sessID, time.Now())
	require.False(t, open, "a failed user turn paces the Drain gate")
}
