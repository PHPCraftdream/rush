package modelcontrol

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

type Store interface {
	Read(context.Context, string, string) (sessionbatch.BatchSnapshot, error)
	LiveRootsForHost(context.Context, string) ([]db.BatchNode, error)
	CreateControl(context.Context, string, string, string, string) (db.BatchControlRequest, error)
	AnswerControl(context.Context, db.AnswerBatchControlRequestParams) error
}

type LiveReader interface {
	BatchLevel(context.Context, sessionbatchread.BatchLiveRef, string) sessionbatchread.BatchLevelResult
}

type Sessions interface {
	Get(context.Context, string) (coresession.Session, error)
}

type ReadStopRunner interface {
	Snapshot(context.Context, jobbatch.ID) (jobbatch.BatchView, error)
	Output(context.Context, jobbatch.ID, uint64) (jobbatch.OutputChunk, error)
	Stop(context.Context, jobbatch.ID) error
}

type ReadCachedOutput func(jobbatch.ID, uint64) (jobbatch.OutputChunk, uint64, int, bool, bool)

type Access struct {
	store            Store
	live             LiveReader
	sessions         Sessions
	runner           ReadStopRunner
	readCachedOutput ReadCachedOutput
	host, runnerID   string
}

func NewAccess(store Store, live LiveReader, sessions Sessions, runner ReadStopRunner, readCachedOutput ReadCachedOutput, host, runnerID string) Access {
	return Access{store: store, live: live, sessions: sessions, runner: runner, readCachedOutput: readCachedOutput, host: host, runnerID: runnerID}
}

func TerminalBatchStoreState(s string) bool {
	switch s {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

func (c *Access) ReadAuthorized(ctx context.Context, caller, root string) (sessionbatch.BatchSnapshot, error) {
	if caller == "" || root == "" || c.store == nil || c.sessions == nil {
		return sessionbatch.BatchSnapshot{}, errors.New("batch root access unavailable")
	}
	for current, steps := caller, 0; current != "" && steps < 128; steps++ {
		snap, err := c.store.Read(ctx, current, root)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return sessionbatch.BatchSnapshot{}, err
			}
			sess, getErr := c.sessions.Get(ctx, current)
			if getErr != nil {
				return sessionbatch.BatchSnapshot{}, getErr
			}
			current = sess.ParentSessionID
			continue
		}
		return snap, nil
	}
	return sessionbatch.BatchSnapshot{}, errors.New("batch root not owned by caller ancestry")
}

func FindBatchNode(nodes []db.BatchNode, id string) (db.BatchNode, bool) {
	for _, n := range nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return db.BatchNode{}, false
}

func batchModelNodeInSnapshot(nodes []db.BatchNode, id, kind, root string) bool {
	parents := make(map[string]string, len(nodes))
	kinds := make(map[string]string, len(nodes))
	for _, n := range nodes {
		kinds[n.NodeID] = n.Kind
		if n.ParentID.Valid {
			parents[n.NodeID] = n.ParentID.String
		}
	}
	if kind != "" && kinds[id] != kind {
		return false
	}
	for cur, steps := id, 0; cur != "" && steps <= len(nodes); steps++ {
		if cur == root {
			return true
		}
		cur = parents[cur]
	}
	return false
}

func incompleteBatchStatus(root, node string) map[string]any {
	m := map[string]any{"root_id": root, "state": "unknown", "terminal": false, "complete": false}
	if node != "" {
		m["node_id"] = node
	}
	return m
}

func (c *Access) StatusNode(ctx context.Context, caller, root, node string) (any, error) {
	if node == "" || node == root {
		return c.Status(ctx, caller, root)
	}
	if c.live == nil {
		return incompleteBatchStatus(root, node), nil
	}
	s, err := c.ReadAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, node), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return incompleteBatchStatus(root, node), nil
	}
	fact := c.live.BatchLevel(ctx, sessionbatchread.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, node)
	if !fact.Complete || fact.Disappeared {
		return incompleteBatchStatus(root, node), nil
	}
	return fact, nil
}

func (c *Access) Status(ctx context.Context, caller, root string) (any, error) {
	if caller == "" || root == "" {
		return nil, errors.New("batch_status requires root id")
	}
	if c.live == nil || c.runner == nil {
		return incompleteBatchStatus(root, ""), nil
	}
	s, err := c.ReadAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, ""), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" {
		return incompleteBatchStatus(root, ""), nil
	}
	fact := c.live.BatchLevel(ctx, sessionbatchread.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, root)
	if !fact.Complete || fact.Disappeared {
		return incompleteBatchStatus(root, ""), nil
	}
	if fact.Finished {
		return map[string]any{"root_id": root, "state": fact.Node.State, "terminal": true, "complete": true}, nil
	}
	view, err := c.runner.Snapshot(ctx, jobbatch.ID(root))
	if err != nil {
		return incompleteBatchStatus(root, ""), nil
	}
	return view, nil
}

