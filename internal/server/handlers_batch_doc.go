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
//
// Left open:
//   - R15b: hierarchy renderer, output/held-input controls on batch nodes.
//   - R12: root-level terminal completion delivery — terminal ROOTS are not
//     returned by the live-node read (BatchLiveNodesForOwners returns only
//     roots in pending/starting/running/cancelling), so a root finishing its
//     whole tree is delivered by the notice/history path as before; this
//     snapshot carries terminal NODES inside still-live roots only.
type batchDoc struct{}
