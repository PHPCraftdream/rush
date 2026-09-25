package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// titleGenRegistry tracks the cancel funcs of in-flight title-generation
// goroutines.
//
// It exists because those goroutines' contexts are deliberately detached
// from the turn that spawned them (see startTitleGeneration) — nothing in
// the turn's own cancellation reaches them any more. CancelAll therefore
// needs its own hook, or a title provider that ignores cancellation would
// hold runWg (and with it App.Shutdown) for CancelAll's entire grace on
// every shutdown. register returns the id the goroutine must unregister on
// exit; cancelAll fires every live cancel and is safe to call on a nil
// registry (bare test fixtures that never construct one).
type titleGenRegistry struct {
	mu      sync.Mutex
	next    int64
	cancels map[int64]context.CancelFunc
}

func newTitleGenRegistry() *titleGenRegistry {
	return &titleGenRegistry{cancels: make(map[int64]context.CancelFunc)}
}

func (r *titleGenRegistry) register(cancel context.CancelFunc) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := r.next
	r.cancels[id] = cancel
	return id
}

func (r *titleGenRegistry) unregister(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, id)
}

// cancelAll fires every registered cancel. Invoked once by CancelAll, which
// is terminal for the agent, so the map is simply drained.
func (r *titleGenRegistry) cancelAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.cancels))
	for _, c := range r.cancels {
		cancels = append(cancels, c)
	}
	clear(r.cancels)
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// startTitleGeneration launches runTurn's title-generation goroutine when
// needsTitle is true and returns the channel it closes on exit (nil when no
// title was requested, matching the zero value of the local runTurn used to
// declare for this). Split out of agent_turn.go's runTurn when the
// 1000-line limit landed.
//
// Deliberately a free function rather than a method on turnTitleJoiner
// below: runTurn calls this BEFORE its stream watchdog exists (the
// watchdog needs no part of title generation to start), and only the JOIN
// needs the watchdog, to disarm it before waiting. Bundling launch and join
// into one constructor would force moving this call to after the watchdog
// is built, changing the relative order of two independent setup steps
// that this file's own history says not to move without a specific reason
// -- see turnTitleJoiner's doc.
//
// The title's context is deliberately detached from the turn's own
// cancellation: context.WithoutCancel(genCtx) keeps every value genCtx
// carries while making the context immune to the cancel() runTurn fires the
// instant it stops waiting for the title.
//
// Note WHY WithoutCancel and not "the turn's parent context": every context
// in this chain is turn-scoped. runTurn receives turnCtx, which runOwned's
// loop cancels the moment runTurn returns (agent_run.go's `turnCancel()`),
// and runCtx above it is cancelled by runOwned's own deferred runCancel.
// Deriving from either reproduces the bug on a shorter fuse — the title
// request is killed by the turn unwinding rather than by anything the user
// did.
//
// What detachment costs, and how it is paid for: a per-session interrupt
// (Ctrl-C, the web stop button, `sessions kill`) no longer aborts an
// in-flight title request either — it now stops on its own budget
// (effectiveTitleGenerationMaxDuration) instead. Agent SHUTDOWN still stops
// it, via titleGenRegistry below: CancelAll fires every registered cancel,
// so a title provider that ignores cancellation cannot hold App.Shutdown
// open for CancelAll's whole grace. The goroutine is also registered in
// runWg, which is what lets CancelAll join it at all (P0-4).
//
// This is the root-cause fix for "the session title never appears". The old
// code derived titleCtx from genCtx, so the moment runTurn's bounded join
// gave up (titleJoinGrace) and fell through to its explicit cancel(), the
// still-in-flight title request was killed mid-flight. generateTitle then
// logged "Error generating title with fast model; trying next
// err=context canceled", tried the smart model, got the same error, and gave
// up — and for a web session (born titled "New Session", see
// internal/server/handlers_sessions.go) the deferred "Untitled Session"
// fallback then declined to write, because it only stamps a slot it can
// VERIFY is still empty. Net result: no generated title at all, ever, on
// every session whose title provider took longer than the grace.
//
// Detaching the context turns that cliff into a delay: the title goroutine
// keeps its own budget (effectiveTitleGenerationMaxDuration) and its own
// result, and survives the turn that spawned it. The join below bounds only
// how long runTurn WAITS, never whether the title eventually lands.
func startTitleGeneration(a *sessionAgent, genCtx context.Context, needsTitle bool, sessionID, prompt string, cfg turnConfig) chan struct{} {
	if !needsTitle {
		return nil
	}
	titleCtx, titleCancel := context.WithTimeout(context.WithoutCancel(genCtx), a.effectiveTitleGenerationMaxDuration())
	done := make(chan struct{})
	// Safe without admission gate: runWg.Add(1) here is always called from
	// inside an already-admitted Run() call, so runWg counter is guaranteed
	// >= 1 at this point. Per sync.WaitGroup contract, Add(1) when counter
	// is > 0 may happen at any time, including concurrently with Wait. The
	// real race P1-1 closes is Add starting when counter is zero and Wait
	// starting between the check and the Add.
	titleGenID := a.titleGens.register(titleCancel)
	a.runWg.Add(1)
	go func() {
		defer close(done)
		defer a.runWg.Done()
		defer a.titleGens.unregister(titleGenID)
		defer titleCancel()
		a.generateTitle(titleCtx, sessionID, prompt, cfg)
	}()
	return done
}

