// R12-F P1: with RUSH_BATCH_RUNTIME ON, the CLI loop's durable driver
// marker moves from the legacy AsyncJobStore (which registers a host lock
// the flag-ON drain gate never releases -- the CI Windows unlinkat leak) to
// the job-independent session.HostStore, which the flag-ON shutdown DOES
// release. Flag OFF nothing changes: the legacy store remains the marker
// store and claims/unclaims exactly as before.
package agent

import (
	"context"
	"errors"
)

// BatchDriverClaimer is the durable session-driver-marker surface both
// *session.AsyncJobStore (legacy, flag-OFF) and *session.HostStore
// (flag-ON) satisfy: ClaimSessionDriver records this store's host as
// sessionID's driver, ReleaseSessionDriver deletes it. Both return
// session.ErrDriverMarkerUnavailable for the same "no OS-lock-capable data
// dir" case, so the coordinator's handling is identical for either store.
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
// path consults: the injected HostStore when flag-ON wiring set one, else
// the legacy AsyncJobStore (flag-OFF behaviour, byte-identical), else nil.
// It returns a TRUE nil interface when no store exists -- never a typed nil
// pointer -- so the caller's nil guard behaves like the former concrete-
// pointer guard.
func (c *coordinator) driverMarkerStore() BatchDriverClaimer {
	if c.batchDriverClaimer != nil {
		return c.batchDriverClaimer
	}
	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		return c.asyncJobs.store
	}
	return nil
}
