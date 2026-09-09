package shell

// The per-job handle: output snapshots, buffer lifetime and release timers, completion callbacks and the wait/observe surface. Split out of background.go when the 1000-line file limit landed; background.go keeps the manager that owns the job table.

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// GetOutput returns the current output of a background shell. The returned
// strings are bounded snapshots (see boundedBuffer) — for a job that has
// produced more than the per-stream cap of output, the middle has been
// replaced with a "[N bytes truncated]" marker. If the buffers were already
// released after completion (scheduled via time.AfterFunc from the job's
// completion goroutine, see Start), and the stream had produced output before
// release, a placeholder note is returned instead of an empty string so this
// doesn't look like the command produced no output.
func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	stdout, stderr = bs.snapshotOutput()
	select {
	case <-bs.done:
		return stdout, stderr, true, bs.exitErr
	default:
		return stdout, stderr, false, nil
	}
}

func (bs *BackgroundShell) snapshotOutput() (stdout string, stderr string) {
	stdout = bs.stdout.String()
	stderr = bs.stderr.String()
	if bs.bufReleased.Load() {
		if stdout == "" && bs.stdout.releasedWithContent() {
			stdout = "(output was truncated after completion)"
		}
		if stderr == "" && bs.stderr.releasedWithContent() {
			stderr = "(output was truncated after completion)"
		}
	}
	return stdout, stderr
}

// releaseBuffers drops the buffered stdout/stderr content (freeing the
// memory) while leaving the job's status/metadata intact. Idempotent: a
// concurrent caller waits for the first release to finish and then returns.
// Automatically scheduled via time.AfterFunc from the job's completion
// goroutine (Start) after bufferRetention, and also reachable from Cleanup
// when a subsequent bash task triggers it — whichever fires first performs
// the release, the second is a no-op.
func (bs *BackgroundShell) releaseBuffers() {
	// sync.Once makes concurrent timer, cleanup, and removal callers wait for
	// the first release to finish instead of merely observing a flag while the
	// buffers are still being reset.
	bs.releaseOnce.Do(func() {
		bs.stdout.release()
		bs.stderr.release()
		bs.bufReleased.Store(true)
	})
}

// armBufferReleaseTimer schedules the one-shot post-completion buffer
// release. If Remove won a race with job completion, the buffers are released
// immediately instead of installing a timer for a detached job.
func (bs *BackgroundShell) armBufferReleaseTimer(retention time.Duration) {
	bs.retentionMu.Lock()
	if bs.detached {
		// A callback registered before completion owns the final output until
		// it has had a chance to read it. A tracked detached timer performs the
		// bounded release if the callback blocks; with no callback, release now.
		shouldRelease := bs.onDoneCount.Load() == 0
		if !shouldRelease {
			bs.armDetachedReleaseTimerLocked()
		}
		bs.retentionMu.Unlock()
		if shouldRelease {
			bs.releaseBuffers()
		}
		return
	}

	bs.retentionTimer = time.AfterFunc(retention, func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Background shell releaseBuffers panic",
					"shell_id", bs.ID,
					"command", bs.Command,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()

		bs.retentionMu.Lock()
		bs.retentionTimer = nil
		bs.retentionMu.Unlock()
		// Attached jobs always release at their normal retention deadline;
		// completion callbacks neither shorten nor extend that contract.
		bs.releaseBuffers()
	})
	bs.retentionMu.Unlock()
}

// armDetachedReleaseTimerLocked gives pending callbacks a bounded chance to
// consume final output after detachment. Exactly one tracked timer may exist.
// Caller must hold retentionMu.
func (bs *BackgroundShell) armDetachedReleaseTimerLocked() {
	if bs.detachedReleaseTimer != nil || bs.bufReleased.Load() {
		return
	}
	bs.detachedReleaseTimer = time.AfterFunc(detachedCallbackReleaseGrace, func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Background shell detached releaseBuffers panic",
					"shell_id", bs.ID,
					"command", bs.Command,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()

		bs.retentionMu.Lock()
		bs.detachedReleaseTimer = nil
		shouldRelease := bs.detached && (bs.completedAt.Load() > 0 || bs.IsDone())
		bs.retentionMu.Unlock()
		if shouldRelease {
			bs.releaseBuffers()
		}
	})
}

