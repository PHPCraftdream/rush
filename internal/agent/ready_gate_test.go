package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReadyGateRegisterWhileWaitParked reproduces the exact interleaving that
// made coordinator.readyWg (then an errgroup.Group) fail under -race roughly
// once per 120 runs of sdk's TestRunWithCredentialsConcurrentTenantsIsolated:
//
//	run A: RunWithCredentials -> readyWg.Wait()  parks as the FIRST waiter
//	       (sync.WaitGroup models that as a write of &wg.sema)
//	...    the in-flight builds finish, the counter drains back to 0
//	run B: ExecuteRun -> UpdateModels -> buildTools -> agentTool -> buildAgent
//	       -> readyWg.Go()  performs the 0 -> 1 Add
//	       (modeled as a read of &wg.sema)
//
// with no happens-before edge between the two — which is precisely what
// errgroup forbids ("The first call to Go must happen before a Wait") and
// what -race reports. Deliberately NO channel handshake between the parked
// waiter and the late registration: adding one is what hid this for ~190
// clean runs. Under -race this test fails on the errgroup implementation and
// passes on readyGate.
func TestReadyGateRegisterWhileWaitParked(t *testing.T) {
	var g readyGate

	release := make(chan struct{})
	waitStarted := make(chan struct{})

	g.Go(func() error {
		<-release
		return nil
	})

	go func() {
		close(waitStarted)
		_ = g.Wait()
	}()

	<-waitStarted
	time.Sleep(100 * time.Millisecond) // let Wait park as the first waiter
	close(release)
	time.Sleep(100 * time.Millisecond) // let the pending count drain to 0

	done := make(chan struct{})
	g.Go(func() error {
		close(done)
		return nil
	})
	<-done
	require.NoError(t, g.Wait())
}

// TestReadyGateReentrantRegistration covers buildAgent's own shape: the tool
// task registered on the gate calls agentTool, which builds the sub-agent and
// registers two MORE tasks on the SAME gate — while another goroutine is
// already parked in Wait. A plain mutex around Go/Wait would deadlock here;
// the Cond-based gate must let the nested registration through and hold the
// waiter until the whole tree is done.
func TestReadyGateReentrantRegistration(t *testing.T) {
	var g readyGate

	var mu sync.Mutex
	finished := 0
	markDone := func() {
		mu.Lock()
		finished++
		mu.Unlock()
	}

	waiterParked := make(chan struct{})
	waiterDone := make(chan error, 1)
	outerRunning := make(chan struct{})
	releaseOuter := make(chan struct{})

	g.Go(func() error {
		close(outerRunning)
		<-releaseOuter
		// Nested registration from inside a running task.
		for range 2 {
			g.Go(func() error {
				time.Sleep(20 * time.Millisecond)
				markDone()
				return nil
			})
		}
		markDone()
		return nil
	})

	<-outerRunning
	go func() {
		close(waiterParked)
		waiterDone <- g.Wait()
	}()
	<-waiterParked
	time.Sleep(50 * time.Millisecond)
	close(releaseOuter)

	select {
	case err := <-waiterDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("readyGate.Wait deadlocked on a re-entrant registration")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 3, finished, "Wait returned before every nested task finished")
}

// TestReadyGateFirstErrorSticky pins the errgroup semantics the coordinator
// relies on: the first non-nil task error is what Wait reports, and it keeps
// reporting it on every later Wait (run entry points treat a non-nil Wait as
// "this coordinator never became usable").
func TestReadyGateFirstErrorSticky(t *testing.T) {
	var g readyGate

	first := errors.New("first build failure")
	second := errors.New("second build failure")

	started := make(chan struct{})
	g.Go(func() error {
		close(started)
		return first
	})
	<-started
	require.ErrorIs(t, g.Wait(), first)

	g.Go(func() error { return second })
	require.ErrorIs(t, g.Wait(), first, "a later failure must not displace the first one")
}

// TestReadyGateConcurrentGoAndWait is the unbounded version of the first test:
// many registrations and many waits overlapping on one gate, the shape a
// long-lived sdk.Client with N concurrent RunWithCredentials calls produces.
// Its job is to fail under -race, not to assert an outcome.
func TestReadyGateConcurrentGoAndWait(t *testing.T) {
	var g readyGate
	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 25 {
				g.Go(func() error { return nil })
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 25 {
				require.NoError(t, g.Wait())
			}
		})
	}
	wg.Wait()
	require.NoError(t, g.Wait())
}
