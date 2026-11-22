// R12-F P1: the CLI loop's durable driver marker is the job-independent
// session.HostStore, which shutdown releases (the legacy AsyncJobStore
// marker -- whose host lock the drain gate never released, the CI Windows
// unlinkat leak -- is gone).
package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
)

// BatchDriverClaimer is the durable session-driver-marker surface
// *session.HostStore satisfies; the moved declaration lives in batchflow.
type BatchDriverClaimer = batchflow.BatchDriverClaimer

// driverMarkerStore is the durable driver-marker store the claim/release
// path consults: the injected HostStore. It returns a TRUE nil interface
// when no store is bound -- never a typed nil pointer -- so the caller's
// nil guard behaves like a concrete-pointer guard.
func (c *coordinator) driverMarkerStore() BatchDriverClaimer {
	return newBatchCutoverGlueComponent(c).driverMarkerStore()
}