// turnTitleJoiner owns the bounded join that waits for the title-generation
// goroutine startTitleGeneration launched. Unlike checkpoint/peak-hours/
// UI-notify this one carries a real historical fragility, so its
// extraction preserves the exact defer-registration SITE in runTurn, not
// just its behaviour: only the deferred call's target changed (a closure
// became a method value); the `defer` statement itself stays at the
// identical point in runTurn's sequence of defers, which is what the
// ordering below depends on.
//
// join() is a named function called from TWO places (task #525): once
// deferred, once explicit right before runTurn's own final cancel(). The
// join used to exist only as a defer, and runTurn calls cancel() EXPLICITLY
// near its end -- before any defer runs -- so on the success path titleCtx
// (derived from genCtx) was already cancelled by the time anything waited
// for it. The observed failure: a session left "Untitled Session" with
// "Error generating title with fast model; trying next err=context
// canceled" for both models. It surfaced as a ~1-in-27 flake only because
// the mock in the older test answers instantly and usually wins that race.
//
// So the success path joins BEFORE that explicit cancel, and the defer
// remains for every early return that never reaches it. sync.Once keeps a
// turn from paying the grace period twice.
//
// It stays bounded, which is what the original P1-B fix added:
// generateTitle's attempts are blocking agent.Stream calls with no timeout
// of their own, so a provider that ignores context cancellation never
// returns, and waiting unconditionally once held runTurn -- and with it
// Run, the session's mailbox ownership and its OS lock -- open forever on a
// turn whose work had finished.
//
// Abandoning the goroutine no longer loses the title. That used to be the
// trade (and why shortening this constant looked expensive): the title's
// context was derived from genCtx, so runTurn's cancel() -- which fires the
// instant the join gives up -- killed the in-flight request and the title
// with it. startTitleGeneration now derives the title's context with
// context.WithoutCancel, so the goroutine keeps running after the join
// gives up, finishes on its own budget, and still persists (and publishes)
// its result. What the grace now costs is only how long runTurn is held
// open past its own work, never whether the session ends up titled.
//
// disarm() is called FIRST inside the Once body, on every path that reaches
// it -- not just the success path -- because join() is called both
// explicitly (success path) and via the deferred call (every early return).
// The turn's real work is finished by the time ANY caller of join() runs;
// all that remains is this bounded wait and the eventual cancel().
// --timeout-hard-cap is absolute from turn start rather than idle-based, so
// without disarming first, the wait alone could push a turn that finished
// just inside the cap over it -- firing a stall dump for a turn that had
// already finished, and cancelling the very title being waited for. The
// goroutine still exits on genCtx and is still joined by the caller's own
// deferred `<-wd.done` regardless of disarm.
type turnTitleJoiner struct {
	wd            streamWatchdog
	done          chan struct{}
	overrideGrace time.Duration
	sessionID     string

	once sync.Once
}

// Test-only seam; nil in production.
var turnTitleJoinAfterDisarmSeam func()

func newTurnTitleJoiner(wd streamWatchdog, done chan struct{}, overrideGrace time.Duration, sessionID string) *turnTitleJoiner {
	return &turnTitleJoiner{wd: wd, done: done, overrideGrace: overrideGrace, sessionID: sessionID}
}

func (j *turnTitleJoiner) join() {
	j.once.Do(func() {
		j.wd.disarm()
		if turnTitleJoinAfterDisarmSeam != nil {
			turnTitleJoinAfterDisarmSeam()
		}
		if j.done == nil {
			return
		}
		grace := titleJoinGrace
		if j.overrideGrace > 0 {
			grace = j.overrideGrace
		}
		select {
		case <-j.done:
		case <-time.After(grace):
			// Not an error and not a loss: the goroutine keeps running on
			// its own budget and still persists/publishes the title when it
			// lands. Only THIS turn stops waiting for it.
			slog.Warn(
				"agent: title generation outlived the turn's grace — the turn is not held open for it, the title still lands on its own",
				"session_id", j.sessionID,
				"grace", grace,
			)
		}
	})
}
