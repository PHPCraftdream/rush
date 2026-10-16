package agent

import "context"

// awaitSync blocks until job's outcome is ready (deliverLocked closed
// job.done) or ctx is done. job.sync must be true. Reads job.result under
// l.mu -- deliverLocked writes it (via transitionToTerminal) under the same
// lock before closing done, so there is no torn read.
func (l *workLedger) awaitSync(ctx context.Context, job *asyncJob) (jobResult, error) {
	select {
	case <-job.done:
		// A cancelled caller always gets the context error: when the
		// cancellation also ended the executor, both channels are ready and
		// select would otherwise pick between them at random.
		if err := ctx.Err(); err != nil {
			return jobResult{}, err
		}
		l.mu.Lock()
		res := job.result
		l.mu.Unlock()
		return res, nil
	case <-ctx.Done():
		// The job's own executor context is EITHER ctx itself (sync path,
		// §4.2) or derived from it -- cancelling ctx already cancels the
		// executor, which will reach finalize/deliverLocked on its own and
		// eventually close job.done. This branch does not need to cancel
		// anything itself; it only stops THIS caller from waiting further.
		return jobResult{}, ctx.Err()
	}
}
