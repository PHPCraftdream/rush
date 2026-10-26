package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchCancelControlTimeout bounds the Stop submission. Detached from the
// caller's (possibly cancelled) context so the engine always receives the
// request; a Stop acknowledgement never settles, so no result is awaited.
var batchCancelControlTimeout = 30 * time.Second

// batchCancelOutcome is the caller's prompt cancellation result. Settled is
// ALWAYS false: a Stop acknowledgement is not a completion and never settles
// the root; its real report settles it later through the engine.
type batchCancelOutcome struct {
	// Submitted reports whether the engine accepted the Stop request.
	Submitted bool
	// Err is the Stop submission error, if any.
	Err error
	// Settled is always false (held termination keeps the admission unit
	// occupied until the root's REAL report).
	Settled bool
}

// cancelBatchCall implements sync caller cancellation: Runner.Stop is
// submitted under a BOUNDED, NON-CANCELLED control context detached from the
// caller's context (its own timeout), so a cancelled caller still cancels the
// root. The caller returns promptly with the outcome while the root settles
// on its own; the root's admission/execution stays live until its real report
// — cancellation with held termination cannot release admission.
func cancelBatchCall(caller context.Context, runner batchRunnerHandle, root jobbatch.ID) batchCancelOutcome {
	// Deliberately NOT the caller's ctx: a cancelled caller must still
	// deliver the Stop request (mutation target: returning caller directly
	// makes the submission fail exactly when it is needed most).
	ctl := context.WithoutCancel(caller)
	ctl, cancel := context.WithTimeout(ctl, batchCancelControlTimeout)
	defer cancel()
	err := runner.Stop(ctl, root)
	return batchCancelOutcome{Submitted: err == nil, Err: err, Settled: false}
}
