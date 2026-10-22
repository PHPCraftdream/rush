package session

// R10c chunk 1 — history and activity consumers: inventory, mapping table and
// the chunk 2/3 store-function skeleton. INERT: nothing here is called by
// production code; the batch stores are the live execution authority since
// the H4 cutover (the legacy async_jobs store was deleted). This file contains
// only documentation comments, typed
// error declarations and small shared types; chunk 2/3 implement the named
// functions in batch_reset.go / batch_rerun.go / batch_owner_delete.go /
// batch_history.go using internal/db/sql/batch_history.sql plus the existing
// batch_nodes/outbox/read/recovery/control query files.

// GREP INVENTORY (exact commands, run 2026-10-12 in this worktree; counts =
// matching non-test, non-internal/db-generated lines; batch_history.sql
// carries 11 queries, not the 10 once inventoried):
//
//   grep -rn "AsyncJob\|async_jobs\|session_notices" --include="*.go" internal cmd \
//     | grep -v _test.go | grep -v "^internal/db" | awk -F: '{print $1}' | sort | uniq -c
//
//   grep -rn "AsyncJob\|async_jobs\|session_notices\|AsyncDataRetention\|ResetSession\|sessions reset" ... (same filters)
//
// Consumer files and hit counts (second command's counts):
//   58 async_job_store.go  55 async_job_reaction.go  40 notice_pull.go
//   21 async_job_recovery.go  21 async_job_reader.go  14 async_job_rerun.go
//   13 async_job_gc.go  12 handlers_livework.go  12 sessions_gc.go
//   10 sessions_show.go  9 sessions_why.go  9 sessions_audit_rules.go
//   8 work_ledger_announce.go  7 sessions_audit_db.go  7 app_accessors.go
//   7 app.go  7 agent_notice_pull.go  6 async_job_reader_batch.go
//   6 work_ledger.go  5 session_driver.go  5 session.go  5 notice_row_ids.go
//   5 host_lock.go  5 async_job_bgshell.go  5 sessions_jobs.go
//   5 coordinator_background.go  4 protocol.go  4 app_session_activity.go
//   4 sessions_reset.go  4 sessions_kill.go  3 sessions_list.go
//   3 work_ledger_inline.go  3 coordinator_drain_policy.go  3 coordinator.go
//   3 bgshell_claim.go  2 wake_schedule_fire.go  2 session_child_question.go
//   2 message.go  2 app_run_async_wait.go  2 work_ledger_timeout.go
//   2 coordinator_tools.go  2 agent_turn_step.go and 1-hit files:
//   sessions_compact.go sessions_audit_findings.go command_journal.go
//   app_session_drivers.go app_recovery.go app_agent_setup.go
//   work_ledger_transition.go work_ledger_pending.go work_ledger_driver.go
//   wake_scheduler.go turn_arbiter.go job_shell_resolver.go
//   job_output_pending.go supervision.go coordinator_wake.go
//   coordinator_rerun.go coordinator_bgshell_cap.go coordinator_await_tasks.go
//   agent_turn.go agent_options.go agent.go session_lifecycle.go
//   session_activity.go session_purge_queue.go wake_schedule_store.go
//   session_update.go async_job_store_testhook.go handlers_agent_rerun.go
//
// MAPPING TABLE — every consumer group with its node-row replacement. Files
// marked [R8]/[R9]/[R10a] already exist; batch_* names below are the chunk
// 2/3 skeleton signatures documented after the table. "Dropped at R12" means
// the old-path code is deleted by the cutover without a replacement.

