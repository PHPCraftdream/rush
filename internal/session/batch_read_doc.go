package session

/*
R10a additive readers are inert: the OLD async system remains the only production
terminal authority until cutover. In the new model the owning Runner chooses
live terminal outcomes; these APIs neither execute nor mutate projection,
history, host locks, control requests or R8 debt. A nonterminal root is live on
Alive, Unknown and Dead hosts alike. Only committed recovery changes that fact.
Unknown probes and unavailable reads set positive Complete flags false. Empty
zero values cannot prove absence of work. Missing captured incarnations are
Disappeared, never Finished. HasLive remains true for that observation.

Old -> new mapping table (navigation only, not compatibility wrappers):

| Old API / fact | Additive read API / disposition |
| --- | --- |
| LiveJob / liveJobOf | BatchLiveNode: owner/root/node refs, host/state/task facts, task counts; tool-call metadata belongs to root. |
| HostLiveness / probeHostSharedFn | Instance BatchLiveReader host configuration; own-host short circuit, shared probe cached per live call. Dead-host omission is NOT retained. |
| ListRunningAsyncJobsForOwners | BatchLiveNodesForOwners: all nodes of live roots, BFS chunks, Pending/Starting/Cancelling included. |
| LiveJobs | BatchLiveForSessions.Tree: root facts throughout session scope. |
| LiveOwnJobs | BatchLiveForSessions.Own / BatchLiveForOwner.Own: all owner roots, including delegations; no nondelegation-only await filter. |
| LiveDescendantJobs | Descendants: child_session_id node-link evidence within live roots; no session-parent traversal or originating-session cycle result. |
| LiveWorkForRoots / liveWorkForRoot / RootLiveWork | BatchLiveForSessions / batchLiveReplay / BatchLiveResult: shared forest, positive per-session completeness, own/tree/descendant distinctions, depth/chunk guards. |
| ListAsyncJobsForOwner / JobsInTree | Historical session consumers deferred to R10c; BatchLevel reads one incarnation, not session history. |
| GetAsyncHost | Display-only host metadata and display consumers deferred to R10c. |
| HasRunningDelegationFor / ListRunningForOwners | Live node/root facts available; missing incoming-delegation predicates and consumer policy facts deferred to R10b/R10c. |
| Worker owner/tool-call disappearance capture | BatchCaptured private refs, BatchCapturedLevel, BatchLevel: immutable entry refs, later Add visible, later roots excluded. Done/ANY/ALL/Notify await units/blocking deferred to R10b. |
| RunningOwnJobs / RunningDelegation arbiter and CLI facts | Completeness-aware HasLive inputs available; missing driver/policy facts, scope and once-wake consumer wiring deferred to R10b. |

BatchLevel uses a single SQL statement for a consistent whole-incarnation level
read. Children are ordered by ordinal; Subtree is pre-order with per-node task
aggregates, excluding structural batch double counting. Starting, Running and
Cancelling counts remain distinct; Active also aggregates those states. No R8
debt is inferred from terminal levels, notice selection, or host facts. Scope,
once-wakes, selected notice debt and captured targets remain independent. No
schema migration or existing caller change is needed. Connect/Release pairing
remains test-owned.
*/
