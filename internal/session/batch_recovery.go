package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// BatchHostProbe is a three-way, dead-only authority. Host UUIDs never revive.
type BatchHostProbe func(string) (HostLockStatus, error)

// BatchSharedHostProbe adapts the real hermetic OS shared-lock probe.
func BatchSharedHostProbe(dataDir string) BatchHostProbe {
	return func(host string) (HostLockStatus, error) { return ProbeHostShared(dataDir, host) }
}

type BatchRecoveryOutcome struct{ Interrupted, DeletedRoots, Notices, Controls int64 }

// BatchRecoverDeadHost is inert: no production caller, execution or history write.
// Each root commits separately; failures are returned without blocking other roots.
func (s *BatchStore) BatchRecoverDeadHost(ctx context.Context, host string, probe BatchHostProbe) (BatchRecoveryOutcome, error) {
	var total BatchRecoveryOutcome
	if host == "" || IsOwnHostID(host) {
		return total, nil
	}
	if probe == nil {
		return total, errors.New("batch recovery: missing host probe")
	}
	status, err := probe(host)
	if status != HostStatusDead || err != nil {
		return total, err
	}
	roots, err := db.New(s.writer).BatchRecoveryRoots(ctx)
	if err != nil {
		return total, err
	}
	var errs []error
	for _, r := range roots {
		if r.HostID != host {
			continue
		}
		out, err := s.batchRecoverRoot(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover root %s: %w", r.RootID, err))
			continue
		}
		total.Interrupted += out.Interrupted
		total.DeletedRoots += out.DeletedRoots
		total.Notices += out.Notices
		total.Controls += out.Controls
	}
	return total, errors.Join(errs...)
}

func (s *BatchStore) batchRecoverRoot(ctx context.Context, listed db.BatchNode) (BatchRecoveryOutcome, error) {
	var out BatchRecoveryOutcome
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	r, err := q.BatchRecoveryElect(ctx, db.BatchRecoveryElectParams{RootID: listed.RootID, Owner: listed.OwnerSessionID, Host: listed.HostID})
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if r.Announced == 0 {
		out.DeletedRoots, err = q.DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{RootID: r.RootID, NodeID: r.NodeID, OwnerSessionID: r.OwnerSessionID, HostID: r.HostID})
	} else {
		nodes, e := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: r.RootID, OwnerSessionID: r.OwnerSessionID})
		if e != nil {
			return out, e
		}
		live := false
		sequence := r.RootSequence
		for _, n := range nodes {
			live = live || batchStateLive(n.State)
			if n.ProjectionSequence > sequence {
				sequence = n.ProjectionSequence
			}
		}
		if !live {
			if sequence <= 0 {
				sequence = 1
			}
			out.Controls, err = q.BatchRecoveryCloseControls(ctx, db.BatchRecoveryCloseControlsParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})
			if err != nil {
				return BatchRecoveryOutcome{}, err
			}
			if err = tx.Commit(); err != nil {
				return BatchRecoveryOutcome{}, err
			}
			return out, nil
		}
		sequence++
		now := time.Now().Unix()
		out.Interrupted, err = q.BatchRecoveryInterrupt(ctx, db.BatchRecoveryInterruptParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sequence, Now: now})
		if err != nil {
			return BatchRecoveryOutcome{}, err
		}
		// A terminal root keeps its outcome and projection sequence unchanged.
		if batchStateLive(r.State) {
			err = batchRequireRow(q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: r.RootID, OwnerSessionID: r.OwnerSessionID, HostID: r.HostID, Sequence: sequence, UpdatedAt: now}))
			if err != nil {
				return BatchRecoveryOutcome{}, err
			}
		}
		out.Notices, err = q.BatchRecoveryNotice(ctx, db.BatchRecoveryNoticeParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: now})
		if err != nil {
			return BatchRecoveryOutcome{}, err
		}
		out.Controls, err = q.BatchRecoveryCloseControls(ctx, db.BatchRecoveryCloseControlsParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: sql.NullInt64{Int64: now, Valid: true}})
	}
	if err != nil {
		return BatchRecoveryOutcome{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchRecoveryOutcome{}, err
	}
	return out, nil
}

// BatchSweepDeadHosts discovers hosts from any live descendant or unannounced root.
// An owner filter selects hosts, not subsets of a host's recovery authority.
func (s *BatchStore) BatchSweepDeadHosts(ctx context.Context, owner string, probe BatchHostProbe) (map[string]BatchRecoveryOutcome, error) {
	roots, err := db.New(s.writer).BatchRecoveryRoots(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := map[string]BatchRecoveryOutcome{}
	var errs []error
	for _, r := range roots {
		if seen[r.HostID] || (owner != "" && r.OwnerSessionID != owner) {
			continue
		}
		seen[r.HostID] = true
		result, e := s.BatchRecoverDeadHost(ctx, r.HostID, probe)
		if e != nil {
			errs = append(errs, e)
		}
		if result != (BatchRecoveryOutcome{}) {
			out[r.HostID] = result
		}
	}
	return out, errors.Join(errs...)
}

// BatchRecoverOwnerScope discovers recovery hosts from this owner only.
func (s *BatchStore) BatchRecoverOwnerScope(ctx context.Context, owner string, probe BatchHostProbe) (map[string]BatchRecoveryOutcome, error) {
	if owner == "" {
		return map[string]BatchRecoveryOutcome{}, nil
	}
	return s.BatchSweepDeadHosts(ctx, owner, probe)
}