func (c *Access) Output(ctx context.Context, caller, root, node string, cursor uint64) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	s, err := c.ReadAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.HostID != c.host || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "task", root) {
		return nil, errors.New("batch_output owner/runtime or descendant mismatch")
	}
	chunk, err := c.runner.Output(ctx, jobbatch.ID(node), cursor)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, jobbatch.ErrReleased) {
			return nil, err
		}
		chunk, totalBytes, retainedBytes, truncated, found := c.readCachedOutput(jobbatch.ID(node), cursor)
		if found {
			return cachedBatchOutput(chunk, totalBytes, retainedBytes, truncated), nil
		}
		return expiredBatchOutput(cursor), nil
	}
	return LiveOutput(chunk), nil
}

func LiveOutput(chunk jobbatch.OutputChunk) any {
	// Retained bytes are the live window size, not the end cursor: the
	// difference Next-Oldest, clamped at zero.
	var retained uint64
	if chunk.Next > chunk.Oldest {
		retained = chunk.Next - chunk.Oldest
	}
	return OutputResponse(chunk, chunk.Next, int(retained), chunk.Truncated)
}

func cachedBatchOutput(chunk jobbatch.OutputChunk, totalBytes uint64, retainedBytes int, truncated bool) any {
	return OutputResponse(chunk, totalBytes, retainedBytes, truncated)
}

func OutputResponse(chunk jobbatch.OutputChunk, totalBytes uint64, retainedBytes int, truncated bool) any {
	oldest := chunk.Oldest
	start := chunk.Next - uint64(len(chunk.Data))
	if start < oldest {
		start = oldest
	}
	if start > chunk.Next {
		start = chunk.Next
	}
	data := chunk.Data
	if uint64(len(data)) > OutputLimit {
		data = data[:OutputLimit]
		chunk.Next = start + uint64(len(data))
		chunk.EOF = false
		truncated = true
	}
	if chunk.Oldest > start {
		truncated = true
	}
	status := "available"
	if truncated {
		status = "output_truncated"
	}
	return map[string]any{"output_status": status, "data": string(data), "next_cursor": chunk.Next, "oldest_cursor": chunk.Oldest, "eof": chunk.EOF, "truncated": truncated, "total_bytes": totalBytes, "retained_bytes": retainedBytes}
}

func expiredBatchOutput(cursor uint64) any {
	return map[string]any{"output_status": "output_expired", "reason": "expired_or_evicted", "data": "", "next_cursor": cursor, "oldest_cursor": uint64(0), "eof": true, "truncated": false, "total_bytes": uint64(0), "retained_bytes": 0}
}

type WorkerStopScopeKey struct{}

func WithWorkerStopScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, WorkerStopScopeKey{}, true)
}

func stopBatchWorkerScopeFromContext(ctx context.Context) bool {
	scope, _ := ctx.Value(WorkerStopScopeKey{}).(bool)
	return scope
}

func (c *Access) isSessionDescendant(ctx context.Context, owner, ancestor string) bool {
	if owner == "" || ancestor == "" || c.sessions == nil {
		return false
	}
	ancestorSession, err := c.sessions.Get(ctx, ancestor)
	if err != nil || ancestorSession.ID != ancestor {
		return false
	}
	seen := make(map[string]struct{})
	for current, steps := owner, 0; current != "" && steps < 128; steps++ {
		if current == ancestor {
			return true
		}
		if _, exists := seen[current]; exists {
			return false
		}
		seen[current] = struct{}{}
		sess, err := c.sessions.Get(ctx, current)
		if err != nil || sess.ID != current {
			return false
		}
		current = sess.ParentSessionID
	}
	return false
}

func (c *Access) readStopRoot(ctx context.Context, caller, root string) (sessionbatch.BatchSnapshot, error) {
	if c.store == nil || root == "" || c.host == "" {
		return sessionbatch.BatchSnapshot{}, errors.New("batch_stop root access unavailable")
	}
	roots, err := c.store.LiveRootsForHost(ctx, c.host)
	if err != nil {
		return sessionbatch.BatchSnapshot{}, err
	}
	for _, candidate := range roots {
		if candidate.RootID != root {
			continue
		}
		return c.readStopCandidate(ctx, candidate, root)
	}
	// Terminal roots are excluded by LiveRootsForHost. Retain idempotence
	// for the caller's existing owned-root ancestry without scanning all sessions.
	if snapshot, readErr := c.ReadAuthorized(ctx, caller, root); readErr == nil {
		return c.readStopCandidate(ctx, snapshot.Root, root)
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return sessionbatch.BatchSnapshot{}, readErr
	}
	return sessionbatch.BatchSnapshot{}, errors.New("batch_stop root not found among live local batches")
}

