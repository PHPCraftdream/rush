package server

// R15a browser-surface map (handlers_batch.go): what the old live-work path
// exposed, what the structured batch path replaces it with, and what is
// deliberately left open.
//
// Old path (unchanged when RUSH_BATCH_RUNTIME is off):
//   - Push "session_live_work" (EventSessionLiveWork): flat Commands/Agents
//     rows built from async_jobs (handlers_livework.go, liveWorkPusher).
//   - Request "get_session_live_work" (CmdGetSessionLiveWork): the same
//     snapshot on demand.
//   - Wire LiveWorkItemWire / SessionLiveWorkPayload (protocol.go).
//   - Completions derived client-side by PARSING notice prose
//     (web/src/asyncJobCompletion.ts, consumed via store.ts's
//     $asyncJobCompletions) — fragile by construction.
//
// Structured replacement (flag on only):
//   - Push "session_batch_work" (EventSessionBatchWork) and request
//     "get_session_batch_work" (CmdGetSessionBatchWork), both carrying
//     BatchWorkSnapshotWire (v: 1): full batch_nodes trees for every
//     nonterminal root, per-root counts, R8 notice-delivery state, and
//     structured BatchCompletionWire entries (outcome code + response
//     envelope ref read from columns — no text parsing anywhere).
//   - Client store web/src/batchWork.ts: incomplete snapshots MERGE into the
//     last good snapshot, complete ones REPLACE; a fetch failure delivers
//     no snapshot at all, so live rows never disappear on a read error.
//   - Dedupe rule (web): a flat live-work row whose toolCallID matches a
//     snapshot node's toolCallID is hidden from the merged view (empty
//     toolCallIDs never dedupe).
//   - R12-C: terminal-root completion delivery — Completions also carry
//     every TERMINAL root incarnation of the visited sessions (NodeID ==
//     RootID, keyed by root_id, state + outcome summary columns, deduped),
//     read from batch_nodes history; a reload re-reads them from the
//     structured store. No prose parsing, no legacy SQL. With the flag ON
//     the flat session_live_work snapshot is likewise built from batch
//     nodes only (handlers_livework_batch.go) and carries complete=false
//     on an incomplete read, so previously shown active work is kept.
//
// Left open:
//   - R15b: hierarchy renderer, output/held-input controls on batch nodes.
type batchDoc struct{}
