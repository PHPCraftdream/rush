package appbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
)

// Batch fact readers for the app layer's activity
// and quota surfaces: reaction debt, live work, host
// liveness and finished-work export read ONLY batch node state
// (batch_nodes and its debt predicates). Nothing here reads async_jobs.
// The readers build their own handles from the App's pools, so they work
// on an App with no batch runtime installed (no provider configured).

// BatchWorkFacts is the live-work shape the classifier consumes:
// the counts and the unreadable flags.
type BatchWorkFacts struct {
	Own, Descendants      int
	DescendantSessionIDs  []string
	OwnIncomplete         bool
	DescendantsIncomplete bool
}

// CutoverWriter is the pool the batch debt store opens its read-only
// transactions on (same pool choice as BatchCLIStore). Nil without a DB.
func CutoverWriter(p BatchCLIParams) *sql.DB {
	if p.DB == nil {
		return nil
	}
	writer := p.DB()
	if writer == nil {
		return nil
	}
	if conns := p.DBConns(); len(conns) > 0 && conns[0] != nil {
		writer = conns[0]
	}
	return writer
}

// CutoverReader returns the read pool the batch live reader runs on
// (nil falls back to the writer, like BatchCLIStore).
func CutoverReader(p BatchCLIParams, writer *sql.DB) *sql.DB {
	if read := p.ReadDB(); read != nil {
		return read
	}
	return writer
}

// BatchCutoverDebtOwners answers the pending-inclusive reaction-debt fact
// from batch node/outbox predicates only. A nil pool answers "no debt"
// (the configless-App rule), never an error.
func BatchCutoverDebtOwners(p BatchCLIParams, ctx context.Context, ids []string) (map[string]bool, error) {
	writer := CutoverWriter(p)
	if writer == nil {
		return map[string]bool{}, nil
	}
	return session.NewBatchStore(writer).BatchReactionDebtOwners(ctx, ids)
}

// BatchCutoverLiveWork derives the live own-work and live-delegation facts
// from batch nodes: BatchLiveForSessions walks the owner's live roots and,
// delegation links included, the child-session forest in one shared BFS.
// Host death never settles nodes (dead hosts stay live until recovery
// commits), so there is no dead-host-running-rows fact under the batch
// authority; a failed or Unknown-host read stays incomplete and surfaces
// as an Unreadable entry, never as "nothing running".
func BatchCutoverLiveWork(p BatchCLIParams, ctx context.Context, ids []string) map[string]BatchWorkFacts {
	out := make(map[string]BatchWorkFacts, len(ids))
	writer := CutoverWriter(p)
	if writer == nil {
		return out
	}
	reader := session.NewBatchLiveReader(db.New(CutoverReader(p, writer)), p.DataDir())
	// TxStarter: every live read runs in ONE read-only tx on the writer.
	reader.TxStarter = writer
	for id, r := range reader.BatchLiveForSessions(ctx, ids) {
		f := BatchWorkFacts{
			Own:                   len(r.Own),
			Descendants:           len(r.Descendants),
			OwnIncomplete:         !r.OwnComplete,
			DescendantsIncomplete: !r.Complete,
		}
		seen := make(map[string]struct{}, len(r.Descendants))
		for _, n := range r.Descendants {
			child := n.ChildSessionID
			if child == "" {
				continue
			}
			if _, dup := seen[child]; dup {
				continue
			}
			seen[child] = struct{}{}
			f.DescendantSessionIDs = append(f.DescendantSessionIDs, child)
		}
		out[id] = f
	}
	return out
}

// BatchCutoverHostLiveness decides host liveness for the driver
// facts with the shared, non-acquiring probe (the same predicate
// BatchLiveReader uses). A host id
// registered as this process's own is Alive by definition (the shared
// probe refuses to answer it with ErrProbeOwnHost); foreign hosts keep
// the conservative probe verdict.
func BatchCutoverHostLiveness(p BatchCLIParams) func(string) sessionhosts.HostLockStatus {
	return func(hostID string) sessionhosts.HostLockStatus {
		if hostID == "" || p.DataDir() == "" {
			return session.HostStatusUnknown
		}
		if sessionhosts.IsOwnHostID(hostID) {
			return session.HostStatusAlive
		}
		st, err := sessionhosts.ProbeHostShared(p.DataDir(), hostID)
		if err != nil {
			return session.HostStatusUnknown
		}
		if st != session.HostStatusAlive && st != session.HostStatusDead {
			return session.HostStatusUnknown
		}
		return st
	}
}

