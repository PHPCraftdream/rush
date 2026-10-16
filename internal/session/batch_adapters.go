package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// BatchLiveProbe is one read-only live-attach lookup. The fences are exactly
// ReserveIdentity's attach block: input hash, owning host and Runner
// incarnation. A mismatch is an ERROR, never a resubmission.
type BatchLiveProbe struct {
	Owner, Host, RunnerID, ToolCallID, InputHash string
}

// BatchProbeLive reports the live root reserved for (owner, tool call id)
// under the caller's fences, WITHOUT inserting or projecting anything: the
// commit hook reserves at engine Submit time, so a probe must stay read-only
// or a first caller would create two reservations. attached=false with a nil
// error means no live root exists; the caller submits.
func (s *BatchStore) BatchProbeLive(ctx context.Context, p BatchLiveProbe) (string, bool, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	live, err := db.New(tx).GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{OwnerSessionID: p.Owner, ToolCallID: sql.NullString{String: p.ToolCallID, Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// R11F round-2 (P1-7 item 7): the schema forces task_kind/task_payload to
	// NULL on the kind='batch' ROOT row, so the one-item TASK spec can only be
	// reconstructed from the root's TASK CHILD row(s). Read them inside the
	// same read-only tx.
	children, err := db.New(tx).ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: live.RootID, OwnerSessionID: p.Owner})
	if err != nil {
		return "", false, err
	}
	if !batchProbeHashMatches(live, children, p.InputHash) {
		return "", false, ErrBatchInputMismatch
	}
	if live.HostID != p.Host {
		return "", false, ErrBatchForeignLiveHost
	}
	if live.RunnerID != p.RunnerID {
		return "", false, ErrBatchRunnerMismatch
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return live.RootID, true, nil
}

// batchProbeHashMatches compares the probe hash against the reserved rows.
// The direct column match covers probes that already carry the commit hook's
// canonical Spec hash; the recomputed match covers ingress probes whose hash
// is the canonical hash of the one-item TASK spec (batch_invocation.go).
// R11F round-2 (P1-7 item 7): the task kind and payload live ONLY on the
// root's single TASK CHILD row (a root row has task_kind NULL by schema
// CHECK), so the spec is reconstructed from that child — never from the
// root. Zero or several task children is ambiguous and fails closed.
func batchProbeHashMatches(live db.BatchNode, children []db.BatchNode, hash string) bool {
	if live.InputHash.String == hash {
		return true
	}
	var task *db.BatchNode
	for i := range children {
		n := children[i]
		if n.NodeID == live.RootID || !n.ParentID.Valid || n.ParentID.String != live.RootID {
			continue
		}
		if n.Kind != "task" || task != nil {
			return false
		}
		task = &children[i]
	}
	if task == nil || !task.TaskKind.Valid || !task.TaskPayload.Valid {
		return false
	}
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind:    task.TaskKind.String,
		Payload: json.RawMessage(task.TaskPayload.String),
	}}
	return batchCommitSpecHash(spec) == hash
}

// BatchDeliveryOneItemCompletion resolves THE completion of a one-item batch
// (R11F P1-2). The R11B commit hook writes the Settled envelope on the
// settled NODE — for a one-item batch that is the single task child
// (root.1), not the root — so the read is: the root envelope when present,
// else the single task child's envelope. More than one child of the root,
// or a child with an unreadable stored envelope, is ambiguous and FAILS
// CLOSED (error), never a fabricated or partial completion. Read-only.
func (s *BatchStore) BatchDeliveryOneItemCompletion(ctx context.Context, owner, root string) (BatchDeliveryEnvelope, bool, error) {
	snap, err := s.Read(ctx, owner, root)
	if err != nil {
		// A root row that does not exist (yet, or ever — e.g. a foreign or
		// unknown root) is "no persisted completion", NOT an error: the
		// batchDoneSource contract (batch_delivery.go LoadDone) reads a
		// persisted completion without waiting and reports ok=false when
		// none exists, and AwaitDone's durable poll must keep waiting on
		// such a root until ctx cancel instead of treating it as terminal.
		// This no-rows error can only originate from the FIRST query of
		// batchSnapshot (GetBatchRoot, a :one query on the root row,
		// batch_nodes.sql); the second query (ListBatchNodes) is :many and
		// returns an empty slice, never sql.ErrNoRows. So errors.Is
		// sql.ErrNoRows here unambiguously means "root row absent".
		if errors.Is(err, sql.ErrNoRows) {
			return BatchDeliveryEnvelope{}, false, nil
		}
		return BatchDeliveryEnvelope{}, false, err
	}
	if e, ok, err := batchDeliveryEnvelopeOf(snap.Root); ok || err != nil {
		return e, ok, err
	}
	var children []db.BatchNode
	for _, n := range snap.Nodes {
		if n.NodeID != root && n.ParentID.Valid && n.ParentID.String == root {
			children = append(children, n)
		}
	}
	if len(children) != 1 {
		// Ambiguous (zero children, or a multi-item structure): the caller
		// must not guess which envelope is the completion.
		return BatchDeliveryEnvelope{}, false, nil
	}
	return batchDeliveryEnvelopeOf(children[0])
}

// batchDeliveryEnvelopeOf decodes one node's stored response envelope.
// ok=false when the node has no envelope yet (or the column is empty).
func batchDeliveryEnvelopeOf(n db.BatchNode) (BatchDeliveryEnvelope, bool, error) {
	if !n.ResponseEnvelope.Valid || n.ResponseEnvelope.String == "" {
		return BatchDeliveryEnvelope{}, false, nil
	}
	var e BatchDeliveryEnvelope
	if err := json.Unmarshal([]byte(n.ResponseEnvelope.String), &e); err != nil {
		return BatchDeliveryEnvelope{}, false, err
	}
	e.Ref = n.ResponseEnvelopeRef.String
	return e, true, nil
}

// EnsureHost registers this store's host identity exactly once (the
// ensureHost lazy registration) and returns its id. R11F (M3): the flagged
// batch-runtime install needs the commit-hook identity BEFORE the first
// Claim, so the app boundary registers the host up front.
func (s *AsyncJobStore) EnsureHost(ctx context.Context) (string, error) {
	return s.ensureHost(ctx)
}
