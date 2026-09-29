// Settle-by-failure coverage (docs/plans/2026-09-28-async-phase4-durable-
// core.md sec.3.4/sec.6): an unrecoverable provider classification settles
// immediately; a temporary one settles only at K=3; an admission refusal
// never settles at all. Real SQLite throughout, a mock SessionAgent (no real
// HTTP) since wakeSession's classification only needs a typed error back
// from agent.Run.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// settleFixture is a real coordinator+workLedger+AsyncJobStore harness for
// settle-by-failure tests, with a mock driver whose Run outcome the test
// controls directly.
type settleFixture struct {
	coord    *coordinator
	store    *session.AsyncJobStore
	messages message.Service
	agent    *mockSessionAgent
	sessID   string
}

func newSettleFixture(t *testing.T, title string) *settleFixture {
	t.Helper()
	env := testEnv(t)
	sess, err := env.sessions.Create(context.Background(), title)
	require.NoError(t, err)

	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	f := &settleFixture{sessID: sess.ID, store: store, messages: env.messages}
	f.coord = &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), messages: env.messages}
	ledger := newWorkLedger(f.coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = f.coord
	f.coord.asyncJobs = ledger

	f.agent = &mockSessionAgent{}
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.agent, call: SessionAgentCall{SessionID: f.sessID}})

	// Seed one VISIBLE debt row (a real pull, so delivery='done').
	_, err = store.Claim(context.Background(), session.ClaimParams{
		Owner: f.sessID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(context.Background(), f.sessID, "call-1"))
	_, err = store.Transition(context.Background(), session.TransitionParams{
		Owner: f.sessID, ToolCallID: "call-1", State: "completed", ResultSummary: "boom", Wake: true,
	})
	require.NoError(t, err)
	_, err = store.PullJobNotices(context.Background(), env.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	return f
}

func (f *settleFixture) debtExists(t *testing.T) bool {
	t.Helper()
	debt, err := f.store.ReactionDebtExists(context.Background(), f.sessID)
	require.NoError(t, err)
	return debt
}

func (f *settleFixture) markerCount(t *testing.T) int {
	t.Helper()
	notices, err := f.store.ListSessionNotices(context.Background(), f.sessID)
	require.NoError(t, err)
	n := 0
	for _, notice := range notices {
		if notice.Kind == "wake_failed" {
			n++
		}
	}
	return n
}

// TestSettleByFailure_QuotaMarkerSettlesImmediately: a 401/quota-shaped
// provider error is unrecoverable (classifyProviderError's classTerminal) --
// debt closes on the FIRST failed pass, K does not matter, exactly one
// marker.
//
// REVERT CHECK: changed settleOrRetryDrainFailure's classTerminal branch to
// call incrementThenSettleIfThreshold instead of settleAndMark directly --
// this test FAILED (debt still existed, zero markers after one failure).
// Restored the direct settleAndMark call; re-ran, passed.
func TestSettleByFailure_QuotaMarkerSettlesImmediately(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "quota-settle")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "invalid api key"}
	}
	require.True(t, f.debtExists(t), "precondition: debt must be visible before the failing wake")

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)

	require.False(t, f.debtExists(t), "a 401 must close the debt on the first failed pass")
	require.Equal(t, 1, f.markerCount(t), "exactly one wake-failed marker")

	job, err := f.store.Get(context.Background(), f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Reacted)
	require.EqualValues(t, 1, job.ReactedFailed)
}

// TestSettleByFailure_OneTransientFailureDoesNotSettle pins doc sec.3.4/6: a
// single transient (classTransient) failure keeps the debt -- "a one-minute
// provider outage does not close the debt". No marker either.
//
// REVERT CHECK: changed incrementThenSettleIfThreshold's
// `if attempts < drainFailureSettleThreshold { return }` to always fall
// through to settleAndMark -- this test FAILED (debt closed, one marker
// after a single transient failure). Restored the threshold check; re-ran,
// passed.
func TestSettleByFailure_OneTransientFailureDoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "transient-outage")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "temporarily overloaded"}
	}

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)

	require.True(t, f.debtExists(t), "a single transient failure must keep the debt")
	require.Zero(t, f.markerCount(t))

	job, err := f.store.Get(context.Background(), f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, job.WakeAttempts)
	require.EqualValues(t, 0, job.Reacted)
}

