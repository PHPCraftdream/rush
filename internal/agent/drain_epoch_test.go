package agent

// CI-only tests for the gh#11a epoch-terminal classification of drain-launch
// refusals: an unavailable process-bound disk binding cannot come back on a
// timer, so neither the wake path nor the interrupt-inject ticker may keep
// re-arming a paced retry against it.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// countingFenceStore counts LoadSessionExecutionRequirement calls so a test
// can prove a wake reads the fence exactly once and a timed pass over an
// EMPTY set reads it zero times (the no-periodic-retry guarantee).
type countingFenceStore struct {
	fakeFenceStore
	loads atomic.Int64
}

func (f *countingFenceStore) LoadSessionExecutionRequirement(ctx context.Context, sessionID string) (*session.SessionExecutionRequirement, error) {
	f.loads.Add(1)
	return f.fakeFenceStore.LoadSessionExecutionRequirement(ctx, sessionID)
}

// wireLostBindingFence installs the restart-state shape on the fixture's
// coordinator: a fence stored under f.sessID naming a binding the EMPTY
// manager no longer knows (the process restarted and nothing re-registered
// the provider). Shared by both flag-off and flag-on subtests.
func wireLostBindingFence(t *testing.T, f *attemptFixture) *countingFenceStore {
	t.Helper()
	store := &countingFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(context.Background(), f.sessID,
		sessionRequirementForBinding("lost-with-the-first-process")))
	f.coord.execReqs = store
	f.coord.diskCaps = newDiskCapabilityManager() // empty manager = restart state
	return store
}

// assertBindingLossWake drives one wakeSession against the lost binding and
// pins the gh#11a routing: the drainCallFor refusal typed
// ErrDiskProviderUnavailable must clear the session from the recheck set and
// NOT let noteDrainRefused re-add it; it must leave the durable debt
// untouched (never counted, never settled) and read the fence exactly
// wantLoads times.
func assertBindingLossWake(t *testing.T, f *attemptFixture, store *countingFenceStore, ctx context.Context, wantLoads int64) {
	t.Helper()
	err := f.coord.wakeSession(ctx, f.sessID, false)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)

	// Terminal for this epoch: the timed pass must not re-arm for this
	// session.
	set := f.coord.drainRecheckSet()
	require.Empty(t, set, "a binding-loss refusal is terminal for this epoch; the timed recheck pass must not re-spin it")

	// The durable debt is preserved untouched: nothing counted, nothing settled.
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.Reacted)
	require.EqualValues(t, 0, row.ReactedFailed)
	require.Zero(t, f.markers(ctx))

	require.EqualValues(t, wantLoads, store.loads.Load(),
		"the single launch reads the fence exactly once")
}

// assertNoPeriodicReArm is the STRENGTHENED no-re-arm oracle shared by both
// subtests. The mutation each must catch: reverting handleDrainBuildError to
// the old clear-then-noteDrainRefused shape, which re-adds non-external
// sessions to the recheck set at drain_attempt.go:549-551. Under the
// reverted old code the set contains f.sessID, RecheckPass launches a
// recheck wake, drainCallFor loads the fence again -> loads==2 -> FAILS.
func assertNoPeriodicReArm(t *testing.T, f *attemptFixture, store *countingFenceStore, ctx context.Context) {
	t.Helper()
	// A timed pass over an EMPTY set must launch NOTHING: the fence is not
	// re-read, the set stays empty.
	f.coord.RecheckPass(ctx)
	require.EqualValues(t, 1, store.loads.Load(),
		"the timed pass over an empty recheck set must not touch the fence")
	require.Empty(t, f.coord.drainRecheckSet(),
		"the timed pass over an empty recheck set must not re-arm the refused session")

	// A FRESH wake re-reads once: that is the launch predicate working, not
	// a retry spin.
	err := f.coord.wakeSession(ctx, f.sessID, false)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
	require.EqualValues(t, 2, store.loads.Load(),
		"a fresh wake re-reads the fence once (launch predicate, not a retry spin)")
	require.Empty(t, f.coord.drainRecheckSet(), "the fresh refusal is terminal again")
}

