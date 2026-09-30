// A Drain leg an operator cuts off while handing the session a message
// (interrupt-and-send, Stop/Cancel with a queued human message) is an operator
// stop (R3B-3): runTurn returns the next call with a nil error, so the leg is
// accounted from the error agent.Stream returned. Nothing is counted and the
// gate the human message just reopened stays open. Real SQLite, real
// *sessionAgent, httptest provider.
package agent

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cutOffDrainWithHandover runs a Drain against a provider that stalls, hands
// the session a human message through handover (while the Drain is in flight),
// and asserts the Drain leg was not charged: no attempt on the row, gate open,
// and the human turn reached the provider.
func cutOffDrainWithHandover(t *testing.T, title string, handover func(f *attemptFixture, human SessionAgentCall)) {
	t.Helper()
	ctx := context.Background()
	f := newAttemptFixture(t, title, attemptFixtureOpts{noIdle: true, handler: stalledStreamResponse})
	f.seedDebt(ctx, "call-1", false)

	done := make(chan error, 1)
	go func() { _, err := f.drainRun(ctx); done <- err }()
	require.Eventually(t, func() bool { return f.requests.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) { textFinishResponse(w, "answered the human") })

	handover(f, SessionAgentCall{SessionID: f.sessID, Prompt: "human message"})
	select {
	case err := <-done:
		require.NoError(t, err, "the human turn is the loop's next leg and completes")
	case <-time.After(20 * time.Second):
		t.Fatal("the loop never ran the human turn")
	}

	require.EqualValues(t, 2, f.requests.Load(), "the human message reached the provider")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.WakeAttempts, "a Drain cut off for a human message is not a paid attempt")
	open, _, _ := f.ledger.drainGateOpen(f.sessID, time.Now())
	require.True(t, open, "the gate the human message reopened stays open")
	require.Zero(t, f.markers(ctx))
}

// Revert-check: accounting the leg from the loop's nil error alone (dropping
// drainAttempt.streamErr from failure()) counts the attempt and paces the
// gate: this test goes red.
func TestDrainAttempt_InterruptAndSendIsNotChargedToTheDrain(t *testing.T) {
	t.Parallel()
	cutOffDrainWithHandover(t, "attempt-interrupt-send", func(f *attemptFixture, human SessionAgentCall) {
		require.True(t, f.sa.InterruptAndReplace(f.sessID, human), "the interrupt reaches the live Drain")
	})
}

// A human message queued behind the Drain and a Stop/Cancel of the live turn.
//
// Revert-check: as above.
func TestDrainAttempt_CancelWithQueuedMessageIsNotChargedToTheDrain(t *testing.T) {
	t.Parallel()
	cutOffDrainWithHandover(t, "attempt-cancel-queued", func(f *attemptFixture, human SessionAgentCall) {
		res, err := f.sa.Run(context.Background(), human)
		require.NoError(t, err)
		require.Nil(t, res, "the human call queues behind the running Drain")
		f.sa.Cancel(f.sessID)
	})
}