// QuotaWorkRow is the finished-work row the quota terminal snapshot
// consumes. Key is the per-node identity the quota terminal snapshot dedupes
// on (the root/node incarnation); Announced mirrors the root row's
// announcement (the tree's delivery is announced by the root).
type QuotaWorkRow struct {
	OwnerSessionID string
	Key            string
	ToolCallID     string
	State          string
	Announced      bool
	Result         string
	ChildSessionID string
}

// BatchFinishedWork exports every task node of the session's batch tree
// (owner roots and, via child_session_id, the descendants' roots --
// terminal roots INCLUDED, which is why the enumeration reads
// BatchHistoryRootsForOwner, not the live-roots query). Roots and nested
// structural batch nodes are structure, never work, so they are not
// exported. A one-task root keeps the original tool-call id as the
// exported identity; a multi-task root exports each task by its node id.
// The root/node Key keeps incarnations distinct for the quota terminal
// latch. Errors report incomplete, never a partial silent answer.
func BatchFinishedWork(p BatchCLIParams, ctx context.Context, sessionID string) ([]QuotaWorkRow, bool, error) {
	conn := p.ActivityConn()
	if conn == nil {
		return nil, false, nil
	}
	visited := map[string]bool{sessionID: true}
	frontier := []string{sessionID}
	var out []QuotaWorkRow
	for depth := 0; depth <= batchReadMaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, owner := range frontier {
			roots, err := conn.BatchHistoryRootsForOwner(ctx, owner)
			if err != nil {
				return nil, true, fmt.Errorf("quota exit: batch finished work: roots: %w", err)
			}
			for _, root := range roots {
				nodes, err := conn.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: root.OwnerSessionID, RootID: root.RootID})
				if err != nil {
					return nil, true, fmt.Errorf("quota exit: batch finished work: nodes: %w", err)
				}
				announced := root.Announced != 0
				// Export only task nodes; the root and nested batch nodes
				// are structure. The child-link visiting must still run for
				// every node so terminal descendant sessions stay reachable.
				var tasks []db.BatchNode
				for _, n := range nodes {
					child := n.ChildSessionID.String
					if n.ChildSessionID.Valid && child != "" && !visited[child] {
						visited[child] = true
						next = append(next, child)
					}
					if n.Kind == "task" {
						tasks = append(tasks, n)
					}
				}
				// An ordinary one-task root (the OneItem wrapper of a direct
				// tool call) keeps the original tool-call id as its exported
				// identity; a multi-task root exports each task by its node
				// id. The Key stays root/node, so a reused id in a new root
				// incarnation still yields a distinct row.
				oneTask := len(tasks) == 1 && root.ToolCallID.Valid && root.ToolCallID.String != ""
				for _, n := range tasks {
					jobID := n.NodeID
					if oneTask {
						jobID = root.ToolCallID.String
					}
					out = append(out, QuotaWorkRow{
						OwnerSessionID: n.OwnerSessionID,
						Key:            n.RootID + "/" + n.NodeID,
						ToolCallID:     jobID,
						State:          n.State,
						Announced:      announced,
						Result:         n.OutcomeSummary,
						ChildSessionID: n.ChildSessionID.String,
					})
				}
			}
		}
		frontier = next
	}
	// A depth-bounded walk must never read as complete (same rule as the
	// session live-work walk).
	if len(frontier) > 0 {
		return out, true, nil
	}
	return out, false, nil
}

// batchReadMaxDepth matches the session-side batch walk depth bound.
const batchReadMaxDepth = 16

// Owner is the identity key of the package-level batch registries
// (batchHostStores, batchNoticeStores, batchMaintenanceLoops). Its pointer
// identity replaces the old *App key; it carries no state because the
// registry code reads dataDir and the DB pools from the composition root
// at the same lazy moments as before. The blank byte is load-bearing: a
// zero-size struct makes every &Owner{} the same address
// (runtime.zerobase), so all roots would share one registry key and a
// later root would adopt an earlier root's HostStore.
type Owner struct{ _ byte }

// Host-identity and session-notice plumbing
// (package R12-F chunk C): the App's host lifecycle lives in
// sessionhosts.HostStore, and the job-independent session-notice machinery in
// session.SessionNoticeStore, so registration, teardown and the notice
// surfaces never query the async_jobs table.

// batchHostStores memoizes each App's HostStore. The App struct
// (app.go) does not hold the field, so the store lives in this
// package-level map keyed by the App pointer; the entry is created once at
// the install and lives as long as the App.
var batchHostStores sync.Map // *Owner -> *sessionhosts.HostStore

