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
	if !batchProbeHashMatches(live, p.InputHash) {
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

// batchProbeHashMatches compares the probe hash against the reserved row.
// The direct column match covers probes that already carry the commit hook's
// canonical Spec hash; the recomputed match covers ingress probes whose hash
// is the canonical hash of the one-item TASK spec (batch_invocation.go),
// which ReserveIdentity stores indirectly as task_kind plus task_payload.
func batchProbeHashMatches(live db.BatchNode, hash string) bool {
	if live.InputHash.String == hash {
		return true
	}
	if !live.TaskKind.Valid || !live.TaskPayload.Valid {
		return false
	}
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind:    live.TaskKind.String,
		Payload: json.RawMessage(live.TaskPayload.String),
	}}
	return batchCommitSpecHash(spec) == hash
}

// EnsureHost registers this store's host identity exactly once (the
// ensureHost lazy registration) and returns its id. R11F (M3): the flagged
// batch-runtime install needs the commit-hook identity BEFORE the first
// Claim, so the app boundary registers the host up front.
func (s *AsyncJobStore) EnsureHost(ctx context.Context) (string, error) {
	return s.ensureHost(ctx)
}
