package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// BatchLiveProbe is one read-only live-attach lookup. The fences are exactly
// ReserveIdentity's attach block: input hash, owning host and Runner
// incarnation. A mismatch is an ERROR, never a resubmission. The
// AnnounceMessageID-derived log key names the generated started-ack
// message, not the caller's policy message id.
//
// SubmittedMessageID and SubmittedRegistryID are OPTIONAL probe diagnostics
// (review 4, live input mismatch): the caller may carry the invoking
// message id and pinned registry identity so the mismatch path can log
// stored-vs-submitted identity. Callers that do not supply them leave the
// fields empty; they are never matched against, only logged.
type BatchLiveProbe struct {
	Owner, Host, RunnerID, ToolCallID, InputHash string
	// SubmittedMessageID and SubmittedRegistryID are diagnostics only.
	SubmittedMessageID  string
	SubmittedRegistryID string
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
	detail := batchProbeHashDetail(live, children, p.InputHash)
	if !detail.direct && !detail.fallback {
		slog.Warn("batch probe live input mismatch",
			"root", live.RootID,
			"tool_call_id", p.ToolCallID,
			"stored_hash", live.InputHash.String,
			"submitted_hash", p.InputHash,
			"reconstructed_task_hash", detail.taskHash,
			"direct_match", detail.direct,
			"fallback_match", detail.fallback,
			"announce_message_id", live.AnnounceMessageID.String,
			"submitted_message_id", p.SubmittedMessageID,
			"submitted_registry_id", p.SubmittedRegistryID,
			"stored_registry_id", detail.storedRegistryID,
			"task_children", batchProbeTaskChildCount(live, children))
		return "", false, ErrBatchInputMismatch
	}
	slog.Debug("batch probe live attach",
		"root", live.RootID,
		"tool_call_id", p.ToolCallID,
		"match", batchProbeMatchName(detail),
		"announce_message_id", live.AnnounceMessageID.String,
		"submitted_message_id", p.SubmittedMessageID,
		"submitted_registry_id", p.SubmittedRegistryID,
		"stored_registry_id", detail.storedRegistryID)
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

// batchProbeTaskChildCount counts the root's direct task children for the
// mismatch diagnostics; it never reads payload content.
func batchProbeTaskChildCount(live db.BatchNode, children []db.BatchNode) int {
	count := 0
	for _, n := range children {
		if n.Kind == "task" && n.ParentID.Valid && n.ParentID.String == live.RootID {
			count++
		}
	}
	return count
}

// batchProbeMatchName names which hash comparison matched, for diagnostics.
func batchProbeMatchName(d batchProbeHashes) string {
	if d.direct {
		return "direct"
	}
	return "fallback"
}

// batchProbeHashes is the outcome of the probe's two hash comparisons,
// used both for the attach decision and the mismatch diagnostics.
type batchProbeHashes struct {
	direct   bool
	fallback bool
	taskHash string
	// storedRegistryID carries the stored task payload's registry_id for
	// mismatch diagnostics only; it is never read or logged otherwise.
	storedRegistryID string
}

// batchProbeHashDetail compares the probe hash against the reserved rows and
// reports which comparison matched plus the reconstructed task hash for
// diagnostics. The direct column match covers probes that already carry the
// commit hook's canonical Spec hash; the recomputed match covers probes whose
// hash is the canonical hash of the one-item TASK spec (batch_invocation.go).
// R11F round-2 (P1-7 item 7): the task kind and payload live ONLY on the
// root's single TASK CHILD row (a root row has task_kind NULL by schema
// CHECK), so the spec is reconstructed from that child — never from the
// root. Zero or several task children is ambiguous and fails closed. The
// reconstruction carries the child's stored group and timeout too, so a
// caller hashing the FULL task spec (jobbatch.TaskSpec Kind, Group, Payload
// and Timeout) matches; dropping them previously diverged from any such
// caller's hash.
func batchProbeHashDetail(live db.BatchNode, children []db.BatchNode, hash string) batchProbeHashes {
	detail := batchProbeHashes{direct: live.InputHash.String == hash}
	var task *db.BatchNode
	for i := range children {
		n := children[i]
		if n.NodeID == live.RootID || !n.ParentID.Valid || n.ParentID.String != live.RootID {
			continue
		}
		if n.Kind != "task" || task != nil {
			detail.taskHash = ""
			return detail
		}
		task = &children[i]
	}
	if task == nil || !task.TaskKind.Valid || !task.TaskPayload.Valid {
		return detail
	}
	detail.storedRegistryID = batchProbeStoredRegistryID(*task)
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind:    task.TaskKind.String,
		Group:   task.TaskGroup,
		Payload: json.RawMessage(task.TaskPayload.String),
		Timeout: time.Duration(task.TimeoutNs),
	}}
	detail.taskHash = batchCommitSpecHash(spec)
	detail.fallback = detail.taskHash == hash
	return detail
}

