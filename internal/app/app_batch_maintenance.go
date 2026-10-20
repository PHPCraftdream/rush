package app

// H4 W2: the App-owned batch dead-host maintenance pass — the flag-ON
// equivalent of the OFF path's registration-time SweepDeadHosts and the
// 60s coordinator maintenance sweep. One bounded pass per invocation
// (one 1s context, at most batchMaintenanceMaxRoots root attempts) over
// the W1 keyset-cursor primitive (session.BatchRecoverPass), with the
// cursor kept between passes so repeated passes rotate through the
// candidate set instead of rescanning. Covers every host kind served by
// this App (CLI, server, SDK): it runs at composition, not behind the
// web-only recheck ticker. Recovery semantics are the store's: only a
// (Dead, nil probe error) verdict writes, each root commits separately,
// and recovery never wakes a paid turn (wake=0).

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// Maintenance pass bounds. Vars only so tests can shrink them; production
// never writes them.
var (
	batchMaintenanceInterval   = 60 * time.Second
	batchMaintenancePassBudget = 1 * time.Second
	batchMaintenanceMaxRoots   = 64
	batchMaintenancePageSize   = 64
)

// batchMaintenanceLoop is one App's maintenance goroutine state: the stop
// signal, the cancelable base context (so a stop also abandons the
// in-flight pass at its next root boundary), the join channel, and the
// rotating keyset cursor kept between passes.
type batchMaintenanceLoop struct {
	mu      sync.Mutex
	baseCtx context.Context
	cancel  context.CancelFunc
	cursor  session.BatchRecoveryCursor
	stop    chan struct{}
	done    chan struct{}
}

// batchMaintenanceLoops memoizes each App's maintenance loop (one
// goroutine per App, never per candidate), mirroring batchHostStores.
var batchMaintenanceLoops sync.Map // *App -> *batchMaintenanceLoop

// startBatchMaintenance runs the startup maintenance pass synchronously
// (bounded by batchMaintenancePassBudget) and arms the periodic pass at
// batchMaintenanceInterval. Flag off, or without a data dir or DB writer,
// it is a no-op — the flag-OFF App keeps its existing OFF recovery sites
// untouched. First wins: a second call never starts a second loop.
func (app *App) startBatchMaintenance() {
	if !batchCutoverOn() || app.dataDir == "" || app.batchCutoverWriter() == nil {
		return
	}
	base, cancel := context.WithCancel(context.Background())
	loop := &batchMaintenanceLoop{baseCtx: base, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	if _, loaded := batchMaintenanceLoops.LoadOrStore(app, loop); loaded {
		cancel()
		return
	}
	// Startup pass: outside host-registration locks, before the App
	// advertises readiness, exactly one bounded pass.
	app.batchMaintenancePass()
	go func() {
		defer close(loop.done)
		ticker := time.NewTicker(batchMaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loop.stop:
				return
			case <-ticker.C:
				app.batchMaintenancePass()
			}
		}
	}()
}

// batchMaintenancePass is ONE bounded recovery pass: a fresh timeout on
// the loop's cancelable base context, at most batchMaintenanceMaxRoots
// root attempts, probing each distinct host at most once, with the keyset
// cursor carried across passes. A failed or expired pass leaves the
// cursor resumable — the next pass continues where this one stopped.
// Best-effort: the pass never fails its caller.
func (app *App) batchMaintenancePass() {
	v, ok := batchMaintenanceLoops.Load(app)
	if !ok {
		return
	}
	loop := v.(*batchMaintenanceLoop)
	loop.mu.Lock()
	defer loop.mu.Unlock()
	ctx, cancel := context.WithTimeout(loop.baseCtx, batchMaintenancePassBudget)
	defer cancel()
	cursor := loop.cursor
	out, cursor, done, err := session.NewBatchStore(app.batchCutoverWriter()).
		BatchRecoverPass(ctx, session.BatchSharedHostProbe(app.dataDir),
			session.BatchRecoveryLimits{MaxRoots: batchMaintenanceMaxRoots, PageSize: batchMaintenancePageSize},
			cursor)
	loop.cursor = cursor
	_ = out
	_ = done
	if err != nil {
		slog.Debug("app: batch maintenance pass finished with errors", "err", err)
	}
}

// stopBatchMaintenance signals the periodic pass goroutine, cancels any
// in-flight pass, and joins the goroutine — the same synchronous
// before-DB-close ordering as the other App pollers. Idempotent and
// nil-safe for bare App literals.
func (app *App) stopBatchMaintenance() {
	v, ok := batchMaintenanceLoops.LoadAndDelete(app)
	if !ok {
		return
	}
	loop := v.(*batchMaintenanceLoop)
	close(loop.stop)
	if loop.cancel != nil {
		loop.cancel()
	}
	// Each pass is bounded by batchMaintenancePassBudget and the goroutine
	// selects on stop between passes, so the join is bounded in practice;
	// the select has no timeout on purpose, matching the other pollers'
	// synchronous joins.
	<-loop.done
}
