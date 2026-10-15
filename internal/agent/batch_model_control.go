package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

type batchModelDelivery interface {
	DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse, error)
}

type installedBatchModelControl struct {
	runner             batchModelRunner
	delivery           batchModelDelivery
	store              *session.BatchStore
	live               *session.BatchLiveReader
	sessions           session.Service
	host, runnerID     string
	profile            *BatchProfile
	resolver           batchMCPCapabilityResolver
	rootLeases         *batchRootLeaseStore
	leafRegistryRefs   *batchLeafRegistryLeaseStore
	maxDepth, maxNodes int
}

var _ BatchModelControl = (*installedBatchModelControl)(nil)

func NewInstalledBatchModelControl(runner batchModelRunner, delivery batchModelDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions session.Service, host, runnerID string, profile *BatchProfile, resolver batchMCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	control := &installedBatchModelControl{runner: runner, delivery: delivery, store: store, live: live, sessions: sessions, host: host, runnerID: runnerID, profile: profile, resolver: resolver, maxDepth: batchDefaultMaxDepth, maxNodes: batchDefaultMaxNodes}
	if len(leases) > 0 {
		control.rootLeases = leases[0]
		control.leafRegistryRefs = leases[0].registries
	}
	return control
}

func (c *installedBatchModelControl) InstallBatchModelControls(reg *BatchLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	if reg == nil {
		return nil, errors.New("batch controls require calling pinned registry")
	}
	return newBatchModelTools(c, inv, reg), nil
}

func (c *installedBatchModelControl) Run(ctx context.Context, inv BatchInvocation, reg *policyLeafRegistry, raw batchModelSpec) (jobbatch.ID, error) {
	if c.runner == nil || c.store == nil || c.delivery == nil {
		return "", errors.New("batch runtime unavailable")
	}
	if err := inv.Validate(); err != nil {
		return "", err
	}
	if inv.CallID == "" || reg == nil || inv.RegistryID != inv.Policy.RegistryID || !reg.hasToolset(inv.Policy.RestrictedToolset) {
		return "", errBatchMissingCallID
	}
	spec, err := modelBatchSpec(raw, inv, reg)
	if err != nil {
		return "", err
	}
	if err := assignModelBatchCallIDs(&spec, inv.CallID, "run"); err != nil {
		return "", err
	}
	root := batchRootID()
	if err = ValidateSubmit(c.profile, batchRankContext{Rank: inv.WaiterRank, Root: root}, nil, spec, c.maxDepth, c.maxNodes, c.resolver); err != nil {
		return "", err
	}
	var pending *batchRootLeasePending
	if inv.Policy.DiskBindingID != "" {
		if c.rootLeases == nil {
			return "", ErrDiskProviderUnavailable
		}
		pending, err = c.rootLeases.acquire(inv.Policy.DiskBindingID, inv.RegistryID)
		if err != nil {
			return "", err
		}
	}
	request := jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: inv.CallID}
	var id jobbatch.ID
	var batch *jobbatch.Batch
	if pending != nil {
		bound, ok := c.runner.(batchBoundRunnerHandle)
		if !ok {
			pending.release()
			return "", ErrDiskProviderUnavailable
		}
		id, batch, err = bound.SubmitBound(ctx, request)
	} else {
		id, err = c.runner.Submit(ctx, request)
	}
	if err != nil {
		if pending != nil {
			pending.release()
		}
		return "", err
	}
	if id != root {
		if pending != nil {
			pending.release()
		}
		return "", fmt.Errorf("runner root %q differs from requested %q", id, root)
	}
	if pending != nil {
		if batch == nil {
			pending.release()
			return "", ErrDiskProviderUnavailable
		}
		if batch.ID() != root {
			pending.release()
			return "", fmt.Errorf("runner root %q differs from requested %q", batch.ID(), root)
		}
		if err := c.rootLeases.adopt(root, batch.Done(), pending); err != nil {
			return "", err
		}
	}
	resp, err := c.delivery.DeliverBatch(ctx, root, inv)
	if err != nil {
		return "", err
	}
	if resp.IsError {
		return "", fmt.Errorf("batch delivery failed: %s", resp.Content)
	}
	return root, nil
}