// BatchDeliveryOneItemCompletion resolves THE completion of a one-item batch
// (R11F P1-2). The R11B commit hook writes the Settled envelope on the
// settled NODE — for a one-item batch that is the single task child
// (root.1), not the root — so the read is: the root envelope when present,
// else the single task child's envelope. More than one child of the root
// with an unreadable stored envelope fails CLOSED (error), never a
// fabricated or partial completion; a multi-item root without envelopes is
// handled by the terminal derivation below. Read-only.
//
// Terminal-without-envelope roots (review 5a): when neither the root nor a
// single task child carries a response envelope, the completion is derived
// from ONE consistent snapshot read (the s.Read transaction above): the root
// AND every direct child must be terminal, otherwise the batch is live and
// ok=false. A ONE-ITEM root derives from its single TASK CHILD's own
// terminal outcome: the StartRejected outcome lives on the task (jobbatch
// engine.go EventStartRejected) and the root is only the fallback. A
// MULTI-ITEM root returns the ROOT-level outcome (root state/code/summary):
// a per-item summary would need a structured per-node list (node id, state,
// code per task), which this scalar state/code/summary envelope cannot carry
// without a shape change. No executor details, no media or metadata, no
// fabricated success; Details and Ref stay empty because no executor
// envelope exists. Accepted-execution envelopes are
// returned unchanged by the earlier reads above.
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
	if len(children) == 1 {
		if e, ok, err := batchDeliveryEnvelopeOf(children[0]); ok || err != nil {
			return e, ok, err
		}
	}
	// No envelope anywhere. A terminal root WITHOUT an executor response
	// (StartRejected, pending-stop terminal, recovery-interrupted) is
	// delivered honestly from persisted terminal facts; a live root or a
	// live task child stays live (ok=false) and keeps waiting.
	return batchTerminalCompletion(snap.Root, children)
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

// batchNodeTerminal reports whether one batch node row is in a final state.
// The set matches jobbatch's State.Terminal and the terminal-state SQL of
// batch_stop.go; pending, starting, running and cancelling are live.
func batchNodeTerminal(n db.BatchNode) bool {
	switch n.State {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// batchProbeStoredRegistryID extracts ONLY the registry_id metadata field
// from the stored task payload, for mismatch diagnostics; it never reads or
// logs any other payload content. Empty when absent or unparseable.
func batchProbeStoredRegistryID(task db.BatchNode) string {
	if !task.TaskPayload.Valid {
		return ""
	}
	var p struct {
		RegistryID string `json:"registry_id"`
	}
	if err := json.Unmarshal([]byte(task.TaskPayload.String), &p); err != nil {
		return ""
	}
	return p.RegistryID
}

// batchTerminalEnvelope builds the derived envelope from the primary row's
// terminal facts, falling back to the fallback row's facts when the primary
// carries none.
func batchTerminalEnvelope(primary, fallback db.BatchNode) BatchDeliveryEnvelope {
	code := primary.OutcomeCode
	if code == "" {
		code = fallback.OutcomeCode
	}
	summary := primary.OutcomeSummary
	if summary == "" {
		summary = fallback.OutcomeSummary
	}
	if summary == "" {
		summary = primary.Cause
	}
	if summary == "" {
		summary = fallback.Cause
	}
	if summary == "" {
		summary = code
	}
	if summary == "" {
		summary = primary.State + " without executor response"
	}
	return BatchDeliveryEnvelope{
		State:   primary.State,
		Code:    code,
		Summary: summary,
	}
}

// batchTerminalCompletion derives the honest, error-shaped completion of a
// terminal root that has no executor response envelope. It reports done only
// when the root AND every direct child are terminal in the SAME snapshot; a
// live row keeps the completion "not ready" (ok=false).
//
// A ONE-ITEM root derives from its single TASK CHILD's own terminal outcome:
// EventStartRejected assigns the rejected outcome/code to the task
// (jobbatch engine.go EventStartRejected) and structural settlement computes
// the root state WITHOUT copying the task outcome into the root, so the
// child's facts are the real terminal facts; the root's own outcome is the
// fallback. A MULTI-ITEM root returns the ROOT-level outcome: a per-item
// summary would need a structured per-node list (node id, state, code per
// task), which this scalar state/code/summary envelope cannot carry without
// a shape change; the root-level outcome is the only honest aggregate
// persisted for it. The summary falls back through the primary outcome
// summary, the fallback outcome summary, the persisted causes, the outcome
// code itself, and finally a plain "<state> without executor response"
// note. No executor details, no media or metadata, no fabricated success.
func batchTerminalCompletion(root db.BatchNode, children []db.BatchNode) (BatchDeliveryEnvelope, bool, error) {
	if !batchNodeTerminal(root) {
		return BatchDeliveryEnvelope{}, false, nil
	}
	for _, n := range children {
		if !batchNodeTerminal(n) {
			return BatchDeliveryEnvelope{}, false, nil
		}
	}
	if len(children) == 1 {
		return batchTerminalEnvelope(children[0], root), true, nil
	}
	return batchTerminalEnvelope(root, root), true, nil
}

// EnsureHost registers this store's host identity exactly once (the
// ensureHost lazy registration) and returns its id. R11F (M3): the flagged
// batch-runtime install needs the commit-hook identity BEFORE the first
// Claim, so the app boundary registers the host up front.
func (s *AsyncJobStore) EnsureHost(ctx context.Context) (string, error) {
	return s.ensureHost(ctx)
}