// TestSettleByFailure_KThreeTemporaryFailuresSettleOnce pins the K=3 bound
// (doc sec.3.4/6): the first two transient failures keep the debt with no
// marker; the third settles it with EXACTLY one marker -- no chain of
// markers, no chain of turns beyond the three attempts driven here.
//
// REVERT CHECK: changed drainFailureSettleThreshold from 3 to 4 --
// this test's post-3rd-failure assertions FAILED (debt still open, zero
// markers). Restored 3; re-ran, passed.
func TestSettleByFailure_KThreeTemporaryFailuresSettleOnce(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "k-three")
	failing := func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "overloaded"}
	}
	f.agent.runFunc = failing

	for i := 1; i <= 2; i++ {
		err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
		require.Error(t, err, "attempt %d", i)
		require.True(t, f.debtExists(t), "attempt %d: debt must survive under K=3", i)
		require.Zero(t, f.markerCount(t), "attempt %d: no marker before K=3", i)
	}

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)
	require.False(t, f.debtExists(t), "the 3rd failed pass must close the debt")
	require.Equal(t, 1, f.markerCount(t), "exactly one marker at K=3, not a chain")

	job, err := f.store.Get(context.Background(), f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 3, job.WakeAttempts)
	require.EqualValues(t, 1, job.ReactedFailed)

	// A FOURTH wake, after settlement, must find nothing left to act on
	// (settleOrRetryDrainFailure's snapshot.Empty() guard) -- no second
	// marker, no further churn.
	err = f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)
	require.Equal(t, 1, f.markerCount(t), "settlement must not repeat once the debt is already closed")
}

// TestSettleByFailure_AdmissionRefusalDuringShutdown_DoesNotSettle pins doc
// sec.3.4: ErrAgentShuttingDown is an admission refusal, never a turn
// failure -- the debt is untouched (no increment, no settle, no marker),
// and the session goes into the 60s recheck set instead.
//
// REVERT CHECK: removed the `if turnAttemptRefused(runErr) { ...; return
// runErr }` branch from wakeSession (coordinator_wake.go), letting shutdown
// refusals fall into settleOrRetryDrainFailure -- this test FAILED (a
// wake_failed marker appeared, wake_attempts incremented, for a mere
// shutdown refusal). Restored the branch; re-ran, passed.
func TestSettleByFailure_AdmissionRefusalDuringShutdown_DoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "shutdown-refusal")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, ErrAgentShuttingDown
	}

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.ErrorIs(t, err, ErrAgentShuttingDown)

	require.True(t, f.debtExists(t), "an admission refusal must never close the debt")
	require.Zero(t, f.markerCount(t))
	job, err := f.store.Get(context.Background(), f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 0, job.WakeAttempts, "a refusal is not a counted failed pass")

	f.coord.recheckMu.Lock()
	_, inSet := f.coord.recheckSet[f.sessID]
	f.coord.recheckMu.Unlock()
	require.True(t, inSet, "a session-lock/shutdown refusal must go into the 60s recheck set, not be forgotten")
}

// TestSettleByFailure_SnapshotAlreadyReactedBeforeSettle_NoSpuriousMarker is
// W-DRAIN item 1's "queued user turn behind a Drain that did react" scenario
// (docs/reviews/2026-09-29-async-phase4-round1.md B5/C3): by the time
// settleAndMark actually runs, every row the pre-captured snapshot named has
// ALREADY been reacted to by something else (in production: a queued user
// turn dispatched behind this Drain in the same release window, whose own
// step-finish reacted before this Drain's failure was even classified).
// SettleReactedFailedWithMarker's server-side wake=1/reacted=0 scoping means
// settling this snapshot now touches zero rows -- so NO marker may appear.
//
// Revert-check performed: reverted settleAndMark (coordinator_drain_policy.go)
// to the pre-item-1 two-step SettleReactedFailed + persistWakeFailedMarker
// sequence -- this test's `require.Zero(t, f.markerCount(t))` FAILED (one
// spurious wake_failed marker appeared even though SettleReactedFailed
// itself settled zero rows). Restored the atomic
// SettleReactedFailedWithMarker call; re-ran, passed.
func TestSettleByFailure_SnapshotAlreadyReactedBeforeSettle_NoSpuriousMarker(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "reacted-before-settle")
	f.agent.runFunc = func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		// Simulate the queued user turn reacting to the SAME rows this
		// Drain attempt's snapshot already captured, before this attempt's
		// own failure is classified.
		reactionMsg, err := f.messages.Create(ctx, f.sessID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "queued turn's own reply"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		require.NoError(t, f.coord.asyncJobs.store.MarkReactedWithMessageUpdate(ctx, f.messages, f.sessID, reactionMsg))
		return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "invalid api key"}
	}
	require.True(t, f.debtExists(t), "precondition: debt must be visible before the wake")

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)

	require.False(t, f.debtExists(t), "the queued turn's own reaction already cleared the debt")
	require.Zero(t, f.markerCount(t), "nothing was actually settled by THIS attempt -- no marker may appear")
}

