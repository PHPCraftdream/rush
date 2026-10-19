package server

// R2C-4 / R2C-12 (docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md):
// a rerun keeps automatic (Drain) turns off the session from the moment it
// cancels the live turn until it hands the reservation to the replacement turn,
// and the stop of the voided jobs runs off the handler.

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

var (
	_ agent.Coordinator    = (*orderedHoldCoordinator)(nil)
	_ agent.AutoTurnHolder = (*orderedHoldCoordinator)(nil)
)

// orderedHoldCoordinator records, in order, when the rerun takes and releases
// the automatic-turn hold relative to CancelTurn and the handoff into the
// replacement turn.
type orderedHoldCoordinator struct {
	cancellableHoldCoordinator
	evMu      sync.Mutex
	events    []string
	held      int
	heldAtRun int
}

func (m *orderedHoldCoordinator) log(ev string) {
	m.evMu.Lock()
	m.events = append(m.events, ev)
	m.evMu.Unlock()
}

func (m *orderedHoldCoordinator) HoldAutomaticTurns(string) (release func()) {
	m.evMu.Lock()
	m.events = append(m.events, "hold")
	m.held++
	m.evMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.evMu.Lock()
			m.events = append(m.events, "release")
			m.held--
			m.evMu.Unlock()
		})
	}
}

func (m *orderedHoldCoordinator) CancelTurn(sessionID string) {
	m.log("cancelTurn")
	m.cancellableHoldCoordinator.CancelTurn(sessionID)
}

func (m *orderedHoldCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	m.evMu.Lock()
	m.events = append(m.events, "handoff")
	m.heldAtRun = m.held
	m.evMu.Unlock()
	return m.cancellableHoldCoordinator.RunWithReservedOwnership(ctx, sessionID, prompt, epoch, cancel, onHandoff, smart, fast, attachments...)
}

func (m *orderedHoldCoordinator) snapshot() ([]string, int, int) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	return append([]string(nil), m.events...), m.held, m.heldAtRun
}

func indexOfEvent(events []string, ev string) int {
	for i, e := range events {
		if e == ev {
			return i
		}
	}
	return -1
}

// TestRerun_HoldsAutomaticTurnsUntilHandoff: the hold is taken before
// CancelTurn and released exactly once, before the replacement turn starts.
//
// Revert-check: not taking the hold (or releasing it only after the handoff
// returns) turns the order/heldAtRun assertions red.
func TestRerun_HoldsAutomaticTurnsUntilHandoff(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-holds-turns")
	coord := &orderedHoldCoordinator{}
	f.a.AgentCoordinator = coord

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type)
	events, held, heldAtRun := coord.snapshot()
	hold, cancelTurn, release, handoff := indexOfEvent(events, "hold"), indexOfEvent(events, "cancelTurn"), indexOfEvent(events, "release"), indexOfEvent(events, "handoff")
	require.GreaterOrEqual(t, hold, 0, "the rerun holds automatic turns: %v", events)
	require.Less(t, hold, cancelTurn, "the hold is taken before the live turn is cancelled: %v", events)
	require.Less(t, cancelTurn, release, "and released only after: %v", events)
	require.Less(t, release, handoff, "released before the replacement turn starts: %v", events)
	require.Zero(t, heldAtRun, "no hold is left while the replacement turn runs")
	require.Zero(t, held, "every hold is released")
	require.Equal(t, 1, countEvents(events, "release"), "released exactly once: %v", events)
}

func countEvents(events []string, ev string) int {
	n := 0
	for _, e := range events {
		if e == ev {
			n++
		}
	}
	return n
}

// TestRerun_BailoutReleasesTheHold: a rerun that bails out (cancelled at the last
// point) releases its hold, so automatic turns resume.
//
// Revert-check: dropping the deferred release leaves the hold held forever.
func TestRerun_BailoutReleasesTheHold(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-bailout-releases")
	coord := &orderedHoldCoordinator{}
	f.a.AgentCoordinator = coord
	rerunHoldingReservationSeam = func() { coord.Cancel(f.sessionID) }
	t.Cleanup(func() { rerunHoldingReservationSeam = nil })

	env := f.run(t)

	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "cancelled")
	events, held, _ := coord.snapshot()
	require.Contains(t, events, "hold")
	require.Zero(t, held, "a bailout releases the hold: %v", events)
	require.Equal(t, 1, countEvents(events, "release"))
	require.True(t, f.exists(t, f.tail.ID), "nothing was changed")
}

