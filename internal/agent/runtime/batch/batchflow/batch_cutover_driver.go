package batchflow

import (
	"context"
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
