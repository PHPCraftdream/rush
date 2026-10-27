package session

// W1 bounded recovery core. Public signatures consumed by W2 (production
// wiring in internal/app and internal/agent). W2 must call exactly these;
// do not re-enumerate recovery roots elsewhere.
//
//	type BatchRecoveryLimits struct{ MaxRoots, PageSize int }
//	type BatchRecoveryCursor string
//	const BatchRecoveryCursorStart BatchRecoveryCursor = ""
//	func (s *BatchStore) BatchRecoverRoot(ctx context.Context, rootID string, probe BatchHostProbe) (BatchRecoveryOutcome, error)
//	func (s *BatchStore) BatchRecoverPass(ctx context.Context, probe BatchHostProbe, limits BatchRecoveryLimits, cursor BatchRecoveryCursor) (BatchRecoveryOutcome, BatchRecoveryCursor, bool, error)
//	func (s *BatchStore) RecoverHostsOnce(ctx context.Context, probe BatchHostProbe, hosts []string, maxRoots int) (BatchRecoveryOutcome, bool, error)
//
// Semantics shared with BatchRecoverDeadHost: only (HostStatusDead, nil probe
// error) permits recovery; Unknown/Alive/own host write nothing; each root
// commits separately through the per-root writer-election transaction.
// BatchRecoverPass returns done=true only when the whole candidate set has
// been scanned (cursor resets to BatchRecoveryCursorStart); done=false means
// the pass stopped early (budget or context) and cursor is resumable. The
// cursor is a keyset on root_id, so repeated passes rotate through the
// candidate set instead of rescanning from the beginning. RecoverHostsOnce
// returns more=true when its single root budget was exhausted before every
// dead host's roots were attempted; it never leaves a partially recovered
// root behind.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
)

const (
	defaultBatchRecoveryMaxRoots = 64
	defaultBatchRecoveryPageSize = 64
)

// BatchRecoveryCursor is an opaque keyset cursor over candidate root ids.
type BatchRecoveryCursor string

// BatchRecoveryCursorStart begins a fresh pass from the first candidate.
const BatchRecoveryCursorStart BatchRecoveryCursor = ""

// BatchRecoveryLimits bounds one pass. Non-positive values fall back to the
// package defaults (64 roots, 64 rows per page).
type BatchRecoveryLimits struct {
	MaxRoots int
	PageSize int
}

type batchHostVerdict struct {
	status HostLockStatus
	err    error
}

// batchRecoveryCandidateSQL mirrors the canonical BatchRecoveryRoots
// predicate (internal/db/sql/batch_recovery.sql) with its intentional CLI-root
// exclusion: CLI roots remain history until ordinary retention, while pending
// descendants and controls still select them for recovery.
const batchRecoveryCandidateSQL = `SELECT r.root_id, r.owner_session_id, r.host_id FROM batch_nodes r
WHERE r.node_id = r.root_id
  AND ((r.announced = 0 AND r.origin_cli = 0) OR EXISTS (
    SELECT 1 FROM batch_nodes n WHERE n.root_id = r.root_id
      AND n.state IN ('pending', 'starting', 'running', 'cancelling'))
    OR EXISTS (SELECT 1 FROM batch_control_requests c WHERE c.root_id = r.root_id AND c.status = 'pending'))`

