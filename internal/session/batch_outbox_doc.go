package session

/*
Batch outbox consumer mapping (R8, inert; no production caller wiring).

Only accepted Engine selections create completion debt. Root announcement
covers internal nodes. Pull rechecks the gate and earlier pending sequence /
ordinal entries inside its CAS, not only while listing. Ordering is per root:
sequences belong to its owning Runner; unrelated Runner sequences are not a
global clock. A failed earlier insert keeps later notices of that root blocked.
History insertion, delivery identity and ack/inline election commit together;
publication follows commit. No method settles execution or accepts stop intent.
Inline is eligible only before ack, for one selected terminal one-task root.
The caller supplies its elected completion envelope, never a control answer.

Full function inventory from the three legacy files follows. Names on the
left are legacy functions, not wrappers used by this implementation.

notice_pull.go:
  PullJobNotices -> BatchPullNotices.
  pullOneJobNotice -> batchPullOneNotice (root/node identity and ordered CAS).
  PullSessionNotices -> retained unchanged (SessionNoticeStore carries it
    since H4); binding delivery deferred to cutover.
  pullOneSessionNotice -> retained unchanged; not duplicated in this package.
  sessionNoticeVoidCondition -> retained unchanged (SessionNoticeStore's
    injected batch facts replace the async_jobs reads at H4). No Batch lookup
    is hidden
    behind job_tool_call_id. Child-question binding delivery is later-cutover
    work; no node question metadata or binding schema is invented here.
  InsertSessionNotice -> retained unchanged (existing notice infrastructure).
  AnnounceStarted -> BatchAnnounceStarted (root-incarnation ack election).
  AnnounceJobKillResult -> dropped: stop intent is not completion delivery.
  AnnounceInlineResult -> BatchAnnounceInlineResult (one-task root election;
    losing consumers do not insert extra history, unlike legacy fallback).
  ListSessionNotices -> retained unchanged (existing notice reader).
  RependJobKillRowWithoutNotice -> dropped: no premature stop settlement or
    historyless done interval exists in the new consumer transactions.

async_job_reaction.go:
  ReactionDebtExists -> BatchReactionDebtExists.
  VisibleReactionDebtExists -> BatchReactionVisibleDebtExists.
  HasRunningDelegationFor -> deferred to R10 node/live-root readers: Pending,
    Starting and Cancelling are not replaced with a running-only predicate.
  MarkReactedWithMessageUpdate -> BatchReactionMarkReactedWithMessageUpdate.
    Caller eligibility remains real content, including an awaiting question;
    error/cancel/empty and compaction finishes do not count. Both selected
    completion rows and retained session_notices react without job lookup.
  DebtSnapshot.Empty -> BatchDebtSnapshot.Empty.
  CaptureDebtSnapshot -> BatchReactionCaptureDebtSnapshot / batchDebtSnapshot.
  PendingInclusiveDebtSummary -> BatchReactionPendingInclusiveDebtSummary.
  IncrementWakeAttempts -> BatchReactionIncrementWakeAttempts.
  MaxWakeAttempts -> BatchReactionMaxWakeAttempts.
  snapshotRowStillDebt -> SQL BatchOutboxAccountNode / AccountNotice plus
    identity-set intersection in BatchReactionMaxWakeAttempts: owner, root,
    node and delivered message are fenced, not a reused tool-call string.
  SettleReactedFailed -> BatchReactionSettleReactedFailed.
  settleSnapshotRows -> batchReactionAccountSnapshot / batchReactionAccount.
  SettleReactedFailedWithMarker -> BatchReactionSettleReactedFailedWithMarker.
  ListReactedFailedText -> BatchReactionListReactedFailedText.
  SetWakeZeroForOwners -> BatchReactionSetWakeZeroForOwners.
  ListRunningForOwners -> deferred to R10 nonterminal root/node readers.
  HostNotDead -> retained host infrastructure; no Batch writer host registration
    or liveness authority is added to consumer transactions.
  hostNotDeadFor -> retained host infrastructure, same reason.
  ReactionDebtOwners -> BatchReactionDebtOwners.

notice_row_ids.go:
  InsertSessionNoticeReturningID -> retained unchanged; AUTOINCREMENT notice
    IDs still identify retained question/wake/marker infrastructure.
  PendingInclusiveDebtRows -> BatchReactionPendingInclusiveDebtRows. Selected
    root/node refs replace archived claim strings; notice IDs/kinds remain.

New multistep consumer operations use one transaction across all participating
rows. Failed ack leaves the controllable reserved identity untouched and cannot
repeat execution. Envelope persistence and Engine-to-projection wiring belong
to other owners; snapshots expose the existing response-envelope reference.
Recovery, retention, rerun/reset, host probes, await, bound-notice delivery and
production activation are later packages. At the H4 cutover the legacy
async_jobs store and its callers were deleted; the batch stores are the sole
execution authority. No migration is required for R8.
*/