// | Consumer (file) | Today | Replacement / disposition | Proving test (chunk 2/3) |
// | --- | --- | --- | --- |
// | internal/session/async_job_reset.go ResetOwnerHistory + ErrResetJobsRunning | Wipes messages, refuses while running jobs exist, voids undelivered rows, cancels wake schedules | BatchResetOwnerHistory (below): refusal typed via BatchResetRefusal, check+delete in ONE tx; batch sides covered in the same tx | TestBatchResetRefusedPending/Cancelling; TestBatchResetAtomicWipe |
// | internal/cmd/sessions_reset.go | CLI caller of ResetOwnerHistory | keeps calling legacy path until R12; then rewired to BatchResetOwnerHistory — needs production rewiring at R12 | (rewire covered by grep gate at R12; refusal behavior above) |
// | internal/session/async_job_rerun.go TruncateForRerun | One tx deletes tail messages, repends/voids async_jobs + session_notices, returns Voided set | BatchRerunReconcile (below): same message tx shape, node-row repend/void via BatchRerun{Repend,Void}NodesByNoticeMessageIDs / VoidRootsBy{AnnounceMessage,ToolCall}IDs | TestBatchRerunVoidDeletedHistory; TestBatchRerunRependDoneNotice |
// | internal/agent/coordinator_rerun.go StopRerunJobs + work_ledger_rerun.go stopToolCallsForRerun | Post-commit stop of voided running jobs, scoped to owner + tool_call ids | BatchRerunStopVoided (agent package, internal/agent/batch_* — other chunk): stop scoped to EXACT (owner, root_id) incarnation; late old-incarnation completion can never reach a new incarnation (I5) | TestBatchRerunIncarnationScopedStop (agent chunk); session-side TestBatchRerunVoidPreservesIncarnation |
// | internal/session/session_lifecycle.go service.Delete (55-155) | Deletes messages/files/session row; async_jobs/session_notices/batch rows go by FK ON DELETE CASCADE; NO refusal of live work (observed) | BatchOwnerDeleteHistory (below) mirrors old behavior: cascade, no refusal, recorded decision D-R10c-1 | TestBatchOwnerDeleteCascade; TestBatchOwnerDeleteRemovesControls |
// | internal/session/async_job_reader.go + async_job_reader_batch.go | sessions jobs/why/show/list/audit host facts, trees, chunked traversal | BatchHistorySnapshot / BatchHistoryRoots / BatchHistoryLevel (below) on top of R10a BatchLiveReader + BatchLevelNodes; no duplication of R10a readers | TestBatchHistorySnapshotCounts; TestBatchHistoryRootsForOwner |
// | internal/cmd/sessions_jobs.go, sessions_why.go, sessions_show.go, sessions_list.go | CLI readers of the reader API above | consume BatchHistorySnapshot at R12 rewiring — needs production rewiring at R12 | snapshot tests above |
// | internal/cmd/sessions_gc.go (CountJobsOlderThan/PurgeJobsOlderThan) | operator retention one-shot | BatchCountExpired / BatchPurgeExpired [R9, batch_retention.go] — reuse, do not duplicate | existing batch_retention_test.go |
// | internal/cmd/sessions_audit_db.go / sessions_audit_rules.go / sessions_audit_findings.go | raw SQL over async_jobs; missing old table disables the rule | batch audit queries BatchHistoryStateCounts (batch_history.sql) at R12 — needs production rewiring at R12 | TestBatchHistoryStateCounts |
// | internal/server/handlers_livework.go, handlers_sessions.go, protocol.go | flat livework snapshot from async_jobs | structured node snapshot [R15a, other package] reading BatchHistorySnapshot — needs production rewiring at R15a/R12 | R15a server tests |
// | internal/server/handlers_agent_rerun.go | rerun alias -> TruncateForRerun + StopRerunJobs | BatchRerunReconcile + BatchRerunStopVoided at R12 — needs production rewiring at R12 | rerun tests above |
// | internal/agent/work_ledger*.go, bgshell_claim.go, notice_pull.go, async_job_reaction.go, notice_row_ids.go | claim/terminal CAS, ack, inline, reaction debt | [R8 batch_outbox_doc.go mapping: BatchPullNotices, BatchAnnounceStarted, BatchAnnounceInlineResult, BatchReaction*] — done, inert | existing batch_pull/reaction tests |
// | internal/session/async_job_recovery.go, async_job_gc.go | host-death sweep, retention | [R9 batch_recovery_doc.go mapping: BatchRecoverDeadHost, BatchSweepDeadHosts, BatchPurgeExpired, BatchCountExpired] — done, inert | existing batch_recovery/retention tests |
// | internal/agent/turn_arbiter.go, coordinator_await_tasks.go, coordinator_drain_policy.go, cli_active_work.go | debt/live-work gates, captured pairs | [R10a readers done; await mapping is R10b, not this package] | R10b tests |
// | internal/session/wake_schedule_fire.go, wake_scheduler.go, session_child_question.go, session_driver.go, host_lock.go | retained notice/host/wake infrastructure | retained unchanged; explicitly implemented test-only utility BatchDisposeLegacyBoundNotices (below), referenced by tests; NOT production-wired | TestBatchDisposeLegacyBoundNotices |
// | async_jobs writer core (async_job_store.go, async_job_bgshell.go) | claim/archive/terminal CAS | dropped at R12: replaced by Reserve/Project [R5, batch_store.go]; no compatibility wrapper | existing batch_store tests |
// | work_ledger_announce.go / work_ledger_inline.go / work_ledger_pending.go | ack, durable fallback, controller routing | dropped at R12: replaced by BatchAnnounce* [R8] and durable control requests [R9] | existing R8/R9 tests |
// | tools/job_output.go, tools/job_kill.go | controller tools | dropped at R12 (D6): replaced by batch_status/output/stop [R13] | R13 tests |
// | supervision.go / delegation_question.go / work_ledger_timeout.go question+wake-only notices | supervision notices, held input | retained session_notices infrastructure until cutover; node question delivery is R8 reaction + R7c adapter scope — needs production rewiring at R12/R7c | R7c/R8 tests |
// | app.go / app_accessors.go / app_session_activity.go / app_run_async*.go / app_recovery.go / app_agent_setup.go / app_session_drivers.go | store accessors, activity, recovery wiring | composition roots; rewired to Batch* stores at R11/R12 — needs production rewiring at R11/R12 | R11 wiring tests |
// | internal/cmd/sessions_watch.go (~457, watch tick via App.SessionActivity) | periodic activity tick | needs production rewiring at R11F/R12: App.SessionActivity / classifier input switches to BatchLiveReader + BatchHistorySnapshot | R11F wiring tests |
// | internal/cmd/sessions_tail.go (~194) | live tail reader | needs production rewiring at R11F/R12: App.SessionActivity / classifier input switches to BatchLiveReader + BatchHistorySnapshot | R11F wiring tests |
// | internal/cmd/sessions_inject.go (~108) | live-work gate before inject | needs production rewiring at R11F/R12: App.SessionActivity / classifier input switches to BatchLiveReader + BatchHistorySnapshot | R11F wiring tests |
// | internal/cmd/sessions_cancel.go (~128, sessionHasLiveWork) | live-work refusal gate | needs production rewiring at R11F/R12: App.SessionActivity / classifier input switches to BatchLiveReader + BatchHistorySnapshot | R11F wiring tests |
// | internal/cmd/sessions_locks.go (~262, SessionActivityBatch) | batched activity for lock listing | needs production rewiring at R11F/R12: App.SessionActivity / classifier input switches to BatchLiveReader + BatchHistorySnapshot | R11F wiring tests |