// TestSettleByFailure_NonProviderErrorWithNoAttemptEvidence_DoesNotSettle is
// W-DRAIN item 1's "DB error in the preamble" scenario: a Drain's turn-start
// pull (or any other preamble step) fails with a plain error that is
// neither a *fantasy.ProviderError nor a net.Error nor context.Canceled/
// DeadlineExceeded -- classifyProviderError's own fallback would call this
// classTerminal (its default case), which, before this fix, closed the
// debt and wrote a visible wake-failed marker for a failure that never
// touched the provider at all.
//
// Revert-check performed: removed the `else if !isProviderClassifiable(runErr)`
// branch from settleOrRetryDrainFailure (coordinator_drain_policy.go),
// falling straight through to classifyProviderError -- this test's
// `require.True(t, f.debtExists(t))` FAILED (debt closed, one wake_failed
// marker written for a bare DB-shaped error). Restored the branch; re-ran,
// passed.
func TestSettleByFailure_NonProviderErrorWithNoAttemptEvidence_DoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "db-error-preamble")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, fmt.Errorf("pull notices: %w", errors.New("database is locked"))
	}
	require.True(t, f.debtExists(t), "precondition: debt must be visible before the wake")

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)

	require.True(t, f.debtExists(t), "a non-provider preamble error must not settle the debt")
	require.Zero(t, f.markerCount(t))
	f.coord.recheckMu.Lock()
	_, inSet := f.coord.recheckSet[f.sessID]
	f.coord.recheckMu.Unlock()
	require.True(t, inSet, "a non-provider preamble error must go into the 60s recheck set, not be forgotten")
}

// TestSettleAndMark_MarkerText_RealIDIncludedPseudoIDOmitted is item 1's
// marker-text proof: a real, model-facing tool_call_id (an ordinary async-
// job wake) is useful context and stays in the marker text; wakeSession's
// own internal pseudo-ids (release-recheck, cli-loop, recheck-pass,
// supervision-<uuid> -- jobIdentity.toolCallID is diagnostic-only, never a
// real id for those paths) must never leak into it.
//
// Revert-check performed: changed isPseudoJobID (coordinator_drain_policy.go)
// to always return true -- the real-id half of this test
// (`require.Contains(t, marker.Text, "call-1")`) FAILED. Changed it to always
// return false -- the pseudo-id half (`require.NotContains(t, marker.Text,
// "release-recheck")`) FAILED. Restored the real switch/prefix check; both
// halves passed.
func TestSettleAndMark_MarkerText_RealIDIncludedPseudoIDOmitted(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "marker-text-real-id")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "invalid api key"}
	}
	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)
	require.Equal(t, 1, f.markerCount(t))
	notices, listErr := f.store.ListSessionNotices(context.Background(), f.sessID)
	require.NoError(t, listErr)
	var markerText string
	for _, n := range notices {
		if n.Kind == "wake_failed" {
			markerText = n.Text
		}
	}
	require.Contains(t, markerText, "call-1", "a real tool_call_id is useful context and must stay in the marker")

	f2 := newSettleFixture(t, "marker-text-pseudo-id")
	f2.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "invalid api key"}
	}
	err = f2.coord.wakeSession(context.Background(), jobIdentity{owner: f2.sessID, toolCallID: "release-recheck"}, true)
	require.Error(t, err)
	require.Equal(t, 1, f2.markerCount(t))
	notices2, listErr := f2.store.ListSessionNotices(context.Background(), f2.sessID)
	require.NoError(t, listErr)
	var markerText2 string
	for _, n := range notices2 {
		if n.Kind == "wake_failed" {
			markerText2 = n.Text
		}
	}
	require.NotContains(t, markerText2, "release-recheck", "an internal pseudo-id must never leak into a notice the model/operator reads")
}
