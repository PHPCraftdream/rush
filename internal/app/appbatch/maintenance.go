package appbatch

// H4 W2: the App-owned batch dead-host maintenance pass — the flag-ON
// equivalent of the OFF path's registration-time SweepDeadHosts and the
// 60s coordinator maintenance sweep. One bounded pass per invocation
// (one 1s context, at most the configured MaxRoots root attempts) over
// the W1 keyset-cursor primitive (session.BatchRecoverPass), with the
// cursor kept between passes so repeated passes rotate through the
// candidate set instead of rescanning. Covers every host kind served by
// this App (CLI, server, SDK): it runs at composition, not behind the
// web-only recheck ticker. Recovery semantics are the store's: only a
// (Dead, nil probe error) verdict writes, each root commits separately,
// and recovery never wakes a paid turn (wake=0).

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// MaintenanceBounds is a value snapshot of the limits used by batch
// dead-host maintenance. Providers may return updated values between passes.
type MaintenanceBounds struct {
	Interval   time.Duration
	PassBudget time.Duration
	MaxRoots   int
	PageSize   int
}

const (
	defaultBatchMaintenanceInterval   = 60 * time.Second
	defaultBatchMaintenancePassBudget = 1 * time.Second
	defaultBatchMaintenanceMaxRoots   = 64
	defaultBatchMaintenancePageSize   = 64
)

func maintenanceBounds(p BatchCLIParams) MaintenanceBounds {
	bounds := MaintenanceBounds{}
	if p.MaintenanceBounds != nil {
		bounds = p.MaintenanceBounds()
	}
	if bounds.Interval == 0 {
		bounds.Interval = defaultBatchMaintenanceInterval
	}
	if bounds.PassBudget == 0 {
		bounds.PassBudget = defaultBatchMaintenancePassBudget
	}
	if bounds.MaxRoots == 0 {
		bounds.MaxRoots = defaultBatchMaintenanceMaxRoots
	}
	if bounds.PageSize == 0 {
		bounds.PageSize = defaultBatchMaintenancePageSize
	}
	return bounds
}

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
var batchMaintenanceLoops sync.Map // *Owner -> *batchMaintenanceLoop