// TestWakeSession_BindingLossIsTerminalForEpoch runs the gh#11a routing in
// BOTH CI modes (the RUSH_BATCH_RUNTIME flag is process-global, so the
// subtests are sequential and never t.Parallel): flag-off (the launch
// decision reads async_jobs) and flag-on shadow (the decision reads batch
// node facts; without a bound facts source unknownBatchCutoverFacts
// .SelectedDebtExists errors and wakeSession takes the launch-decision error
// branch BEFORE reaching drainCallFor -- so the binding refusal would never
// be exercised with the wrong error, hence the ON twin).
func TestWakeSession_BindingLossIsTerminalForEpoch(t *testing.T) {
	ctx := context.Background()

	t.Run("flag-off", func(t *testing.T) {
		// TestMain/default is off; set explicitly for determinism
		// (mirrors batch_gate_refusal_test.go:86).
		t.Setenv("RUSH_BATCH_RUNTIME", "")
		f := newAttemptFixture(t, "wake-binding-loss-epoch", attemptFixtureOpts{
			noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
		})
		store := wireLostBindingFence(t, f)
		f.coord.addToRecheckSet(f.sessID)
		f.seedDebt(ctx, "call-1", false)

		assertBindingLossWake(t, f, store, ctx, 1)
		assertNoPeriodicReArm(t, f, store, ctx)
	})

	t.Run("flag-on", func(t *testing.T) {
		t.Setenv("RUSH_BATCH_RUNTIME", "1")
		f := newAttemptFixture(t, "wake-binding-loss-epoch-on", attemptFixtureOpts{
			noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
		})
		store := wireLostBindingFence(t, f)
		f.coord.addToRecheckSet(f.sessID)
		// The legacy row stays so the Reacted-untouched assertions still
		// mean something; under ON the batch decision path never reads
		// async_jobs, so it cannot influence the launch decision.
		f.seedDebt(ctx, "call-1", false)

		// Batch debt owned by f.sessID (the same settled DELIVERED shape the
		// entry test uses): a countingTool leaf, so the root settles
		// instantly with no release bookkeeping.
		fast := &countingTool{name: "view"}
		env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
		// batch_nodes.owner_session_id REFERENCES sessions(id), and the
		// detached delivery's history writes are FK'd to sessions: create
		// the fixture's session row in the cutover DB first.
		_, err := env.q.CreateSession(env.ctx, db.CreateSessionParams{ID: f.sessID, Title: f.sessID})
		require.NoError(t, err)
		invokeDetachedAs(t, env, f.sessID, "call-binding-loss-on", "view", fast)
		rootID := rootIDFor(t, env, f.sessID, "call-binding-loss-on")
		require.Equal(t, "completed", rootTerminalFor(t, env, f.sessID, rootID))
		// Pull with the OWNER so the completion is VISIBLE/selected debt.
		require.Eventually(t, func() bool {
			pulled, err := env.store.BatchPullNotices(env.ctx, env.messages, f.sessID, buildBatchNoticeMessageParams)
			require.NoError(t, err)
			return len(pulled) >= 1
		}, 5*time.Second, 5*time.Millisecond, "the batch pull never delivered the completion for %s", f.sessID)

		// Bind the node-backed facts to the FIXTURE's coordinator so
		// drainDecision's SelectedDebtExists counts the delivered row for
		// f.sessID instead of unknownBatchCutoverFacts' conservative error.
		// The binding is process-global: delete it on cleanup.
		BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(env.store, env.live, env.conn))
		t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

		assertBindingLossWake(t, f, store, ctx, 1)
		assertNoPeriodicReArm(t, f, store, ctx)
	})
}

// sessionRequirementForBinding builds the fence envelope the tests store.
func sessionRequirementForBinding(bindingID string) session.SessionExecutionRequirement {
	return session.SessionExecutionRequirement{BindingID: bindingID}
}

// TestInterruptTickStopsForUnavailableBinding pins the gh#11a
// classification of tick errors: a permanently non-durable dependency and a
// lost process-bound binding both STOP the ticker; transient errors do not.
// The ticker's loop calls this predicate:
//
//	if interruptTickStopsFor(err) {
//		slog.Error("coordinator: interrupt-inject cannot cross the durable queue; the dependency is permanently unavailable for this epoch", "session_id", sessionID, "err", err)
//		return
//	}
func TestInterruptTickStopsForUnavailableBinding(t *testing.T) {
	require.True(t, interruptTickStopsFor(fmt.Errorf("wrap: %w", ErrDiskProviderUnavailable)))
	require.True(t, interruptTickStopsFor(ErrDiskProviderNotDurable))
	require.False(t, interruptTickStopsFor(errors.New("db hiccough")))
	require.False(t, interruptTickStopsFor(context.DeadlineExceeded))
}

// tickRefusingSessions stubs PeekInterruptInject with the permanently
// unavailable dependency error, counting the tick attempts.
type tickRefusingSessions struct {
	session.Service
	peeks atomic.Int64
}

func (s *tickRefusingSessions) PeekInterruptInject(context.Context, string) (*session.PendingInject, error) {
	s.peeks.Add(1)
	return nil, fmt.Errorf("peek refused: %w", ErrDiskProviderUnavailable)
}

// TestInterruptTicker_StopsOnUnavailableBinding drives the REAL ticker loop
// (startInterruptTicker) against a session service whose PeekInterruptInject
// fails typed ErrDiskProviderUnavailable, and proves the loop EXITS (done
// closes) after exactly one tick instead of spinning refusals.
//
// WHY IT FAILS ON OLD CODE: reverting the loop at coordinator_interrupt.go
// (~:259-263) to test only ErrDiskProviderNotDurable leaves the ticker
// running: done never closes (the Eventually times out) and the peek count
// grows past 1 (the Never fails).
func TestInterruptTicker_StopsOnUnavailableBinding(t *testing.T) {
	// The tick interval is a process-global var: shrink it to test timescale
	// and restore on cleanup.
	orig := interruptInjectTick
	interruptInjectTick = 2 * time.Millisecond
	t.Cleanup(func() { interruptInjectTick = orig })

	env := testEnv(t)
	// Bare coordinator: a nil currentAgent is safe here -- handleInterruptTick
	// calls activeCallStateForSession(nil, id), whose type assertions all
	// fall through to the snapshotter miss returning
	// (SessionAgentCall{}, activeCallToken{}, owned=false, published=true,
	// tokenAvailable=false) with no panic; the FIRST real interaction is
	// PeekInterruptInject, which the stub refuses.
	coord := &coordinator{
		sessions:        &tickRefusingSessions{Service: env.sessions},
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	stub := coord.sessions.(*tickRefusingSessions)

	done := coord.startInterruptTicker(context.Background(), "s-tick")
	// The loop exiting IS the stop.
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 2*time.Millisecond,
		"the ticker must STOP (close done) after the unavailable-binding tick error")
	require.EqualValues(t, 1, stub.peeks.Load(), "exactly one tick attempt")
	// After the goroutine has returned the count cannot grow: the stop is a
	// stop, not a pause.
	require.Never(t, func() bool { return stub.peeks.Load() > 1 },
		20*time.Millisecond, 2*time.Millisecond,
		"the stopped ticker must not keep peeking")
}
