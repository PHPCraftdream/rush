// The run-queue row id travels to the coordinator through the execution
// context (R12-H2b §2.4): the pump leases a row and hands only its
// serialized call data across the Coordinator.Run boundary, but a durable
// continuation's rebuild must be able to release the row-keyed
// Continuation holder the enqueue producer took — and that release is
// keyed by the ROW's durable id, which only the pump knows.
package session

import "context"

// runQueueRowIDContextKey carries the leased run-queue entry's id.
type runQueueRowIDContextKey struct{}

// WithRunQueueRowID stamps the leased row's durable id onto the
// execution context the pump hands to Coordinator.Run.
func WithRunQueueRowID(ctx context.Context, rowID string) context.Context {
	return context.WithValue(ctx, runQueueRowIDContextKey{}, rowID)
}

// RunQueueRowIDFrom returns the leased row id the pump stamped (or "" for
// callers outside the pump path — a rebuild that cannot know the row id
// simply skips the row-keyed holder release, which is safe when absent).
func RunQueueRowIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(runQueueRowIDContextKey{}).(string)
	return id
}