// batchNoticeStores memoizes each App's SessionNoticeStore.
var batchNoticeStores sync.Map // *Owner -> *session.SessionNoticeStore

// BatchHostStore returns this App's HostStore, building it on first
// use over the writer pool (same pool choice as BatchCLIStore). Nil without
// a database or data dir (the configless-App rule): callers treat nil as
// "no host identity", never panic.
func BatchHostStore(p BatchCLIParams) *sessionhosts.HostStore {
	if p.DataDir == nil || p.DataDir() == "" {
		return nil
	}
	if s, ok := batchHostStores.Load(p.Owner()); ok {
		return s.(*sessionhosts.HostStore)
	}
	writer := CutoverWriter(p)
	if writer == nil {
		return nil
	}
	s := sessionhosts.NewHostStore(writer, p.DataDir(), os.Getpid(), "app")
	actual, loaded := batchHostStores.LoadOrStore(p.Owner(), s)
	if loaded {
		// A concurrent caller won: one store per App.
		return actual.(*sessionhosts.HostStore)
	}
	return s
}

// BatchHostEnsure registers this App's host identity exactly once
// (lazy, memoized inside the HostStore). Fail closed: the caller aborts the
// install on error.
func BatchHostEnsure(p BatchCLIParams, ctx context.Context) (string, error) {
	s := BatchHostStore(p)
	if s == nil {
		return "", errors.New("batch host store unavailable: no db writer or data dir")
	}
	return s.EnsureHost(ctx)
}

// BatchHostRelease is the graceful host release (HostStore.Close:
// OS lock release plus row cleanup via DeleteAsyncHostIfNoNodes).
func BatchHostRelease(p BatchCLIParams, ctx context.Context) error {
	s := BatchHostStore(p)
	if s == nil {
		return nil
	}
	if err := s.Close(ctx); err != nil {
		return err
	}
	// The graceful release is the last legitimate consumer moment: forget the
	// memoized stores so the package maps never retain a closed App (P2-6).
	BatchCutoverStoresForget(p)
	return nil
}

// BatchCutoverStoresForget drops this App's memoized stores from the
// package-level maps (batchHostStores, batchNoticeStores), which are keyed by
// *App with no other deletion: without this, closed Apps and their
// coordinators leak for the life of the process. Nil-safe.
func BatchCutoverStoresForget(p BatchCLIParams) {
	if p.Owner == nil {
		return
	}
	batchHostStores.Delete(p.Owner())
	batchNoticeStores.Delete(p.Owner())
}

// BatchHostReleaseKeepLock is the forced counterpart
// (HostStore.CloseKeepLock: forget the handle, pin the OS lock).
func BatchHostReleaseKeepLock(p BatchCLIParams) {
	if s := BatchHostStore(p); s != nil {
		s.CloseKeepLock()
	}
}

// BatchNoticeStore returns this App's SessionNoticeStore, built
// with the batch implementations and wired to the HostStore-backed
// host identity and liveness. Nil without a database (configless App).
func BatchNoticeStore(p BatchCLIParams) *session.SessionNoticeStore {
	if p.Owner == nil {
		return nil
	}
	if s, ok := batchNoticeStores.Load(p.Owner()); ok {
		return s.(*session.SessionNoticeStore)
	}
	writer := CutoverWriter(p)
	if writer == nil {
		return nil
	}
	s := session.NewSessionNoticeStore(writer)
	s.SetReadConn(db.New(CutoverReader(p, writer)))
	if hs := BatchHostStore(p); hs != nil {
		s.SetHostIDFunc(hs.HostID)
		s.SetHostNotDeadFunc(func(own, host string) bool {
			if host == "" {
				return false
			}
			if host == own {
				return true
			}
			// Fail-open on an unreadable probe (alive-or-unknown keeps the
			// notice), exactly like the legacy store's HostNotDead.
			st, err := sessionhosts.ProbeHostShared(p.DataDir(), host)
			return err != nil || st != session.HostStatusDead
		})
	}
	actual, loaded := batchNoticeStores.LoadOrStore(p.Owner(), s)
	if loaded {
		return actual.(*session.SessionNoticeStore)
	}
	return s
}

// Owner is the identity key of the package-level batch registries
// (batchHostStores, batchNoticeStores, batchMaintenanceLoops). Its pointer
// identity replaces the *App key; it carries no state because the registry
// code reads dataDir and the DB pools from the App at the same lazy moments
// as before. The blank byte is load-bearing: a zero-size struct makes every
// &Owner{} the same address (runtime.zerobase), so all Apps would share
