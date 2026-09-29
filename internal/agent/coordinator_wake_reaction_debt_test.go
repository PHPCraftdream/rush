// DUR-4 reaction-debt coverage (Ф4-4 review, P1 fix): a Drain call decides
// by VISIBLE debt (delivery='done'), never by the plain reaction-debt
// predicate (which includes 'pending' rows a broken pull can never clear),
// and settle-by-failure's classification/threshold behavior. Real SQLite
// throughout (newTestAsyncJobStore).
package agent

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// failingCreateTxMessages wraps a real message.Service and makes CreateTx
// fail on demand -- CreateTx is the ONE call the driver's notice pull
// (agent_notice_pull.go) makes to insert a pulled row's history message, so
// failing it deterministically simulates a permanently broken pull without
// touching anything else a turn needs (Create/Update/List all still work).
type failingCreateTxMessages struct {
	message.Service
	fail atomic.Bool
}

func (f *failingCreateTxMessages) CreateTx(ctx context.Context, tx *sql.Tx, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if f.fail.Load() {
		return message.Message{}, errors.New("simulated permanent pull failure: CreateTx always fails")
	}
	return f.Service.CreateTx(ctx, tx, sessionID, params)
}

// wakeDebtFixture is the shared harness for this file's tests: a real
// coordinator + workLedger (real SQLite store) + a real *sessionAgent
// (probe HTTP model, OnSessionIdle wired exactly like production) driving
// one session registered as a driver (the routing shortcut every existing
// wakeSession test already uses -- drainCallFor's driver branch needs no
// cfg/model resolution).
type wakeDebtFixture struct {
	coord    *coordinator
	ledger   *workLedger
	store    *session.AsyncJobStore
	sa       *sessionAgent
	model    Model
	messages *failingCreateTxMessages
	sessID   string
	requests atomic.Int32
	srv      *httptest.Server
}

// debtVisible reads back the VISIBLE (delivery='done') reaction debt
// predicate directly from the store -- what a Drain's turn-start decision
// itself uses (doc sec.6 review fix).
func (f *wakeDebtFixture) debtVisible(t *testing.T, ctx context.Context) bool {
	t.Helper()
	visible, err := f.store.VisibleReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	return visible
}

func newWakeDebtFixture(t *testing.T, title string) *wakeDebtFixture {
	t.Helper()
	return newWakeDebtFixtureOpts(t, title, nil, true)
}

// newWakeDebtFixtureWithHandler is newWakeDebtFixture plus an optional
// onRequestBody hook invoked with each request's raw body, for tests that
// need to inspect what the provider actually received (not just how many
// times it was called).
func newWakeDebtFixtureWithHandler(t *testing.T, title string, onRequestBody func([]byte)) *wakeDebtFixture {
	t.Helper()
	return newWakeDebtFixtureOpts(t, title, onRequestBody, true)
}

// newWakeDebtFixtureNoIdleHook is newWakeDebtFixture without OnSessionIdle
// wired. A handful of tests fire two or more EXPLICIT, sequential
// wakeSession calls on the SAME session and need each to complete
// deterministically; with OnSessionIdle wired, the first call's own release
// spawns a background recheckDebtOnRelease goroutine (production's own
// automatic convergence path) that races the test's own next explicit call
// for the mailbox -- if that goroutine wins, the test's call can be queued
// and then orphaned by agent_ownership.go's abandonOwnershipWithHandoff
// (which drops an orphaned DRAIN call outright, by design: doc sec.3.4,
// "dropping an orphaned Drain here is always safe" since a later wake
// re-derives it) -- observed as the test's own explicit call silently doing
// nothing, ~25% of runs. Tests that want ONLY their own explicit calls to
// drive the outcome (not the automatic release-recheck too) use this
// variant; tests that specifically exercise the release-recheck path keep
// using the hook (newWakeDebtFixture/WithHandler).
func newWakeDebtFixtureNoIdleHook(t *testing.T, title string) *wakeDebtFixture {
	t.Helper()
	return newWakeDebtFixtureOpts(t, title, nil, false)
}

func newWakeDebtFixtureOpts(t *testing.T, title string, onRequestBody func([]byte), wireOnSessionIdle bool) *wakeDebtFixture {
	t.Helper()
	f := &wakeDebtFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if onRequestBody != nil {
			body, _ := io.ReadAll(r.Body)
			onRequestBody(body)
		}
		textFinishResponse(w, "reacted")
	}))
	t.Cleanup(f.srv.Close)
	model := newProbeModel(t, f.srv)
	f.model = model

	env := testEnv(t)
	sess, err := env.sessions.Create(context.Background(), title)
	require.NoError(t, err)
	f.sessID = sess.ID

	f.store = newTestAsyncJobStore(t)
	f.coord = &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
	f.ledger.store = f.store
	f.ledger.coord = f.coord
	f.coord.asyncJobs = f.ledger

	f.messages = &failingCreateTxMessages{Service: env.messages}
	opts := SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		Sessions: env.sessions, Messages: f.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: f.ledger,
	}
	if wireOnSessionIdle {
		opts.OnSessionIdle = f.coord.onSessionIdleHook
	}
	sa := NewSessionAgent(opts)
	f.sa = sa.(*sessionAgent)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})
	return f
}

