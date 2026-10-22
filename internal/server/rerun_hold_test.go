package server

// R2C-4 / R2C-12 (docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md):
// a rerun keeps automatic (Drain) turns off the session from the moment it
// cancels the live turn until it hands the reservation to the replacement turn,
// and the stop of the voided batch incarnations runs off the handler on a
// detached goroutine (the legacy StopRerunJobs contract is gone).

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

// blockingStopCoordinator's BatchRerunStopVoided (the agent.BatchRerunStopper
// surface the detached stop goroutine runs on) blocks until released, like a
// child transition retried until it commits. It follows the record → signal →
// block order: record the call, signal entered, then block on unblock
// (batchDone closes only after unblock, so a test joins the handler's
// detached goroutine by closing unblock and waiting batchDone).
type blockingStopCoordinator struct {
	cancellableHoldCoordinator
	entered chan struct{}
	unblock chan struct{}
	// batchDone is closed by BatchRerunStopVoided after its embedded unblock
	// wait returns, so cleanup can join the handler's detached goroutine.
	// closeOnce guards the close (defensive: only one detached stop runs per
	// rerun, but the goroutine is the only closer and cleanup never closes).
	batchDone   chan struct{}
	closeOnce   sync.Once
	batchMu     sync.Mutex
	batchCalls  int
	batchVoided []session.BatchRerunVoidedNode
}

// BatchRerunStopVoided implements agent.BatchRerunStopper. Like the
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
	m.closeOnce.Do(func() {
		if m.batchDone != nil {
			close(m.batchDone)
		}
	})
}

// batchStopSnapshot returns the recorded stop calls and voided set.
func (m *blockingStopCoordinator) batchStopSnapshot() (int, []session.BatchRerunVoidedNode) {
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	return m.batchCalls, append([]session.BatchRerunVoidedNode(nil), m.batchVoided...)
}

// TestRerun_DetachedBatchStopDoesNotBlockTheHandler: a BatchRerunStopVoided
// that never returns must not keep the handler, its reservation or the reply
// waiting — the detached goroutine runs it.
//
// Revert-check: calling BatchRerunStopVoided inline in the handler blocks the
// reply until the 10s handler bound and this test goes red.
func TestRerun_DetachedBatchStopDoesNotBlockTheHandler(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-stop-detached")
	coord := &blockingStopCoordinator{entered: make(chan struct{}), unblock: make(chan struct{}), batchDone: make(chan struct{})}
	f.a.AgentCoordinator = coord
	t.Cleanup(func() {
		// Release the stop, then join the handler's detached
		// BatchRerunStopVoided goroutine: it can only return once unblock is
		// closed. Cleanup runs on the test goroutine, so failures are
		// t.Errorf, never t.Fatal.
		close(coord.unblock)
		select {
		case <-coord.batchDone:
		case <-time.After(10 * time.Second):
			t.Errorf("the detached BatchRerunStopVoided goroutine did not return after unblock")
		}
	})

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type)
	select {
	case <-coord.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the voided incarnations were never stopped")
	}
	require.Equal(t, "void", f.jobDelivery(t))
	calls, batchVoided := coord.batchStopSnapshot()
	require.Equal(t, 1, calls, "the detached stop must run exactly once")
	require.Len(t, batchVoided, 1, "the stop carries the voided incarnation")
	_, _, owned := coord.counters()
	require.False(t, owned, "the reservation is released while the stop is still running")
}
