package agent

import "sync"

// readyGate is the coordinator's "agent build still in flight" barrier: every
// asynchronous piece of agent construction (buildAgent's system-prompt build
// and its tool build) registers here, and every entry point that is about to
// USE the built agent — Run, RunWithOverrides, RunWithCredentials,
// RunWithReservedOwnership, the interrupt paths — waits on it first.
//
// It used to be an errgroup.Group, and that was a real, reportable data race.
// errgroup documents both "A Group should not be reused for different tasks"
// and "The first call to Go must happen before a Wait", and this coordinator
// violates both by construction:
//
//   - the SAME gate is registered on every time an agent is (re)built —
//     once from NewCoordinator, and then again from every UpdateModels, via
//     buildTools -> agentTool -> buildAgent;
//   - every run entry point calls Wait, and internal/app's ExecuteRun calls
//     UpdateModels on its own goroutine right before starting the turn.
//
// One *App / one sdk.Client may have several runs in flight at once (that is
// the whole point of sdk.Client.RunWithCredentials' multi-tenant story), so
// run A's Wait can be parked as the FIRST waiter on the group's WaitGroup
// while run B's UpdateModels drives buildAgent into a fresh registration
// after the counter has already drained back to zero. sync.WaitGroup models
// exactly that pair as a write of &wg.sema (Wait, first waiter) against a
// read of &wg.sema (Add, on the 0 -> N transition) precisely so the detector
// catches it: -race reports a genuine DATA RACE between
// errgroup.(*Group).Wait and errgroup.(*Group).Go. In a build without -race
// the same interleaving can instead trip WaitGroup's own
// "sync: WaitGroup misuse: Add called concurrently with Wait" panic, so this
// is a production defect, not a test artifact. errgroup.Wait's unsynchronized
// `return g.err` racing a later task's `g.err = err` is the same bug again in
// a second field.
//
// A counter plus a sync.Cond has none of those constraints: registrations and
// waits may overlap freely and repeatedly, and — importantly — a task may
// register MORE work while it is itself running (buildAgent's tool task calls
// agentTool, which builds the sub-agent and registers two further tasks on
// this same gate) without deadlocking a concurrent waiter, because Cond.Wait
// releases the mutex while it blocks. A plain mutex around Go/Wait would
// deadlock on exactly that re-entrant registration.
//
// The zero value is ready to use. Semantics deliberately match the errgroup
// this replaces: Wait blocks until every task registered so far has returned,
// returns the FIRST non-nil error, and keeps returning it on later calls;
// panics inside a task are not recovered (they crash the process, as before),
// but the pending count is released first so a concurrent Wait cannot be
// wedged forever by one.
type readyGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending int
	err     error
}

// condLocked returns the gate's condition variable, creating it on first use.
// Caller must hold mu — which is also the Locker the Cond is built on, so a
// waiter releases it for the duration of Cond.Wait.
func (g *readyGate) condLocked() *sync.Cond {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
	return g.cond
}

// Go runs f in a new goroutine and counts it as pending until it returns.
// Safe to call at any time, including concurrently with Wait and from inside
// another task already registered on this gate.
func (g *readyGate) Go(f func() error) {
	g.mu.Lock()
	g.pending++
	g.mu.Unlock()

	go func() {
		var err error
		// Deferred so a panicking task releases its slot before unwinding;
		// the panic itself still propagates, exactly like errgroup.
		defer func() {
			g.mu.Lock()
			g.pending--
			if err != nil && g.err == nil {
				g.err = err
			}
			if g.pending == 0 {
				g.condLocked().Broadcast()
			}
			g.mu.Unlock()
		}()
		err = f()
	}()
}

// Wait blocks until no task is pending and returns the first error any task
// has reported so far (sticky, as with errgroup).
func (g *readyGate) Wait() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.pending > 0 {
		g.condLocked().Wait()
	}
	return g.err
}