// detachFromManager stops a pending retention timer and releases completed
// output immediately. If completion is still in flight, detached makes its
// completion path perform the release after the process exits.
func (bs *BackgroundShell) detachFromManager() {
	bs.retentionMu.Lock()
	bs.detached = true
	retentionTimer := bs.retentionTimer
	bs.retentionTimer = nil
	completed := bs.completedAt.Load() > 0 || bs.IsDone()
	shouldRelease := completed && bs.onDoneCount.Load() == 0
	if completed && !shouldRelease {
		// This also covers a callback that began while the shell was attached
		// and has already run longer than the grace period: its attached phase
		// had no watchdog, so detachment starts a fresh bounded release window.
		bs.armDetachedReleaseTimerLocked()
	}
	bs.retentionMu.Unlock()

	if retentionTimer != nil {
		retentionTimer.Stop()
	}
	if shouldRelease {
		bs.releaseBuffers()
	}
}

// OnDone registers fn to be called EXACTLY ONCE when this background shell
// reaches a terminal state (the command exits, or it is killed). If the shell
// is already done when OnDone is called, fn runs promptly. fn runs on its own
// goroutine — it must not block. Used by higher layers (the agent) to react to
// a backgrounded command finishing without internal/shell importing them.
//
// This goroutine is independent of whatever turn started the background job
// — it can fire long after that turn (and any panic-recovery wrapping it)
// has already returned, so it needs its own panic isolation. fn is normally
// the agent package's notifyBackgroundJobDone, which can itself start a
// fresh top-level turn (Phase 4 auto-resume); an unrecovered panic here
// would otherwise crash the whole rush process with no log output, exactly
// like the goroutine in app.go's RunNonInteractive this mirrors.
func (bs *BackgroundShell) OnDone(fn func()) {
	if fn == nil {
		return
	}
	// Serialize registration with detach/completion arming. This makes a
	// callback registered before the completion transition visible to the
	// detached release decision, instead of racing a zero-count observation.
	bs.retentionMu.Lock()
	bs.onDoneCount.Add(1)
	bs.retentionMu.Unlock()
	go func() {
		<-bs.done
		defer func() {
			bs.retentionMu.Lock()
			lastCallback := bs.onDoneCount.Add(-1) == 0
			shouldRelease := bs.detached && lastCallback && (bs.completedAt.Load() > 0 || bs.IsDone())
			detachedTimer := bs.detachedReleaseTimer
			if shouldRelease {
				bs.detachedReleaseTimer = nil
			}
			bs.retentionMu.Unlock()

			if shouldRelease {
				if detachedTimer != nil {
					detachedTimer.Stop()
				}
				bs.releaseBuffers()
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background shell OnDone callback panic",
					"shell_id", bs.ID,
					"command", bs.Command,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// IsDone checks if the background shell has finished execution.
func (bs *BackgroundShell) IsDone() bool {
	select {
	case <-bs.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the background shell completes.
func (bs *BackgroundShell) Wait() {
	<-bs.done
}

func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Elapsed returns the wall-clock runtime of the background shell. For a job
// that has completed this is start→now (not start→finish); callers polling a
// finished job see a stable, monotonically increasing value which is fine for
// a "how long has this been going" status hint.
func (bs *BackgroundShell) Elapsed() time.Duration {
	if bs.StartTime.IsZero() {
		return 0
	}
	return time.Since(bs.StartTime)
}

// TotalWrittenBytes returns the total number of stdout+stderr bytes ever
// written by the job so far, independent of how much is still resident after
// bounded-buffer truncation. Callers that want to poll for "has more output
// arrived" (job_output's wait:true path) MUST use this — not
// len(stdout)+len(stderr) from GetOutput's bounded snapshot — as the baseline
// passed to WaitForChange. Once a stream has overflowed its cap, the
// snapshot's length no longer grows 1:1 with real output, so a baseline
// derived from it would already be smaller than the live counters
// WaitForChange compares against, making WaitForChange return immediately
// instead of actually waiting for new output.
func (bs *BackgroundShell) TotalWrittenBytes() int {
	return bs.stdout.Len() + bs.stderr.Len()
}

// WaitForChange blocks until the job finishes, total written output (stdout+
// stderr combined, counted via each stream's monotonic writtenBytes counter —
// NOT the bounded resident snapshot, so this keeps working correctly even
// after a stream has been truncated) grows beyond sinceLen bytes, or ctx
// ends. sinceLen should come from TotalWrittenBytes, not from measuring a
// GetOutput snapshot. It polls the counters on a short ticker — there is no
// event bus for incremental writes, so the granularity is the ticker
// interval (250ms). Returns without error from any branch.
func (bs *BackgroundShell) WaitForChange(ctx context.Context, sinceLen int) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-bs.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if bs.stdout.Len()+bs.stderr.Len() > sinceLen {
				return
			}
		}
	}
}

// SetMaxJobs overrides this manager's concurrency cap. Intended for tests:
// exercising the limit's behaviour does not require paying the production