// StartBatchMaintenance runs the startup maintenance pass synchronously
// (bounded by the provider's PassBudget) and arms the periodic pass at the
// provider's Interval. Without a data dir or DB writer,
// it is a no-op. First wins: a second call never starts a second loop.
func StartBatchMaintenance(p BatchCLIParams) {
	if p.DataDir == nil || p.DataDir() == "" || CutoverWriter(p) == nil {
		return
	}
	base, cancel := context.WithCancel(context.Background())
	loop := &batchMaintenanceLoop{baseCtx: base, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	if _, loaded := batchMaintenanceLoops.LoadOrStore(p.Owner(), loop); loaded {
		cancel()
		return
	}
	// Startup pass: outside host-registration locks, before the App
	// advertises readiness, exactly one bounded pass.
	BatchMaintenancePass(p)
	go func() {
		defer close(loop.done)
		ticker := time.NewTicker(maintenanceBounds(p).Interval)
		defer ticker.Stop()
		for {
			select {
			case <-loop.stop:
				return
			case <-ticker.C:
				BatchMaintenancePass(p)
			}
		}
	}()
}

// BatchMaintenancePass is ONE bounded recovery pass: a fresh timeout on
// the loop's cancelable base context, at most the provider's MaxRoots
// root attempts, probing each distinct host at most once, with the keyset
// cursor carried across passes. A failed or expired pass leaves the
// cursor resumable — the next pass continues where this one stopped.
// Best-effort: the pass never fails its caller.
func BatchMaintenancePass(p BatchCLIParams) {
	v, ok := batchMaintenanceLoops.Load(p.Owner())
	if !ok {
		return
	}
	loop := v.(*batchMaintenanceLoop)
	loop.mu.Lock()
	defer loop.mu.Unlock()
	passBudget := maintenanceBounds(p).PassBudget
	ctx, cancel := context.WithTimeout(loop.baseCtx, passBudget)
	defer cancel()
	cursor := loop.cursor
	store := session.NewBatchStore(CutoverWriter(p))
	probe := session.BatchSharedHostProbe(p.DataDir())
	limits := maintenanceBounds(p)
	out, cursor, done, err := store.BatchRecoverPass(ctx, probe,
		session.BatchRecoveryLimits{MaxRoots: limits.MaxRoots, PageSize: limits.PageSize},
		cursor)
	loop.cursor = cursor
	_ = out
	_ = done
	if err != nil {
		slog.Debug("app: batch maintenance pass finished with errors", "err", err)
	}
}

// StopBatchMaintenance signals the periodic pass goroutine, cancels any
// in-flight pass, and joins the goroutine — the same synchronous
// before-DB-close ordering as the other App pollers. Idempotent and
// nil-safe for bare App literals.
// MaintenanceLoopRegistered reports whether a periodic maintenance loop is
// armed for this owner (registry observability; a nil owner answers false).
func (o *Owner) MaintenanceLoopRegistered() bool {
	if o == nil {
		return false
	}
	_, ok := batchMaintenanceLoops.Load(o)
	return ok
}

// MaintenanceLoopCursor snapshots the rotating keyset cursor under the loop
// lock (cursor observability; BatchRecoveryCursorStart when no loop is armed).
func (o *Owner) MaintenanceLoopCursor() session.BatchRecoveryCursor {
	v, ok := batchMaintenanceLoops.Load(o)
	if !ok {
		return session.BatchRecoveryCursorStart
	}
	loop := v.(*batchMaintenanceLoop)
	loop.mu.Lock()
	defer loop.mu.Unlock()
	return loop.cursor
}

func StopBatchMaintenance(p BatchCLIParams) {
	v, ok := batchMaintenanceLoops.LoadAndDelete(p.Owner())
	if !ok {
		return
	}
	loop := v.(*batchMaintenanceLoop)
	close(loop.stop)
	if loop.cancel != nil {
		loop.cancel()
	}
	// Each pass is bounded by the provider's PassBudget and the goroutine
	// selects on stop between passes, so the join is bounded in practice;
	// the select has no timeout on purpose, matching the other pollers'
	// synchronous joins.
	<-loop.done
}

// The drain gate: in ordinary shutdown the Runner drain precedes
// projection/DB/host-identity release; a Close timeout RETAINS the
// still-draining writer and host identity and reports Forced, like the
// existing CancelAll convention (coordinator_interrupt.go:119). This is the
// single place R11F will call from app_lifecycle.go instead of "release on
// agent-idle".

// BatchDrainResult reports how the drain gate completed.
type BatchDrainResult struct {
	// Forced is true when the Runner was still draining after the drain
	// bound (Close timeout) or when a fault fence is active. On Forced the
	// host identity and the writer are deliberately NOT released — the same
	// policy as app_lifecycle.go's forced shutdown path: never release
	// resources beneath a draining Runner. A caller that sees Forced must
	// exit its process, as the CLI does and as ShutdownResult documents.
	Forced bool
	// Err is the Runner.Close error (CloseTimeoutError, DisposeError, fault
	// error) or the host-identity release error on the graceful path.
	Err error
}

// DrainAndRelease is the shutdown gate. Order, in ordinary shutdown:
//
//  1. Runner.Close (drain to Final) bounded by drainTimeout.
//  2. Host identity release.
//  3. Writer release.
//
// The gate never releases anything beneath a draining Runner: a Close
// timeout returns Forced with both the writer and the host identity
// retained. If the fault fence is active (fault observed before or during
// the drain) nothing is released at all and Forced is reported; the fault
// handler owns the process from that point and exits 70.
func (rt *BatchRuntime) DrainAndRelease(ctx context.Context, drainTimeout time.Duration) BatchDrainResult {
	// Fault already observed: the fault path retains everything until exit.
	if rt.Fenced() {
		slog.Warn("Batch drain: fault fence active - retaining host identity and writer until exit")
		return BatchDrainResult{Forced: true, Err: rt.faultErrLocked()}
	}

	runner := rt.Runner()
	if runner == nil {
		// Dormant runtime (no Runner ever attached): nothing to drain; keep
		// the existing release order so flag-off behavior is unchanged.
		return rt.releaseAll(ctx, BatchDrainResult{})
	}

	closeCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	closeErr := runner.Close(closeCtx)

	// Fault fired while (or before) draining: the fence wins over any
	// release the drain was about to perform.
	if rt.Fenced() {
		slog.Warn("Batch drain: fault observed during drain - retaining host identity and writer until exit")
		return BatchDrainResult{Forced: true, Err: closeErr}
	}

	if closeErr != nil {
		// Close timeout (ErrCloseTimeout) or dispose failures: retain the
		// still-draining resources. Forced, like CancelAll's still-busy
		// verdict; the caller exits the process.
		slog.Warn("Batch drain: runner did not drain within bound - retaining host identity and writer (forced)",
			"error", closeErr)
		rt.retainHostIdentity()
		return BatchDrainResult{Forced: true, Err: closeErr}
	}

	// Ordinary shutdown: the Runner reached Final. Only now may projection
	// support (host identity) and the DB writer be released.
	return rt.releaseAll(ctx, BatchDrainResult{})
}

// releaseAll performs the ordinary-shutdown releases in order: host identity
// first (it may write cleanup rows through the writer), then the writer.
// Each step re-checks the fence: a fault racing a graceful shutdown resolves
// in favour of the fault — the step after the fence observes it and retains.
func (rt *BatchRuntime) releaseAll(ctx context.Context, res BatchDrainResult) BatchDrainResult {
	if rt.Fenced() {
		return BatchDrainResult{Forced: true, Err: rt.faultErrLocked()}
	}
	if rt.Config.ReleaseHostIdentity != nil {
		if err := rt.Config.ReleaseHostIdentity(ctx); err != nil {
			slog.Warn("Batch drain: failed to release host identity", "error", err)
			res.Err = errors.Join(res.Err, err)
		}
	}
	if rt.Fenced() {
		return BatchDrainResult{Forced: true, Err: rt.faultErrLocked()}
	}
	if rt.Config.ReleaseWriter != nil {
		if err := rt.Config.ReleaseWriter(ctx); err != nil {
			slog.Warn("Batch drain: failed to release writer", "error", err)
			res.Err = errors.Join(res.Err, err)
		}
	}
	return res
}

// faultErrLocked snapshots the attached Runner's fault error, if any.
func (rt *BatchRuntime) faultErrLocked() error {
	if r := rt.Runner(); r != nil {
		return r.FaultErr()
	}
	return nil
}