// blockingStopCoordinator's StopRerunJobs blocks until released, like a child
// transition retried until it commits. Its BatchRerunStopVoided (the flag-on
// surface) blocks the SAME way — it shares the entered/unblock channels, since
// the handler runs exactly one of the two paths per rerun — and follows the
// record → signal → block order: record the call, signal entered, then block
// on unblock (batchDone closes only after unblock, so a test joins the
// handler's detached goroutine by closing unblock and waiting batchDone).
type blockingStopCoordinator struct {
	cancellableHoldCoordinator
	entered chan struct{}
	unblock chan struct{}
	// Flag-on join channel and call record for BatchRerunStopVoided.
	batchDone   chan struct{}
	batchMu     sync.Mutex
	batchCalls  int
	batchVoided []session.BatchRerunVoidedNode
	// stopDone is closed by the legacy StopRerunJobs wrapper after its
	// embedded call returns, so cleanup can join the detached goroutine.
	stopDone chan struct{}
}

func (m *blockingStopCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
	close(m.entered)
	<-m.unblock
	m.cancellableHoldCoordinator.StopRerunJobs(ctx, sessionID, voided)
	close(m.stopDone)
}

// BatchRerunStopVoided implements agent.BatchRerunStopper. Like the flag-on
// production stop goroutine, it never writes delivery state — the reconcile
// already committed the void. Record → signal → block: the call is recorded
// and entered closed BEFORE it blocks on unblock, so the test's post-run
// assertions see the call while the stop is still blocked (the handler did
// not block on it).
func (m *blockingStopCoordinator) BatchRerunStopVoided(_ context.Context, _ string, voided []session.BatchRerunVoidedNode) {
	m.batchMu.Lock()
	m.batchCalls++
	m.batchVoided = append(m.batchVoided, voided...)
	m.batchMu.Unlock()
	close(m.entered)
	<-m.unblock
	if m.batchDone != nil {
		close(m.batchDone)
	}
}

// batchStopSnapshot returns the recorded flag-on stop calls and voided set.
func (m *blockingStopCoordinator) batchStopSnapshot() (int, []session.BatchRerunVoidedNode) {
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	return m.batchCalls, append([]session.BatchRerunVoidedNode(nil), m.batchVoided...)
}

// TestRerun_StopRerunJobsDoesNotBlockTheHandler: a StopRerunJobs that never
// returns must not keep the handler, its reservation or the reply waiting.
//
// Revert-check: calling StopRerunJobs inline in the handler blocks the reply
// until the 10s handler bound and this test goes red.
func TestRerun_StopRerunJobsDoesNotBlockTheHandler(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-stop-detached")
	coord := &blockingStopCoordinator{entered: make(chan struct{}), unblock: make(chan struct{}), stopDone: make(chan struct{})}
	f.a.AgentCoordinator = coord
	if batchRuntimeEnabled() {
		// Flag ON: the same live-work fact as one never-acked batch root
		// incarnation; the reconcile's live tool-call arm voids it and the
		// stop goes to BatchRerunStopVoided below.
		seedRerunBatchRoot(t, f)
		coord.batchDone = make(chan struct{})
	}
	t.Cleanup(func() {
		close(coord.unblock)
		if coord.batchDone != nil {
			// Join the handler's detached BatchRerunStopVoided goroutine: it
			// can only return once unblock above is closed.
			select {
			case <-coord.batchDone:
			case <-time.After(10 * time.Second):
				t.Errorf("the detached BatchRerunStopVoided goroutine did not return after unblock")
			}
		} else {
			// Legacy path: the detached StopRerunJobs goroutine may still be
			// mid-unblock; join it the same way.
			select {
			case <-coord.stopDone:
			case <-time.After(10 * time.Second):
				t.Errorf("the detached StopRerunJobs goroutine did not return after unblock")
			}
		}
	})

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type)
	select {
	case <-coord.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the voided jobs were never stopped")
	}
	require.Equal(t, "void", f.jobDelivery(t))
	if batchRuntimeEnabled() {
		calls, batchVoided := coord.batchStopSnapshot()
		require.Equal(t, 1, calls, "the flag-on stop must run exactly once")
		require.Len(t, batchVoided, 1, "the stop carries the voided incarnation")
		_, _, stopRerun, _, _ := coord.counters()
		require.Zero(t, stopRerun, "the flag-on stop must not take the legacy StopRerunJobs path")
	}
	_, _, _, _, owned := coord.counters()
	require.False(t, owned, "the reservation is released while the stop is still running")
}
