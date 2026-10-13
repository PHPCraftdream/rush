// The drain gate: in ordinary shutdown the Runner drain precedes
// projection/DB/host-identity release; a Close timeout RETAINS the
// still-draining writer and host identity and reports Forced, like the
// existing CancelAll convention (coordinator_interrupt.go:119). This is the
// single place R11F will call from app_lifecycle.go instead of "release on
// agent-idle".

package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

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
	if rt.cfg.ReleaseHostIdentity != nil {
		if err := rt.cfg.ReleaseHostIdentity(ctx); err != nil {
			slog.Warn("Batch drain: failed to release host identity", "error", err)
			res.Err = errors.Join(res.Err, err)
		}
	}
	if rt.Fenced() {
		return BatchDrainResult{Forced: true, Err: rt.faultErrLocked()}
	}
	if rt.cfg.ReleaseWriter != nil {
		if err := rt.cfg.ReleaseWriter(ctx); err != nil {
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