// batchRecoveryCandidates scans at most limit candidate roots whose root_id
// sorts strictly after after, in deterministic root_id order.
func (s *BatchStore) batchRecoveryCandidates(ctx context.Context, after string, limit int) ([]db.BatchNode, error) {
	rows, err := s.writer.QueryContext(ctx, batchRecoveryCandidateSQL+`
  AND r.root_id > ? ORDER BY r.root_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.BatchNode
	for rows.Next() {
		var r db.BatchNode
		if err := rows.Scan(&r.RootID, &r.OwnerSessionID, &r.HostID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// batchRecoveryCandidateByID returns one candidate root row by exact id.
func (s *BatchStore) batchRecoveryCandidateByID(ctx context.Context, rootID string) (db.BatchNode, error) {
	var r db.BatchNode
	err := s.writer.QueryRowContext(ctx, batchRecoveryCandidateSQL+`
  AND r.root_id = ?`, rootID).Scan(&r.RootID, &r.OwnerSessionID, &r.HostID)
	return r, err
}

// batchRecoveryRootsForHost scans at most limit candidate roots of one host.
func (s *BatchStore) batchRecoveryRootsForHost(ctx context.Context, host string, limit int) ([]db.BatchNode, error) {
	rows, err := s.writer.QueryContext(ctx, batchRecoveryCandidateSQL+`
  AND r.host_id = ? ORDER BY r.root_id LIMIT ?`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.BatchNode
	for rows.Next() {
		var r db.BatchNode
		if err := rows.Scan(&r.RootID, &r.OwnerSessionID, &r.HostID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BatchRecoverRoot recovers exactly one named candidate root through the same
// per-root transaction (writer election, interrupt/notice/control semantics)
// that BatchRecoverDeadHost uses. A root that is no longer a recovery
// candidate, an empty or own host, or any probe verdict other than
// (HostStatusDead, nil) writes nothing; an unknown root id is a no-op, not an
// error.
func (s *BatchStore) BatchRecoverRoot(ctx context.Context, rootID string, probe BatchHostProbe) (BatchRecoveryOutcome, error) {
	var out BatchRecoveryOutcome
	if rootID == "" {
		return out, errors.New("batch recovery: missing root id")
	}
	if probe == nil {
		return out, errors.New("batch recovery: missing host probe")
	}
	root, err := s.batchRecoveryCandidateByID(ctx, rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if root.HostID == "" || IsOwnHostID(root.HostID) {
		return out, nil
	}
	status, perr := probe(root.HostID)
	if status != HostStatusDead || perr != nil {
		return out, perr
	}
	return s.batchRecoverRoot(ctx, root)
}

// BatchRecoverPass recovers dead-host roots within limits.MaxRoots attempts,
// probing each distinct host at most once per pass. The context is honored
// between roots: a cancelled or expired context stops cleanly with a
// resumable cursor and no partial-root state, because every root commits
// separately. Budget exhaustion also stops with a resumable cursor, positioned
// at the last processed root so the unattempted root is offered first on the
// next pass. done reports whether the full candidate set was scanned.
func (s *BatchStore) BatchRecoverPass(ctx context.Context, probe BatchHostProbe, limits BatchRecoveryLimits, cursor BatchRecoveryCursor) (BatchRecoveryOutcome, BatchRecoveryCursor, bool, error) {
	var total BatchRecoveryOutcome
	if probe == nil {
		return total, cursor, false, errors.New("batch recovery: missing host probe")
	}
	maxRoots := limits.MaxRoots
	if maxRoots <= 0 {
		maxRoots = defaultBatchRecoveryMaxRoots
	}
	pageSize := limits.PageSize
	if pageSize <= 0 {
		pageSize = defaultBatchRecoveryPageSize
	}
	attempted := 0
	probed := make(map[string]batchHostVerdict)
	processed := string(cursor)
	var errs []error
	for {
		page, err := s.batchRecoveryCandidates(ctx, processed, pageSize)
		if err != nil {
			return total, BatchRecoveryCursor(processed), false, err
		}
		if len(page) == 0 {
			return total, BatchRecoveryCursorStart, true, errors.Join(errs...)
		}
		for _, root := range page {
			if root.HostID == "" || IsOwnHostID(root.HostID) {
				processed = root.RootID
				continue
			}
			verdict, seen := probed[root.HostID]
			if !seen {
				status, perr := probe(root.HostID)
				verdict = batchHostVerdict{status: status, err: perr}
				probed[root.HostID] = verdict
				if perr != nil {
					errs = append(errs, fmt.Errorf("probe host %s: %w", root.HostID, perr))
				}
			}
			if verdict.status != HostStatusDead || verdict.err != nil {
				processed = root.RootID
				continue
			}
			if attempted >= maxRoots {
				return total, BatchRecoveryCursor(processed), false, errors.Join(errs...)
			}
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				return total, BatchRecoveryCursor(processed), false, errors.Join(errs...)
			}
			out, err := s.batchRecoverRoot(ctx, root)
			attempted++
			processed = root.RootID
			if err != nil {
				errs = append(errs, fmt.Errorf("recover root %s: %w", root.RootID, err))
				continue
			}
			total.Interrupted += out.Interrupted
			total.DeletedRoots += out.DeletedRoots
			total.Notices += out.Notices
			total.Controls += out.Controls
		}
		if len(page) < pageSize {
			return total, BatchRecoveryCursorStart, true, errors.Join(errs...)
		}
	}
}

// RecoverHostsOnce recovers the roots of the given dead-candidate hosts under
// one total budget of maxRoots root attempts. Hosts are probed in order, each
// distinct host once; empty and own hosts are skipped. more reports that the
// budget was exhausted before every dead host's roots were attempted; each
// attempted root commits separately, so nothing partial is ever written.
func (s *BatchStore) RecoverHostsOnce(ctx context.Context, probe BatchHostProbe, hosts []string, maxRoots int) (BatchRecoveryOutcome, bool, error) {
	var total BatchRecoveryOutcome
	if probe == nil {
		return total, false, errors.New("batch recovery: missing host probe")
	}
	if maxRoots <= 0 {
		maxRoots = defaultBatchRecoveryMaxRoots
	}
	more := false
	remaining := maxRoots
	seen := make(map[string]bool, len(hosts))
	var errs []error
	for _, host := range hosts {
		if host == "" || IsOwnHostID(host) || seen[host] {
			continue
		}
		seen[host] = true
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			return total, true, errors.Join(errs...)
		}
		status, perr := probe(host)
		if status != HostStatusDead || perr != nil {
			if perr != nil {
				errs = append(errs, fmt.Errorf("probe host %s: %w", host, perr))
			}
			continue
		}
		if remaining <= 0 {
			more = true
			break
		}
		roots, err := s.batchRecoveryRootsForHost(ctx, host, remaining+1)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(roots) > remaining {
			more = true
			roots = roots[:remaining]
		}
		for _, root := range roots {
			out, err := s.batchRecoverRoot(ctx, root)
			remaining--
			if err != nil {
				errs = append(errs, fmt.Errorf("recover root %s: %w", root.RootID, err))
				continue
			}
			total.Interrupted += out.Interrupted
			total.DeletedRoots += out.DeletedRoots
			total.Notices += out.Notices
			total.Controls += out.Controls
		}
	}
	return total, more, errors.Join(errs...)
}