// DECISIONS (smallest conservative choice; marked where old behavior was
// ambiguous):
//
// D-R10c-1 (owner deletion): OLD code refuses nothing — session_service.Delete
// deletes messages/files/session row and lets FKs cascade async_jobs and
// session_notices, even while rows are running; the owner's own process stops
// its in-process work separately. BatchOwnerDeleteHistory mirrors that: no
// live-root refusal, whole-owner node deletion via BatchResetDeleteOwnerNodes
// in one tx (batch_control_requests cascades by FK). A live Runner writing
// after deletion loses its rows and fails Project with
// ErrBatchProjectionConflict — containment by failure, never a synthesized
// settlement. The refusal error below is reserved for RESET only.
//
// D-R10c-2 (reset refusal scope): refusal covers ANY nonterminal root of the
// owner: pending, starting, running, cancelling — cancelling includes
// "stopped but awaiting termination" (projection stays nonterminal until the
// confirmed report; I8). Refusal carries the first offending root's identity
// and state, writes nothing.
//
// D-R10c-3 (delivery reconciliation states): a selected node whose notice
// message was deleted reconciles to exactly one of: pending (repend, done ->
// pending, counters reset), void (never delivered to surviving history), or
// untouched done (message still exists). A node can never remain "pending
// forever" because of deleted history (repend re-pends it deliberately), and
// "done" is never left for a nonexistent message. Disappeared INCARNATIONS
// (rows deleted) need no reconciliation: nothing can reference a debt reader
// through a deleted root; debt readers fence (owner, root, node, message).
//
// D-R10c-4 (incarnation scoping): every rerun stop/void is fenced by
// (owner, root_id); tool_call_id matching is restricted to roots without an
// announce message (legacy arm), never to a live incarnation that already
// acked. Late completion/ack/stop of an old incarnation cannot settle or ack
// a new one: all write paths key root_id, and the unique live key index
// permits the new incarnation's reservation only because the old root is
// terminal or its notice is void.
//
// D-R10c-5 (legacy bound notices): the explicitly implemented test-only
// BatchDisposeLegacyBoundNotices utility voids a bound session_notices row
// exactly once (delivery IN ('pending','done') guard) and zeroes wake so debt
// readers cannot count it twice; a reacted notice is left as history. This
// utility is referenced by tests and is NOT production-wired; this decision
// does not describe production disposal of retained bound notices.
//
// OPEN DECISIONS (deferred with reason):
// - Whether session reset should also cancel wake_schedules for batch roots
//   (old code cancels them for legacy rows). Left to chunk 2: the batch model
//   has no wake_schedule rows of its own yet; smallest choice is to reuse the
//   existing CancelAllWakeSchedulesForOwner unchanged.
// - Whether sessions jobs/list should show terminal batch roots alongside
//   legacy rows pre-cutover. Chunk 2/3 snapshot functions report node rows
//   only; presentation mixing is a CLI decision at R12.
// - response_envelope_ref exposure shape for `sessions show` (raw ref vs
//   resolved envelope) is undecided; chunk 2 exposes the raw ref.