// claimAndFinish claims toolCallID for f.sessID, announces it, and
// terminal-transitions it to 'completed' with wake=1 -- the shape a real
// async job leaves behind, delivery='pending' until something pulls it.
//
// Registers the claim through f.ledger.Start (not the store directly) so
// the in-memory ledger keeps an open job for f.sessID -- l.running stays
// true, which is what keeps recheckChild/releaseDriverIfScopeClosed from
// concluding this bare test session's scope is closed and yanking its
// driver registration out from under a later wakeSession call. The DB-side
// terminal transition still goes straight through the store (bypassing
// l.finish), so memory and DB deliberately disagree exactly the way a
// store.Transition-only completion (this fixture's whole point) would.
func (f *wakeDebtFixture) claimAndFinish(t *testing.T, ctx context.Context, toolCallID string) {
	t.Helper()
	_, existing, err := f.ledger.Start(f.sessID, toolCallID, toolCallID, "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, f.store.MarkAnnounced(ctx, f.sessID, toolCallID))
	_, err = f.store.Transition(ctx, session.TransitionParams{
		Owner: f.sessID, ToolCallID: toolCallID, State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
}

// TestDrainTurn_PermanentPullFailure_NoProviderCallEver is the P1 fix's core
// proof (doc sec.6): a pull that NEVER succeeds leaves its row stuck at
// delivery='pending' forever. Because the Drain turn-start decision now
// asks VisibleReactionDebtExists (delivery='done' only), it takes the
// no-turn branch every single time -- the provider is NEVER called, no
// matter how many times the session is re-woken (a fresh wakeSession call,
// a duplicate onSessionIdleHook release, or a RecheckPass tick).
//
// REVERT CHECK: changed decideDrainTurn (agent_drain_decision.go) back to
// call reactionDebtExists (the plain, pending-inclusive predicate) instead
// of visibleReactionDebtExists -- this test's `require.Zero(t, requests)`
// FAILED (the probe server received requests: the permanently-pending row
// still counted as debt, forcing an empty-prompt provider turn on every
// wake). Restored visibleReactionDebtExists; re-ran, passed.
func TestDrainTurn_PermanentPullFailure_NoProviderCallEver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "permanent-pull-failure")
	f.messages.fail.Store(true) // the pull's CreateTx always errors from here on
	f.claimAndFinish(t, ctx, "call-1")

	// Trigger 1: an ordinary wake (a fresh hint) -- the Drain runs, its own
	// pull fails, decideDrainTurn sees no VISIBLE debt, no provider call.
	err := f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(), "a permanently failing pull must never reach the provider")

	row, err := f.store.Get(ctx, f.sessID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "the row must still be stuck at pending -- the pull never succeeded")
	require.EqualValues(t, 0, row.Reacted)

	// Trigger 2: a duplicate/spurious release notification (e.g. a
	// supervision or delegation hook firing with nothing new to report) --
	// onSessionIdleHook's own separate-goroutine debt recheck fires
	// (nothing was consumed by rule (a) here, since THIS call is not the
	// original no-turn Drain's own release), reads the DB, finds no visible
	// debt, and does nothing.
	f.coord.onSessionIdleHook(f.sessID)
	require.Never(t, func() bool { return f.requests.Load() > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"the async recheck must correctly find no VISIBLE debt and do nothing")

	// Trigger 3: the 60s host-level pass, primed via the recheck set
	// exactly like a session-lock-busy refusal would prime it.
	f.coord.addToRecheckSet(f.sessID)
	f.coord.RecheckPass(ctx)
	require.Zero(t, f.requests.Load(), "the 60s pass must not force a provider call over permanently-pending debt either")

	row, err = f.store.Get(ctx, f.sessID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery)
}

// TestOnSessionIdleHook_RuleA_SkipsRelaunchOnlyWhenHintUnchanged pins rule
// (a) directly, at the gate itself: a no-turn Drain's own release skips the
// re-launch check ONLY while the hint counter is unchanged since that
// Drain's check; a hint bump in between makes the very next release recheck
// (and, here, find real debt and submit).
//
// REVERT CHECK: changed onSessionIdleHook's gate (supervision.go) to
// `if false && wasNoTurnDrain && hintUnchanged` -- this test's Half 1
// assertion (require.Never requests>0) FAILED ("Condition satisfied": a
// provider call happened even though the hint never moved). Restored the
// gate (byte-identical diff confirmed); re-ran, both halves passed.
func TestOnSessionIdleHook_RuleA_SkipsRelaunchOnlyWhenHintUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "rule-a-gate")

	// Seed REAL visible debt (a fully pulled, unreacted notice) so a
	// recheck that actually runs would find something and submit a Drain.
	f.claimAndFinish(t, ctx, "call-1")
	pulled, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)

	hintAtCheck := f.ledger.hintSeqOf(f.sessID)
	f.ledger.markNoTurnDrainRelease(f.sessID, hintAtCheck)

	// Half 1: hint unchanged since the marked check -- onSessionIdleHook
	// must skip the relaunch despite real debt sitting right there. The
	// skip is synchronous (the gate is checked before the recheck goroutine
	// is ever spawned), so there is nothing to race: if recheckDebtOnRelease
	// were spawned, require.Never gives it ample time to reach the probe.
	f.coord.onSessionIdleHook(f.sessID)
	require.Never(t, func() bool { return f.requests.Load() > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"rule (a) must skip the relaunch when the hint is unchanged")

	// Half 2: bump the hint (something DID happen since), mark the SAME
	// no-turn state again, then release -- this time the recheck must run
	// and find the still-open debt.
	f.ledger.bumpHint(f.sessID)
	f.ledger.markNoTurnDrainRelease(f.sessID, hintAtCheck)
	f.coord.onSessionIdleHook(f.sessID)
	require.Eventually(t, func() bool { return f.requests.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"a hint bump since the last check must make the next release recheck and submit")
}

// TestWakeSession_ExternalDriver_HintOnlyNeverBuildsDrainCall pins doc
// sec.3.4: a session claimed by an external driver (the CLI root of a live
// `rush run` process) gets a hint only -- wakeSession must never submit it a
// Drain turn, no matter how much debt exists. internal/app's
// TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON already
// pins this at the black-box level (its exact request COUNT would be wrong
// if wakeSession also raced its own Drain turn in); this is the direct,
// narrow proof at the mechanism itself.
//
// REVERT CHECK: changed wakeSession's `if c.asyncJobs.isExternalDriver(job.owner)
// { return nil }` branch to `if false { ... }` -- this test's
// `require.Zero(t, f.requests.Load())` FAILED (a Drain turn ran, the probe
// server got a request). Restored the branch; re-ran, passed.
//
// C4 fix update: f.coord is a bare, non-persistent fixture (persistentMode
// defaults false, matching `rush run`), so ReleaseExternalDriver is now
// correctly a no-op for it (coordinator_reaction_source.go's own C4 fix --
// see its doc: a non-persistent coordinator keeps its root hint-only for the
// entire life of the process). The "release restores ordinary routing" half
// this test used to assert here is still real for a PERSISTENT (web)
// coordinator -- proven with TestReleaseExternalDriver_PersistentCoordinator_StillReleases
// instead of on this same fixture.
func TestWakeSession_ExternalDriver_HintOnlyNeverBuildsDrainCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "external-driver-hint-only")
	f.claimAndFinish(t, ctx, "call-1")
	_, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	require.NoError(t, f.coord.ClaimExternalDriver(ctx, f.sessID))
	before := f.ledger.hintSeqOf(f.sessID)

	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(), "an external-driver session must never get a Drain turn, only a hint")
	require.NotEqual(t, before, f.ledger.hintSeqOf(f.sessID), "the hint must still be delivered so the loop can react")

	f.coord.ReleaseExternalDriver(ctx, f.sessID)
	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(),
		"a non-persistent coordinator must NEVER release its external-driver marker (C4 fix) -- routing must stay hint-only")
}

// TestSessionDrainPolicy_StopSuspendsUntilHumanMessage pins doc sec.3.4's
// web session-policy row: after a Stop, automatic turns are suspended until
// the next human message (ResetAutoResumeCounter) -- not merely capped, but
// actively suspended even from a fresh debt event. The "wakes as today"
// half of this row (before any Stop) is already pinned by
// TestWebAsyncJob_EndToEnd_OneNoticeOneTurn (internal/agent).
//
// REVERT CHECK: changed suspendAutoResume to a no-op (`func (c *coordinator)
// suspendAutoResume(sessionID string) {}`) -- this test's post-Stop
// `require.Zero(t, f.requests.Load())` FAILED (a turn ran despite Stop).
// Restored the real body; re-ran, passed.
func TestSessionDrainPolicy_StopSuspendsUntilHumanMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "stop-suspends-web")
	f.coord.Cancel(f.sessID) // Stop, with no prior work -- just arms the suspension

	f.claimAndFinish(t, ctx, "call-1")
	_, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(), "automatic turns must stay suspended after Stop until a human message")

	f.coord.ResetAutoResumeCounter(f.sessID) // the human-message reset path
	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.NotZero(t, f.requests.Load(), "a human message must re-arm automatic turns")
}
