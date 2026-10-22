// Package session's R9 additions are inert. There are no production calls;
// the batch stores are the live execution authority since the H4 cutover (the
// legacy async_jobs store was deleted).
// Recovery never executes, resumes, cancels, writes history, or wakes work.
// A dead UUID cannot revive. Root writer election precedes all snapshot reads;
// node outcomes, one root notice and pending-control closure commit together.
// Existing terminal outcomes and selected outboxes are immutable to recovery.
// Retention deletes whole roots and identity links, never unrelated history.
//
// Retention also protects pending controls: unresolved requester intent is not
// disposable history. Fully terminal dead roots close controls without changing
// nodes/outboxes. The live pump answers terminal/stale races without callbacks.
// Age wrappers share the cutoff predicate; BatchRecoverOwnerScope is inert.
//
// Mapping table (every legacy recovery/question/GC function and host counterpart):
//
// | Legacy function | R9 counterpart or retained/dropped reason |
// | RecoverDeadHost | BatchRecoverDeadHost; root-atomic, all four live states. |
// | removeDeadHostFile | Retained unchanged; R9 never invokes old job-only reap. |
// | recoveredResultText | Dropped: host_lost fact, no inferred execution result. |
// | recoveredDelegationText | Dropped: committed envelopes remain; no prose read. |
// | recoveredQuestionText | Dropped: held input is not recovered completion. |
// | SweepDeadHosts | BatchSweepDeadHosts with empty owner filter. |
// | RecoverOwnerScope | BatchSweepDeadHosts with owner host-discovery filter. |
// | PurgeExpired | BatchPurgeExpired; old notice/driver/file paths unchanged. |
// | purgeEmptyDeadHostFiles | Retained unchanged, not called: unsafe job-only gate. |
// | purgeOrphanHostLockFiles | Retained unchanged; no new R9 file cleanup. |
// | reapOrphanHostLock | Retained unchanged; no new R9 orphan cleanup. |
// | CountJobsOlderThan | BatchCountExpired; same explicit cutoff/query as purge. (The old AsyncJobStore receiver was deleted at H4; sessions gc routes through the BatchStore wrappers.) |
// | PurgeJobsOlderThan | BatchPurgeExpired; whole roots, controls cascade. (Old receiver deleted at H4.) |
// | HostLockStatus.String | Retained shared three-way verdict display. |
// | HostsDir | Retained shared host infrastructure. |
// | HostLockPath | Retained shared host infrastructure. |
// | retainHostLockUntilExit | Retained shutdown fence, not R9 lifecycle owner. |
// | releaseRetainedHostLocksForTest | Retained legacy test cleanup. |
// | markOwnHostID | Retained process-local registration fence. |
// | unmarkOwnHostID | Retained registration/test cleanup. |
// | IsOwnHostID | Retained; R9 skips own-host recovery. |
// | ProbeHostLock | Retained exclusive removal primitive, not sweep authority. |
// | ProbeHost | Retained guarded exclusive primitive, not sweep authority. |
// | ProbeHostLockShared | Retained OS dead-only proof. |
// | ProbeHostShared | BatchSharedHostProbe adapter; no retained lock. |
// | acquireFreshHostLock | Retained registration pathname verification. |
// | RegisterHost | Retained; R9 does not register execution hosts. |
// | HostIdentity.Close | Retained unchanged; R9 never invokes job-only deletion. |
// | RemoveDeadHostFile | Retained verified unlink, no R9 invocation. |
// | AsyncHostStoreFromConn | Retained legacy registration adapter. |
//
// Stop requests bind owner/host/root/node exactly. Invalid foreign-key targets
// receive typed nonpersisted rejection because the schema cannot store them.
// Pending and Cancelling are live targets. Unknown probes write nothing;
// confirmed-dead requests sweep instead of invoking a cancellation callback.
// The owning host retains one serial BatchControlPump per Runner incarnation.
// Its callback fences exact identities and deduplicates request IDs, including
// a replacement pump after callback success but answer-write failure. Callback
// success means accepted committed transition, not physical termination. No
// writer tx spans the callback, which may project through the same writer.
// Persisted answered requests never reach the callback again. Runner wiring,
// authentication, scheduling and the fatal exit boundary remain out of scope.
package session
