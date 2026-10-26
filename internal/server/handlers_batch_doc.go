package server

// The flat Commands/Agents live-work projection remains on its existing wire
// and is now built from batch_nodes by handlers_livework_batch.go. Its
// incomplete-read marker prevents replacing previously shown work with a
// partial snapshot. H5 may retire this compatibility surface.
//
// Structured replacement:
//   - Push "session_batch_work" (EventSessionBatchWork) and request
//     "get_session_batch_work" (CmdGetSessionBatchWork), both carrying
//     BatchWorkSnapshotWire (v: 1): batch_nodes trees, counts, delivery state,
//     and structured completions.
//   - The web store merges incomplete snapshots and replaces complete ones.
//
// Left open:
//   - R15b: hierarchy renderer, output/held-input controls on batch nodes.
type batchDoc struct{}