// CHUNK 2/3 STORE-FUNCTION SKELETON (names and signatures only; bodies live
// in the chunk 2/3 files listed above; nothing here is implemented or called):
//
// (a) Atomic reset (batch_reset.go)
//   func (s *BatchStore) BatchResetOwnerHistory(ctx context.Context, messages
//       message.Service, owner string) (ResetOutcome, error)
//     One tx: ListLiveBatchRootsForOwner -> BatchResetRefusal on first
//     nonterminal root (nothing written); else delete owner messages
//     (message.Service.DeleteTx), BatchResetDeleteOwnerNodes, void legacy
//     undelivered session_notices, cancel wake schedules, commit; publish
//     deleted events only after commit. Mirrors async_job_reset.go.
//   func batchResetLiveRoots(ctx context.Context, q *db.Queries, owner string)
//       ([]db.BatchNode, error) — tx-internal refusal scan.
//
// (b) Selected-node delivery reconciliation (batch_rerun.go)
//   type BatchRerunVoidedNode struct {
//       RootID, NodeID, ToolCallID, State, ChildSessionID, HostID string }
//   func (s *BatchStore) BatchRerunReconcile(ctx context.Context,
//       messages message.Service, p RerunTruncateParams)
//       (RerunTruncation, error)
//     Same shape as TruncateForRerun: one tx deletes [target]+tail, then
//     repends done nodes whose notice message was deleted
//     (BatchRerunRependNodesByNoticeMessageIDs), voids the rest
//     (BatchRerunVoidNodesByNoticeMessageIDs, then roots by announce message
//     and by legacy tool_call_id), returns the voided roots for post-commit
//     stop. Order matters: repend BEFORE void (a node matching both ends
//     void). ErrRerunTargetGone rolls everything back.
//
// (c) Incarnation-scoped post-commit stop routing (agent side,
//     internal/agent/batch_* — sibling chunk; session side exposes only data)
//   func BatchRerunVoidedRoots(...) []BatchRerunVoidedNode — data for the
//     agent's stop, each carrying its exact root_id.
//
// (d) Owner/session deletion (batch_owner_delete.go)
//   func (s *BatchStore) BatchOwnerDeleteHistory(ctx context.Context,
//       messages message.Service, owner string) (ResetOutcome, error)
//     One tx: delete owner messages, BatchResetDeleteOwnerNodes (controls
//     cascade by FK), void legacy undelivered session_notices; NO live-root
//     refusal (D-R10c-1). The session row itself is deleted by
//     session.Service.Delete, whose FKs cascade any remainder.
//
// (e) Explicitly implemented test-only legacy bound-notice disposal utility
//   func (s *BatchStore) BatchDisposeLegacyBoundNotices(ctx context.Context,
//       owner, toolCallID string) (int64, error)
//     Referenced by tests; NOT production-wired. It calls
//     BatchListBoundNoticesByToolCallID for the read, then
//     BatchDisposeBoundNoticesByToolCallID; idempotent (void once, wake 0).
//
// (f) Activity snapshots (batch_history.go; read-only, reuses R10a readers)
//   type BatchHistorySnapshot struct {
//       Root    db.BatchNode            // root row incl. delivery/debt cols
//       Nodes   []BatchLevelNodeView    // per-node: BatchLiveNode + delivery
//       Counts  BatchLiveCounts         // task counts, structural not doubled
//       Debt    BatchDebtSnapshot       // visible debt via R8 snapshot
//       Complete bool                   // conservative-read rule (R10a)
//   }
//   func (r *BatchLiveReader) BatchHistoryLevel(ctx context.Context,
//       ref BatchLiveRef) (BatchHistorySnapshot, error)
//     BatchLevel + delivery/debt columns of the same rows; host verdict via
//     the injectable ProbeHost (R10a); Disappeared surfaced, never Finished.
//   func (r *BatchLiveReader) BatchHistoryRoots(ctx context.Context,
//       owner string) ([]BatchHistorySnapshot, error)
//     BatchHistoryRootsForOwner + per-root BatchHistoryLevel.
//   func (r *BatchLiveReader) BatchHistoryCounts(ctx context.Context,
//       owner string) (map[string]int64, error)
//     BatchHistoryStateCounts for sessions audit/jobs summaries.
//
// SCHEMA NOTES: every query above is expressible on the R5 tables, EXCEPT
// the recorded D-R10c-live decision: the batch_nodes CHECK constraint
// (20261012000001_batch_projection.sql) forbids delivery='void' (and wake=1)
// on notice_selected=0 rows, so a void WRITE to an ordinary live root is a
// schema violation; live old incarnations retire via post-commit stop
// routing (BatchRerunLiveRootsBy{AnnounceMessage,ToolCall}IDs), not via
// delivery. batch_control_requests cascades from batch_nodes root rows
// and from sessions; session_notices.job_tool_call_id is textual (no FK), so
// disposal matching is by tool_call_id equality — same weakness as the old
// repoint logic, recorded, not fixed here.

import "errors"

// ErrBatchResetRefused is the sentinel wrapped by every reset refusal: the
// owner still owns a nonterminal root and nothing was written.
var ErrBatchResetRefused = errors.New("batch reset: the session still owns a nonterminal batch root")

// BatchResetRefusal names the offending incarnation. It satisfies error and
// matches ErrBatchResetRefused via errors.Is.
type BatchResetRefusal struct {
	Owner      string
	RootID     string
	ToolCallID string
	State      string // pending | starting | running | cancelling
}

func (e *BatchResetRefusal) Error() string {
	return ErrBatchResetRefused.Error() + " (" + e.State + " root " + e.RootID + " tool call " + e.ToolCallID + ")"
}

func (e *BatchResetRefusal) Unwrap() error { return ErrBatchResetRefused }

// BatchHistoryNodeView is one node of an activity snapshot: R10a live facts
// plus that node's delivery/debt columns.
type BatchHistoryNodeView struct {
	Fact      BatchLiveNode
	Delivery  string // none | pending | done | void
	Wake      bool
	Reacted   bool
	MessageID string // notice_message_id, empty when unmailed
}
