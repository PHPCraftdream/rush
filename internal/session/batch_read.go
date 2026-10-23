package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// BatchLiveSource is the read-only sqlc boundary. Each call reads one consistent
// statement; no writer, host registration, recovery or outbox mutation is needed.
type BatchLiveSource interface {
	BatchLiveNodesForOwners(context.Context, []string) ([]db.BatchNode, error)
	BatchLevelNodes(context.Context, db.BatchLevelNodesParams) ([]db.BatchNode, error)
}

// BatchLiveReader is independent of BatchStore's writer. Construct it with
// db.New(readPool), or an instance-local source for fault/counting tests.
type BatchLiveReader struct {
	Source BatchLiveSource
	// TxStarter, when non-nil, makes EVERY live read run inside ONE
	// read-only transaction on it: the served two-step read then comes from
	// a single snapshot (index-served, one BeginTx per call), instead of the
	// single-statement join fallback — consistent by construction at the
	// same scan-class cost. Nil falls back to the single-statement join.
	TxStarter BatchLiveTxStarter
	DataDir   string
	IsOwnHost func(string) bool
	ProbeHost func(string, string) (HostLockStatus, error)
}

// NewBatchLiveReader binds the shared, non-acquiring host probe.
func NewBatchLiveReader(source BatchLiveSource, dataDir string) *BatchLiveReader {
	return &BatchLiveReader{Source: source, DataDir: dataDir, IsOwnHost: IsOwnHostID, ProbeHost: ProbeHostShared}
}

// BatchLiveRef identifies one immutable execution incarnation, not a tool call.
type BatchLiveRef struct{ OwnerSessionID, RootID string }

// BatchLiveCounts counts task nodes only; structural batches never double count.
type BatchLiveCounts struct {
	Pending, Starting, Running, Cancelling, Active               int
	Completed, Failed, Cancelled, TimedOut, Interrupted, Skipped int
}

func (c *BatchLiveCounts) add(n db.BatchNode) {
	if n.Kind != "task" {
		return
	}
	switch n.State {
	case "pending":
		c.Pending++
	case "starting":
		c.Starting++
		c.Active++
	case "running":
		c.Running++
		c.Active++
	case "cancelling":
		c.Cancelling++
		c.Active++
	case "completed":
		c.Completed++
	case "failed":
		c.Failed++
	case "cancelled":
		c.Cancelled++
	case "timed_out":
		c.TimedOut++
	case "interrupted":
		c.Interrupted++
	case "skipped":
		c.Skipped++
	default:
		c.Active++
	}
}

// BatchLiveNode is an observed root/node fact. Host status never settles it.
type BatchLiveNode struct {
	Ref                                                                     BatchLiveRef
	NodeID, ParentID, SessionID, ToolCallID, Kind, TaskKind, ChildSessionID string
	Depth                                                                   int
	Ordinal                                                                 int64
	State, Cause                                                            string
	Notify                                                                  sql.NullString
	HostID                                                                  string
	HostStatus                                                              HostLockStatus
	StartedAt                                                               time.Time
	Counts                                                                  BatchLiveCounts
}

func batchLiveFact(n db.BatchNode, call string, depth int, status HostLockStatus) BatchLiveNode {
	return BatchLiveNode{Ref: BatchLiveRef{n.OwnerSessionID, n.RootID}, NodeID: n.NodeID, ParentID: n.ParentID.String, SessionID: n.OwnerSessionID, ToolCallID: call, Kind: n.Kind, TaskKind: n.TaskKind.String, ChildSessionID: n.ChildSessionID.String, Depth: depth, Ordinal: n.Ordinal, State: n.State, Cause: n.Cause, Notify: n.Notify, HostID: n.HostID, HostStatus: status, StartedAt: time.Unix(n.CreatedAt, 0)}
}

func batchReadTerminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

func (r *BatchLiveReader) hostStatus(host string) HostLockStatus {
	if host == "" || r == nil {
		return HostStatusUnknown
	}
	if r.IsOwnHost != nil && r.IsOwnHost(host) {
		return HostStatusAlive
	}
	if r.ProbeHost == nil || r.DataDir == "" {
		return HostStatusUnknown
	}
	st, err := r.ProbeHost(r.DataDir, host)
	if err != nil {
		return HostStatusUnknown
	}
	if st != HostStatusAlive && st != HostStatusDead {
		return HostStatusUnknown
	}
	return st
}

var errBatchReadUnavailable = errors.New("batch reader unavailable or invalid identity")

// BatchLiveResult keeps own roots, delegation-node evidence, and the complete
// session tree distinct. A zero value cannot prove absence of live work.
type BatchLiveResult struct {
	Own, Descendants, Tree []BatchLiveNode
	Counts                 BatchLiveCounts
	OwnComplete, Complete  bool
	Err                    error
}

func (v BatchLiveResult) HasLive() bool            { return !v.Complete || len(v.Tree) > 0 }
func (v BatchLiveResult) HasLiveOwn() bool         { return !v.OwnComplete || len(v.Own) > 0 }
func (v BatchLiveResult) HasLiveDescendants() bool { return !v.Complete || len(v.Descendants) > 0 }

// BatchLiveForOwner returns every nonterminal owner root, delegations included.
func (r *BatchLiveReader) BatchLiveForOwner(ctx context.Context, owner string) BatchLiveResult {
	return r.BatchLiveForSessions(ctx, []string{owner})[owner]
}
