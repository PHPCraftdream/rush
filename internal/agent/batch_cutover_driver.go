// R12-F P1: the CLI loop's durable driver marker is the job-independent
// session.HostStore, which shutdown releases (the legacy AsyncJobStore
// marker -- whose host lock the drain gate never released, the CI Windows
// unlinkat leak -- is gone).
package agent

import (
	"context"
	"errors"
)

// BatchDriverClaimer is the durable session-driver-marker surface
// *session.HostStore satisfies: ClaimSessionDriver records this store's host
// as sessionID's driver, ReleaseSessionDriver deletes it. It returns
// session.ErrDriverMarkerUnavailable for the "no OS-lock-capable data dir"
// case, so the coordinator's handling is uniform.
type BatchDriverClaimer interface {
	ClaimSessionDriver(ctx context.Context, sessionID string) error
	ReleaseSessionDriver(ctx context.Context, sessionID string) error
}

// SetBatchDriverClaimer wires the driver-marker store onto a coordinator
// (internal/app composition root). Fails closed on nil receiver or store.
func SetBatchDriverClaimer(c Coordinator, s BatchDriverClaimer) error {
	if c == nil {
		return errors.New("batch driver claimer: nil coordinator")
	}
	if s == nil {
		return errors.New("batch driver claimer: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("batch driver claimer: unsupported coordinator type")
	}
	co.batchDriverClaimer = s
	return nil
}

// driverMarkerStore is the durable driver-marker store the claim/release
// path consults: the injected HostStore. It returns a TRUE nil interface
// when no store is bound -- never a typed nil pointer -- so the caller's
// nil guard behaves like a concrete-pointer guard.
func (c *coordinator) driverMarkerStore() BatchDriverClaimer {
	if c == nil || c.batchDriverClaimer == nil {
		return nil
	}
	return c.batchDriverClaimer
}
