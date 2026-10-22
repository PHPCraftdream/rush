// Rerun stop routing is batch-only: the post-commit executor stop is
// Coordinator.BatchRerunStopVoided (batch_cutover_rerun.go), the retained
// implementation of the optional BatchRerunStopper surface. The legacy
// StopRerunJobs / []session.VoidedAsyncJob contract is gone — the durable
// reconciliation (session.BatchStore.BatchRerunReconcile) returns the
// exact-incarnation Voided node set the batch stop consumes.
package agent
