package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

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
// titleCtx is derived from genCtx (not runTurn's outer ctx) so the stream
// watchdog cancelling genCtx -- idle timeout, tool timeout, hard cap --
// also cuts off an in-flight title generation instead of leaving it to run
// on an unbounded parent context. It is additionally, independently capped
// by effectiveTitleGenerationMaxDuration as a backstop: generateTitle's two
// model attempts are each a blocking agent.Stream with no timeout of their
// own, so a provider that never returns must not be able to keep the join
// from returning even if genCtx's own cancellation somehow doesn't unblock
// it.
func startTitleGeneration(a *sessionAgent, genCtx context.Context, needsTitle bool, sessionID, prompt string, cfg turnConfig) chan struct{} {
	if !needsTitle {
		return nil
	}
	titleCtx, titleCancel := context.WithTimeout(genCtx, a.effectiveTitleGenerationMaxDuration())
	done := make(chan struct{})
	// Safe without admission gate: runWg.Add(1) here is always called from
	// inside an already-admitted Run() call, so runWg counter is guaranteed
	// >= 1 at this point. Per sync.WaitGroup contract, Add(1) when counter
	// is > 0 may happen at any time, including concurrently with Wait. The
	// real race P1-1 closes is Add starting when counter is zero and Wait
	// starting between the check and the Add.
	a.runWg.Add(1)
	go func() {
		defer close(done)
		defer a.runWg.Done()
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
// turn whose work had finished. We wait up to a grace period and otherwise
// abandon it: the goroutine exits whenever its provider unblocks, but
// abandoning it DOES lose the real title. generateTitle's actual
// a.sessions.Rename call runs on titleCtx itself (cancellable, derived from
// genCtx), not a detached context -- only its FALLBACK path (stamping the
// default "Untitled Session" name) uses context.WithoutCancel, precisely so
// that fallback can still land after the caller gives up. So once runTurn's
// cancel() fires, a late-finishing title attempt fails to save and the
// fallback stamps the default instead.
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
			slog.Warn(
				"agent: abandoning title generation that outlived its deadline — the turn is not held open for it",
				"session_id", j.sessionID,
				"grace", grace,
			)
		}
	})
}
