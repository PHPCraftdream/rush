package sessionbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"

	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// Package session's R9 additions are inert. There are no production calls;
// the batch stores are the live execution authority since the H4 cutover (the
// legacy async_jobs store was deleted).
// Recovery never executes, resumes, cancels, writes history, or wakes work.
// A dead UUID cannot revive. Root writer election precedes all snapshot reads;
// node outcomes, one root notice and pending-control closure commit together.
// Existing terminal outcomes and selected outboxes are immutable to recovery.
// Retention deletes whole roots and identity links, never unrelated history.
//
// Retention also protects pending controls: unresolved requester intent is not
// disposable history. Fully terminal dead roots close controls without changing
// nodes/outboxes. The live pump answers terminal/stale races without callbacks.
// Age wrappers share the cutoff predicate; BatchRecoverOwnerScope is inert.
//
// Mapping table (every legacy recovery/question/GC function and host counterpart):
//
// | Legacy function | R9 counterpart or retained/dropped reason |
// | RecoverDeadHost | BatchRecoverDeadHost; root-atomic, all four live states. |
// | removeDeadHostFile | Retained unchanged; R9 never invokes old job-only reap. |
// | recoveredResultText | Dropped: host_lost fact, no inferred execution result. |
// | recoveredDelegationText | Dropped: committed envelopes remain; no prose read. |
// | recoveredQuestionText | Dropped: held input is not recovered completion. |
// | SweepDeadHosts | BatchSweepDeadHosts with empty owner filter. |
// | RecoverOwnerScope | BatchSweepDeadHosts with owner host-discovery filter. |
// | PurgeExpired | BatchPurgeExpired; old notice/driver/file paths unchanged. |
// | purgeEmptyDeadHostFiles | Retained unchanged, not called: unsafe job-only gate. |
// | purgeOrphanHostLockFiles | Retained unchanged; no new R9 file cleanup. |
// | reapOrphanHostLock | Retained unchanged; no new R9 orphan cleanup. |
// | CountJobsOlderThan | BatchCountExpired; same explicit cutoff/query as purge. (The old AsyncJobStore receiver was deleted at H4; sessions gc routes through the BatchStore wrappers.) |
// | PurgeJobsOlderThan | BatchPurgeExpired; whole roots, controls cascade. (Old receiver deleted at H4.) |
// | HostLockStatus.String | Retained shared three-way verdict display. |
// | HostsDir | Retained shared host infrastructure. |
// | HostLockPath | Retained shared host infrastructure. |
// | retainHostLockUntilExit | Retained shutdown fence, not R9 lifecycle owner. |
// | releaseRetainedHostLocksForTest | Retained legacy test cleanup. |
// | markOwnHostID | Retained process-local registration fence. |
// | unmarkOwnHostID | Retained registration/test cleanup. |
// | IsOwnHostID | Retained; R9 skips own-host recovery. |
// | ProbeHostLock | Retained exclusive removal primitive, not sweep authority. |
// | ProbeHost | Retained guarded exclusive primitive, not sweep authority. |
// | ProbeHostLockShared | Retained OS dead-only proof. |
// | ProbeHostShared | BatchSharedHostProbe adapter; no retained lock. |
// | acquireFreshHostLock | Retained registration pathname verification. |
// | RegisterHost | Retained; R9 does not register execution hosts. |
// | HostIdentity.Close | Retained unchanged; R9 never invokes job-only deletion. |
// | RemoveDeadHostFile | Retained verified unlink, no R9 invocation. |
// | AsyncHostStoreFromConn | Retained legacy registration adapter. |
//
// Stop requests bind owner/host/root/node exactly. Invalid foreign-key targets
// receive typed nonpersisted rejection because the schema cannot store them.
// Pending and Cancelling are live targets. Unknown probes write nothing;
// confirmed-dead requests sweep instead of invoking a cancellation callback.
// The owning host retains one serial BatchControlPump per Runner incarnation.
// Its callback fences exact identities and deduplicates request IDs, including
// a replacement pump after callback success but answer-write failure. Callback
// success means accepted committed transition, not physical termination. No
// writer tx spans the callback, which may project through the same writer.
// Persisted answered requests never reach the callback again. Runner wiring,
// authentication, scheduling and the fatal exit boundary remain out of scope.

// BatchHostProbe is a three-way, dead-only authority. Host UUIDs never revive.
type BatchHostProbe func(string) (sessionhosts.HostLockStatus, error)

// BatchSharedHostProbe adapts the real hermetic OS shared-lock probe.
func BatchSharedHostProbe(dataDir string) BatchHostProbe {
	return func(host string) (sessionhosts.HostLockStatus, error) {
		return sessionhosts.ProbeHostShared(dataDir, host)
	}
}

type BatchRecoveryOutcome struct{ Interrupted, DeletedRoots, Notices, Controls int64 }

// BatchRecoverDeadHost returns announced work to durable history and deletes
// unannounced non-CLI reservations. CLI roots retain their durable identity
// even before an announcement; live CLI descendants are still interrupted.
// Each root commits separately; failures are returned without blocking other roots.
func (s *BatchStore) BatchRecoverDeadHost(ctx context.Context, host string, probe BatchHostProbe) (BatchRecoveryOutcome, error) {
	var total BatchRecoveryOutcome
	if host == "" || sessionhosts.IsOwnHostID(host) {
		return total, nil
	}
	if probe == nil {
		return total, errors.New("batch recovery: missing host probe")
	}
	status, err := probe(host)
	if status != sessionhosts.HostStatusDead || err != nil {
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
	if r.Announced == 0 && r.OriginCli == 0 {
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
			err = session.BatchRequireRow(q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: r.RootID, OwnerSessionID: r.OwnerSessionID, HostID: r.HostID, RunnerID: r.RunnerID, Sequence: sequence, UpdatedAt: now}))
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