func (c *Access) readStopCandidate(ctx context.Context, candidate db.BatchNode, root string) (sessionbatch.BatchSnapshot, error) {
	if candidate.OwnerSessionID == "" || candidate.HostID != c.host || candidate.RunnerID == "" || candidate.RunnerID != c.runnerID {
		return sessionbatch.BatchSnapshot{}, errors.New("batch_stop root has unknown or stale runner incarnation")
	}
	if c.sessions == nil {
		return sessionbatch.BatchSnapshot{}, errors.New("batch_stop owner lookup unavailable")
	}
	owner, err := c.sessions.Get(ctx, candidate.OwnerSessionID)
	if err != nil || owner.ID != candidate.OwnerSessionID {
		return sessionbatch.BatchSnapshot{}, errors.New("batch_stop root owner is unknown")
	}
	return c.store.Read(ctx, candidate.OwnerSessionID, root)
}

func (c *Access) Stop(ctx context.Context, caller, root, node string) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || root == "" {
		return nil, errors.New("batch_stop requires caller and root id")
	}
	if node == "" {
		node = root
	}
	s, err := c.readStopRoot(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if stopBatchWorkerScopeFromContext(ctx) {
		if !c.isSessionDescendant(ctx, caller, caller) || !c.isSessionDescendant(ctx, s.Root.OwnerSessionID, caller) {
			return nil, errors.New("batch_stop is limited to batches owned by this worker or its descendants")
		}
	} else if !c.isSessionDescendant(ctx, caller, caller) || (!c.isSessionDescendant(ctx, caller, s.Root.OwnerSessionID) && !c.isSessionDescendant(ctx, s.Root.OwnerSessionID, caller)) {
		return nil, errors.New("batch_stop caller must own the root or be related to its owner")
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.OwnerSessionID == "" || s.Root.HostID != c.host || s.Root.RunnerID == "" || s.Root.RunnerID != c.runnerID || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return nil, errors.New("batch_stop requires an owned local root or descendant in the current runner incarnation")
	}
	if c.sessions == nil {
		return nil, errors.New("batch_stop owner lookup unavailable")
	}
	ownerSession, ownerErr := c.sessions.Get(ctx, s.Root.OwnerSessionID)
	if ownerErr != nil || ownerSession.ID != s.Root.OwnerSessionID {
		return nil, errors.New("batch_stop root owner is unknown")
	}
	if TerminalBatchStoreState(s.Root.State) {
		return map[string]any{"accepted": false, "terminal": true, "state": s.Root.State}, nil
	}
	before, found := FindBatchNode(s.Nodes, node)
	if !found || before.HostID != c.host || before.RunnerID == "" || before.RunnerID != c.runnerID {
		return nil, errors.New("batch_stop target has unknown or stale runner incarnation")
	}
	req, err := c.store.CreateControl(ctx, s.Root.OwnerSessionID, c.host, root, node)
	if err != nil {
		return nil, err
	}
	if err := c.runner.Stop(ctx, jobbatch.ID(node)); err != nil {
		return nil, err
	}
	latest, err := c.store.Read(ctx, s.Root.OwnerSessionID, root)
	if err != nil {
		return nil, err
	}
	stopped, ok := FindBatchNode(latest.Nodes, node)
	if !ok || stopped.ProjectionSequence <= before.ProjectionSequence || stopped.State == "pending" || stopped.State == "starting" || stopped.State == "running" {
		return nil, errors.New("batch_stop runner transition was not durably projected")
	}
	if err := c.store.AnswerControl(ctx, db.AnswerBatchControlRequestParams{
		RequestID: req.RequestID, OwnerSessionID: s.Root.OwnerSessionID, HostID: c.host,
		RootID: root, NodeID: node, RunnerID: c.runnerID, AnswerCode: "accepted",
		AnswerSequence: sql.NullInt64{Int64: stopped.ProjectionSequence, Valid: true},
		AnsweredAt:     sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
	}); err != nil {
		return nil, err
	}
	// The answer acknowledges the committed stop transition; settlement is
	// still reported only by the runner's subsequent terminal projection.
	return map[string]any{"request_id": req.RequestID, "accepted": true, "terminal": false, "state": "cancelling"}, nil
}