func assignModelBatchCallIDs(spec *jobbatch.Spec, base, path string) error {
	if spec == nil {
		return errors.New("nil batch spec")
	}
	if spec.Task != nil {
		var payload batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &payload); err != nil {
			return err
		}
		payload.ToolCallID = fmt.Sprintf("%s-%s", base, path)
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		spec.Task.Payload = raw
		return nil
	}
	if spec.Batch != nil {
		for i := range spec.Batch.Items {
			if err := assignModelBatchCallIDs(&spec.Batch.Items[i], base, fmt.Sprintf("%s-%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("invalid empty batch spec")
}

func terminalBatchStoreState(s string) bool {
	switch s {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

func (c *installedBatchModelControl) readAuthorized(ctx context.Context, caller, root string) (session.BatchSnapshot, error) {
	if caller == "" || root == "" || c.store == nil || c.sessions == nil {
		return session.BatchSnapshot{}, errors.New("batch root access unavailable")
	}
	for current, steps := caller, 0; current != "" && steps < 128; steps++ {
		snap, err := c.store.Read(ctx, current, root)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return session.BatchSnapshot{}, err
			}
			sess, getErr := c.sessions.Get(ctx, current)
			if getErr != nil {
				return session.BatchSnapshot{}, getErr
			}
			current = sess.ParentSessionID
			continue
		}
		return snap, nil
	}
	return session.BatchSnapshot{}, errors.New("batch root not owned by caller ancestry")
}

func findBatchNode(nodes []db.BatchNode, id string) (db.BatchNode, bool) {
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

func batchModelSpecNodes(items []batchModelItem, inv BatchInvocation, reg *policyLeafRegistry) ([]jobbatch.Spec, error) {
	out := make([]jobbatch.Spec, 0, len(items))
	for i, item := range items {
		var spec jobbatch.Spec
		var err error
		if item.Batch != nil {
			spec, err = modelBatchSpec(*item.Batch, inv, reg)
		} else {
			var wrapper jobbatch.Spec
			wrapper, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Notify: "each", Items: []batchModelItem{item}}, inv, reg)
			if err == nil {
				spec = wrapper.Batch.Items[0]
			}
		}
		if err != nil {
			return nil, err
		}
		if spec.Task != nil {
			var payload batchLeafPayload
			if err = json.Unmarshal(spec.Task.Payload, &payload); err != nil {
				return nil, err
			}
			payload.ToolCallID = fmt.Sprintf("%s-add-%d", inv.CallID, i)
			spec.Task.Payload, err = json.Marshal(payload)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

func (c *installedBatchModelControl) Add(ctx context.Context, caller, root, parent string, inv BatchInvocation, reg *policyLeafRegistry, items []batchModelItem) ([]jobbatch.ID, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || caller != inv.SessionID || root == "" || len(items) == 0 {
		return nil, errors.New("batch_add requires caller, root and items")
	}
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	if inv.CallID == "" || reg == nil || inv.RegistryID != inv.Policy.RegistryID || !reg.hasToolset(inv.Policy.RestrictedToolset) {
		return nil, errBatchMissingCallID
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.HostID != c.host || terminalBatchStoreState(s.Root.State) {
		return nil, errors.New("batch_add requires live local owned root")
	}
	if c.rootLeases == nil || !c.rootLeases.matches(jobbatch.ID(root), inv.Policy.DiskBindingID, inv.RegistryID) {
		return nil, ErrDiskProviderUnavailable
	}
	if parent == "" {
		parent = root
	}
	depths := map[string]int{root: 1}
	for progress := true; progress; {
		progress = false
		for _, n := range s.Nodes {
			if depths[n.NodeID] == 0 && n.ParentID.Valid {
				if d := depths[n.ParentID.String]; d > 0 {
					depths[n.NodeID] = d + 1
					progress = true
				}
			}
		}
	}
	depth := depths[parent]
	if depth == 0 {
		return nil, errors.New("batch_add parent not in root")
	}
	pn, ok := findBatchNode(s.Nodes, parent)
	if !ok || pn.Kind != "batch" {
		return nil, errors.New("batch_add parent must be batch")
	}
	specs, err := batchModelSpecNodes(items, inv, reg)
	if err != nil {
		return nil, err
	}
	used := len(s.Nodes)
	for i := range specs {
		spec := specs[i]
		if err := assignModelBatchCallIDs(&spec, inv.CallID, fmt.Sprintf("add-%d", i)); err != nil {
			return nil, err
		}
		if err := ValidateAdd(c.profile, batchRankContext{Rank: inv.WaiterRank, Root: jobbatch.ID(root)}, nil, spec, depth, used, c.maxDepth, c.maxNodes, c.resolver); err != nil {
			return nil, err
		}
		_, added, _, err := batchSpecNodes([]jobbatch.Spec{spec})
		if err != nil {
			return nil, err
		}
		used += added
		specs[i] = spec
	}
	return c.runner.Add(ctx, jobbatch.AddRequest{Parent: jobbatch.ID(parent), Items: specs, Ref: inv.CallID})
}

func incompleteBatchStatus(root, node string) map[string]any {
	m := map[string]any{"root_id": root, "state": "unknown", "terminal": false, "complete": false}
	if node != "" {
		m["node_id"] = node
	}
	return m
}

func (c *installedBatchModelControl) StatusNode(ctx context.Context, caller, root, node string) (any, error) {
	if node == "" || node == root {
		return c.Status(ctx, caller, root)
	}
	if c.live == nil {
		return incompleteBatchStatus(root, node), nil
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, node), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return incompleteBatchStatus(root, node), nil
	}
	fact := c.live.BatchLevel(ctx, session.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, node)
	if !fact.Complete || fact.Disappeared {
		return incompleteBatchStatus(root, node), nil
	}
	return fact, nil
}

func (c *installedBatchModelControl) Status(ctx context.Context, caller, root string) (any, error) {
	if caller == "" || root == "" {
		return nil, errors.New("batch_status requires root id")
	}
	if c.live == nil || c.runner == nil {
		return incompleteBatchStatus(root, ""), nil
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, ""), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" {
		return incompleteBatchStatus(root, ""), nil
	}
	fact := c.live.BatchLevel(ctx, session.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, root)
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

func (c *installedBatchModelControl) Output(ctx context.Context, caller, root, node string, cursor uint64) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.HostID != c.host || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "task", root) {
		return nil, errors.New("batch_output owner/runtime or descendant mismatch")
	}
	chunk, err := c.runner.Output(ctx, jobbatch.ID(node), cursor)
	if err != nil {
		return nil, err
	}
	start := cursor
	if chunk.Oldest > start {
		start = chunk.Oldest
		chunk.Truncated = true
	}
	if uint64(len(chunk.Data)) > batchModelOutputLimit {
		chunk.Data = chunk.Data[:batchModelOutputLimit]
		chunk.Next = start + uint64(len(chunk.Data))
		chunk.Truncated = true
		chunk.EOF = false
	}
	return chunk, nil
}

func (c *installedBatchModelControl) Stop(ctx context.Context, caller, root, node string) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || root == "" {
		return nil, errors.New("batch_stop requires root id")
	}
	if node == "" {
		node = root
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.HostID != c.host || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return nil, errors.New("batch_stop requires owned local root or descendant")
	}
	if terminalBatchStoreState(s.Root.State) {
		return map[string]any{"accepted": false, "terminal": true, "state": s.Root.State}, nil
	}
	before, found := findBatchNode(s.Nodes, node)
	if !found {
		return nil, errors.New("batch_stop target disappeared before runner request")
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
	stopped, ok := findBatchNode(latest.Nodes, node)
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
