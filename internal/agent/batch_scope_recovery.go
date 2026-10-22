package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// Scope-time batch recovery budgets (H4 W2, review item 1c): ONE total
// recovery budget and at most ONE candidate root attempt per scope call.
// On budget exhaustion or any failure the caller's answer stays
// conservatively live; the App-owned background maintenance pass
// (internal/app/app_batch_maintenance.go) completes the rest later.
// batchScopeRecoveryBudget: Var only so tests can zero it for a
// deterministic budget-exhaustion run; production never writes it.
var (
	batchScopeRecoveryBudget   = 10 * time.Millisecond
	batchScopeRecoveryMaxRoots = 1
)

// batchScopeRecoverySource is the store-and-probe pair extracted from the
// coordinator's bound node-backed fact source. The zero source recovers
// nothing: no binding, a conservative unknown source, a nil store or a nil
// live reader all mean no ON recovery is possible, and the caller keeps
// its pre-hook answer unchanged.
type batchScopeRecoverySource struct {
	facts BatchCutoverFacts
	store *session.BatchStore
	probe session.BatchHostProbe
}

// batchScopeRecoverySourceOf extracts the recovery source from c's bound
// facts. Only the node-backed source (nodeBatchCutoverFacts) carries a
// store; the live reader's own probe and data dir are reused so the
// recovery probe is exactly the predicate the scope read used.
func batchScopeRecoverySourceOf(c *coordinator) batchScopeRecoverySource {
	facts := coordinatorBatchCutoverFacts(c)
	nf, ok := facts.(nodeBatchCutoverFacts)
	if !ok || nf.store == nil || nf.live == nil || nf.live.DataDir == "" || nf.live.ProbeHost == nil {
		return batchScopeRecoverySource{}
	}
	probeHost, dataDir := nf.live.ProbeHost, nf.live.DataDir
	return batchScopeRecoverySource{
		facts: facts,
		store: nf.store,
		probe: func(host string) (session.HostLockStatus, error) {
			return probeHost(dataDir, host)
		},
	}
}

// recoverDeadCandidates recovers the dead hosts this owner's live scope
// just observed, outside any read transaction: dedupe the hosts whose
// observed verdict is Dead, then ONE bounded RecoverHostsOnce with a
// single-root budget. Dead hosts stay live until recovery commits, so a
// failed, cancelled or budget-exhausted attempt is simply retried by the
// background maintenance pass; it never changes the caller's answer by
// itself. Best-effort: errors are dropped, never propagated.
func (s batchScopeRecoverySource) recoverDeadCandidates(ctx context.Context, owner string) {
	if s.store == nil || s.probe == nil || owner == "" {
		return
	}
	nodes, err := s.facts.LiveNodes(ctx, owner)
	if err != nil {
		// An unreadable scope read must not select recovery targets.
		return
	}
	seen := make(map[string]bool)
	var hosts []string
	for _, n := range nodes {
		if n.HostID == "" || seen[n.HostID] || n.HostStatus != session.HostStatusDead {
			continue
		}
		seen[n.HostID] = true
		hosts = append(hosts, n.HostID)
	}
	if len(hosts) == 0 {
		return
	}
	recCtx, cancel := context.WithTimeout(ctx, batchScopeRecoveryBudget)
	defer cancel()
	_, _, _ = s.store.RecoverHostsOnce(recCtx, s.probe, hosts, batchScopeRecoveryMaxRoots)
}

// recoverBatchScopeDeadCandidates is the ONE scope-time recovery hook for
// the flag-ON paths (H4 W2): the turn preamble BEFORE the notice pull,
// runningWorkOpen, CLIActiveWork's batch branch, and nothing else.
// Ordinary output/status/live-work rendering must not trigger recovery.
func recoverBatchScopeDeadCandidates(ctx context.Context, c *coordinator, owner string) {
	batchScopeRecoverySourceOf(c).recoverDeadCandidates(ctx, owner)
}
