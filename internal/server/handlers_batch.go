package server

// Structured batch-node wire (R15a): the web half of the durable batch tree
// (db.batch_nodes) — nodes, counts, delivery state, and structured
// completions. The flat session_live_work projection is also batch-derived
// until H5 retires that wire shape.

import (
	"context"
	"encoding/json"
	"sort"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ── Wire (v: 1) ──────────────────────────────────────────────────────────────

// BatchCountsWire counts task-kind nodes of one root by state; Active sums
// the nonterminal states. Structural batches never double count (mirrors
// session.BatchLiveCounts).
type BatchCountsWire struct {
	Pending     int `json:"pending"`
	Starting    int `json:"starting"`
	Running     int `json:"running"`
	Cancelling  int `json:"cancelling"`
	Completed   int `json:"completed"`
	Failed      int `json:"failed"`
	Cancelled   int `json:"cancelled"`
	TimedOut    int `json:"timed_out"`
	Interrupted int `json:"interrupted"`
	Skipped     int `json:"skipped"`
	Active      int `json:"active"`
}

// BatchDeliveryWire summarizes the R8 notice-delivery state of one root's
// nodes: Selected counts nodes whose notice was selected for delivery,
// Pending/Delivered classify the selected ones by notice_state.
type BatchDeliveryWire struct {
	Selected  int `json:"selected"`
	Pending   int `json:"pending"`
	Delivered int `json:"delivered"`
}

// BatchNodeWire is one batch_nodes row of a live root. Host is the
// conservative probe verdict ("alive"|"dead"|"unknown"), never a guess:
// "unknown" keeps the whole snapshot incomplete.
type BatchNodeWire struct {
	NodeID         string `json:"nodeID"`
	ParentID       string `json:"parentID"`
	Kind           string `json:"kind"`
	TaskKind       string `json:"taskKind"`
	ToolCallID     string `json:"toolCallID"`
	State          string `json:"state"`
	Cause          string `json:"cause"`
	Ordinal        int64  `json:"ordinal"`
	Depth          int    `json:"depth"`
	ChildSessionID string `json:"childSessionID"`
	Host           string `json:"host"`
	OutcomeCode    string `json:"outcomeCode"`
	OutcomeSummary string `json:"outcomeSummary"`
	OutcomeRef     string `json:"outcomeRef"`
}

// BatchRootWire is one live (nonterminal) root of the requested session's
// tree with its full node subtree. Complete is conservative: false whenever
// any node's host could not be proven alive or dead.
type BatchRootWire struct {
	RootID         string            `json:"rootID"`
	OwnerSessionID string            `json:"ownerSessionID"`
	ToolCallID     string            `json:"toolCallID"`
	Kind           string            `json:"kind"`
	TaskKind       string            `json:"taskKind"`
	State          string            `json:"state"`
	Cause          string            `json:"cause"`
	StartedAt      int64             `json:"startedAt"`
	Host           string            `json:"host"`
	Complete       bool              `json:"complete"`
	Counts         BatchCountsWire   `json:"counts"`
	Delivery       BatchDeliveryWire `json:"delivery"`
	Nodes          []BatchNodeWire   `json:"nodes"`
}

// BatchCompletionWire is one structured completion: a TERMINAL node inside
// a returned live root, or a TERMINAL ROOT (R12-C: NodeID == RootID; the
// live-node read excludes terminal roots, so each finished root's outcome —
// its state and result-summary columns — is delivered here, read from the
// batch_nodes history projection). No prose is parsed anywhere on this path.
type BatchCompletionWire struct {
	RootID      string `json:"rootID"`
	NodeID      string `json:"nodeID"`
	OutcomeCode string `json:"outcomeCode"`
	OutcomeRef  string `json:"outcomeRef"`
	// Root completions only; node completions leave all three zero (shape
	// pinned by TestBatchWorkSnapshot_StructuredCompletions).
	State          string `json:"state,omitempty"`
	OutcomeSummary string `json:"outcomeSummary,omitempty"`
	// The terminal root's own tool call identity — the join key the web
	// uses when no legacy prose metadata exists for the root.
	ToolCallID string `json:"toolCallID,omitempty"`
}

// BatchWorkSnapshotWire is both the session_batch_work push and the
// get_session_batch_work reply (V: 1). Complete=false means the read was
// partial (host unknown, chunk failed, depth overflow, root-history read
// failed): clients must MERGE such a snapshot into the previous one, never
// replace. Completions are keyed by root_id (roots) or (root_id, node_id)
// (nodes inside live roots); a terminal root is carried exactly once per
// snapshot, and a reload re-reads it from the structured store — never
// from message prose. Root completions carry the root's toolCallID so the
// web can join without legacy prose metadata.
type BatchWorkSnapshotWire struct {
	V           int                   `json:"v"`
	SessionID   string                `json:"sessionID"`
	Complete    bool                  `json:"complete"`
	Roots       []BatchRootWire       `json:"roots"`
	Completions []BatchCompletionWire `json:"completions"`
}

// ── Injection points (test seams) ───────────────────────────────────────────

// batchNodeSource is the read-only node row boundary — the same sqlc reader
// session.BatchLiveSource uses.
type batchNodeSource interface {
	BatchLiveNodesForOwners(context.Context, []string) ([]db.BatchNode, error)
	// BatchHistoryRootsForOwner is the R12-C terminal-root read: every root
	// incarnation of one owner, terminal included (batch_history.sql).
	BatchHistoryRootsForOwner(context.Context, string) ([]db.BatchNode, error)
	// BatchLevelNodes reads every node row of one (owner, root_id)
	// incarnation (batch_read.sql) — the history BFS uses it to discover
	// delegation child sessions behind terminal roots.
	BatchLevelNodes(context.Context, db.BatchLevelNodesParams) ([]db.BatchNode, error)
}

// Production wiring. The source reads through the app's DB handle; the
// probe reuses session.ProbeHostShared (the shared, non-acquiring host
// probe) against the app data dir; own-host rows are alive by definition.
// Package vars only so tests can fake them; production never reassigns.
var (
	batchSourceFor = func(a *appPkg.App) batchNodeSource {
		if a == nil || a.DB == nil {
			return nil
		}
		return db.New(a.DB())
	}
	batchDataDir = func(a *appPkg.App) string {
		if a == nil || a.Config() == nil {
			return ""
		}
		return a.Config().Options.DataDirectory
	}
	batchProbeHost = func(dataDir, host string) session.HostLockStatus {
		if dataDir == "" || host == "" {
			return session.HostStatusUnknown
		}
		st, err := session.ProbeHostShared(dataDir, host)
		if err != nil || (st != session.HostStatusAlive && st != session.HostStatusDead) {
			return session.HostStatusUnknown
		}
		return st
	}
	batchIsOwnHost = session.IsOwnHostID
)

// batchHostString maps a probe verdict to the wire's host vocabulary.
func batchHostString(st session.HostLockStatus) string {
	switch st {
	case session.HostStatusAlive:
		return "alive"
	case session.HostStatusDead:
		return "dead"
	default:
		return "unknown"
	}
}

// batchTerminal mirrors session batchReadTerminal: the session-side terminal
// states eligible for a structured completion.
func batchTerminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// batchCollectRows walks the delegation forest exactly like
// session.BatchLiveForSessions: links come only from nodes' child_session_id,
// fetches chunk, and any failure or unfetched frontier session makes the
// read incomplete. visited returns the sessions reached, in first-seen
// order (the R12-C terminal-root completion read uses the same scope).
func batchCollectRows(ctx context.Context, src batchNodeSource, sessionID string) (rowsByOwner map[string][]db.BatchNode, visited []string, complete bool) {
	rowsByOwner = make(map[string][]db.BatchNode)
	fetched := map[string]bool{}
	failures := map[string]bool{}
	seen := map[string]bool{sessionID: true}
	visited = []string{sessionID}
	frontier := []string{sessionID}
	for depth := 0; depth <= 16 && len(frontier) > 0; depth++ {
		var todo, next []string
		for _, owner := range frontier {
			if !fetched[owner] {
				fetched[owner] = true
				todo = append(todo, owner)
			}
		}
		for start := 0; start < len(todo); start += 500 {
			chunk := todo[start:min(start+500, len(todo))]
			rows, err := src.BatchLiveNodesForOwners(ctx, chunk)
			if err != nil {
				for _, owner := range chunk {
					failures[owner] = true
				}
				continue
			}
			for _, n := range rows {
				rowsByOwner[n.OwnerSessionID] = append(rowsByOwner[n.OwnerSessionID], n)
				if n.ChildSessionID.Valid && n.ChildSessionID.String != "" && !seen[n.ChildSessionID.String] {
					seen[n.ChildSessionID.String] = true
					visited = append(visited, n.ChildSessionID.String)
					next = append(next, n.ChildSessionID.String)
				}
			}
		}
		frontier = next
	}
	complete = len(failures) == 0 && len(frontier) == 0
	return rowsByOwner, visited, complete
}

// buildBatchWorkSnapshot assembles the v:1 snapshot for one session's
// delegation tree from the live-node read plus conservative host probes.
// Conservative rules mirror the session reader: an unknown host, a failed
// chunk, a missing root row, or a depth overflow all force Complete=false
// while still carrying whatever roots WERE read — an incomplete snapshot
// never drops live work, it only downgrades certainty.
func buildBatchWorkSnapshot(ctx context.Context, a *appPkg.App, sessionID string) BatchWorkSnapshotWire {
	snap := BatchWorkSnapshotWire{V: 1, SessionID: sessionID, Complete: false, Roots: []BatchRootWire{}, Completions: []BatchCompletionWire{}}
	src := batchSourceFor(a)
	if src == nil || sessionID == "" {
		return snap
	}
	dataDir := batchDataDir(a)
	statusOf := func(host string) session.HostLockStatus {
		if host != "" && batchIsOwnHost(host) {
			return session.HostStatusAlive
		}
		return batchProbeHost(dataDir, host)
	}

	rowsByOwner, visited, walkComplete := batchCollectRows(ctx, src, sessionID)
	var all []db.BatchNode
	for _, rows := range rowsByOwner {
		all = append(all, rows...)
	}
	// SQL returns rows ordered by root creation then ordinal; group them
	// preserving first-seen root order.
	roots := make([]db.BatchNode, 0, 4)
	rootByID := map[string]db.BatchNode{}
	nodesByRoot := map[string][]db.BatchNode{}
	missingRoot := false
	for _, n := range all {
		if n.NodeID == n.RootID {
			if _, dup := rootByID[n.RootID]; !dup {
				roots = append(roots, n)
				rootByID[n.RootID] = n
			}
			nodesByRoot[n.RootID] = append(nodesByRoot[n.RootID], n)
			continue
		}
		if _, ok := rootByID[n.RootID]; !ok {
			missingRoot = true
			continue
		}
		nodesByRoot[n.RootID] = append(nodesByRoot[n.RootID], n)
	}
	snap.Complete = walkComplete && !missingRoot
	for _, rootRow := range roots {
		nodes := nodesByRoot[rootRow.RootID]
		// Deterministic order within a root: ordinal first, node id
		// tie-break (the SQL order already implies this; re-sorted here so
		// the guarantee does not depend on the query text).
		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].Ordinal != nodes[j].Ordinal {
				return nodes[i].Ordinal < nodes[j].Ordinal
			}
			return nodes[i].NodeID < nodes[j].NodeID
		})

		depth := make(map[string]int, len(nodes))
		byID := make(map[string]db.BatchNode, len(nodes))
		for _, n := range nodes {
			byID[n.NodeID] = n
		}
		depthOf := func(n db.BatchNode) int {
			if d, ok := depth[n.NodeID]; ok {
				return d
			}
			chain := 0
			cur := n
			for cur.NodeID != cur.RootID && chain < 64 {
				parent, ok := byID[cur.ParentID.String]
				if !ok {
					break
				}
				cur = parent
				chain++
			}
			depth[n.NodeID] = chain
			return chain
		}

		rootComplete := true
		wire := BatchRootWire{
			RootID:         rootRow.RootID,
			OwnerSessionID: rootRow.OwnerSessionID,
			ToolCallID:     rootRow.ToolCallID.String,
			Kind:           rootRow.Kind,
			TaskKind:       rootRow.TaskKind.String,
			State:          rootRow.State,
			Cause:          rootRow.Cause,
			StartedAt:      rootRow.CreatedAt * 1000,
			Host:           batchHostString(statusOf(rootRow.HostID)),
			Nodes:          make([]BatchNodeWire, 0, len(nodes)),
		}
		if wire.Host == "unknown" {
			rootComplete = false
		}
		for _, n := range nodes {
			st := statusOf(n.HostID)
			if st == session.HostStatusUnknown {
				rootComplete = false
			}
			wire.Counts.add(n)
			if n.NoticeSelected > 0 {
				wire.Delivery.Selected++
			}
			switch n.NoticeState.String {
			case "pending":
				wire.Delivery.Pending++
			case "delivered":
				wire.Delivery.Delivered++
			}
			node := BatchNodeWire{
				NodeID:         n.NodeID,
				ParentID:       n.ParentID.String,
				Kind:           n.Kind,
				TaskKind:       n.TaskKind.String,
				ToolCallID:     n.ToolCallID.String,
				State:          n.State,
				Cause:          n.Cause,
				Ordinal:        n.Ordinal,
				Depth:          depthOf(n),
				ChildSessionID: n.ChildSessionID.String,
				Host:           batchHostString(st),
				OutcomeCode:    n.OutcomeCode,
				OutcomeSummary: n.OutcomeSummary,
				OutcomeRef:     n.ResponseEnvelopeRef.String,
			}
			wire.Nodes = append(wire.Nodes, node)
			if batchTerminal(n.State) {
				snap.Completions = append(snap.Completions, BatchCompletionWire{
					RootID:      n.RootID,
					NodeID:      n.NodeID,
					OutcomeCode: n.OutcomeCode,
					OutcomeRef:  n.ResponseEnvelopeRef.String,
				})
			}
		}
		wire.Complete = rootComplete
		snap.Roots = append(snap.Roots, wire)
	}
	if !rootCompleteAll(snap.Roots) {
		snap.Complete = false
	}
	// R12-C terminal-root completions: every terminal root incarnation of
	// the visited sessions, keyed by root_id, read from the same structured
	// store a reload re-reads. A failed history chunk only downgrades
	// certainty — it never drops the completions that WERE read.
	rootComps, historyComplete := batchRootCompletions(ctx, src, visited)
	if !historyComplete {
		snap.Complete = false
	}
	snap.Completions = append(snap.Completions, rootComps...)
	return snap
}

// batchHistoryDepth bounds the completion-history BFS over terminal-root
// child sessions (matches batchCollectRows's delegation depth).
const batchHistoryDepth = 16

// batchRootCompletions reads the TERMINAL root rows reachable from the
// live walk's visited sessions and maps each to one completion keyed by
// root_id (a reused tool_call_id is a DIFFERENT incarnation with its own
// root_id, so two reused ids stay distinct). The live walk's SQL excludes
// terminal roots, so a completed delegation child would never be visited —
// the read is a bounded BFS instead: each TERMINAL root's completion is
// added, then its node rows are read (BatchLevelNodes) and the child
// sessions they name form the next owner frontier. Depth is capped at
// batchHistoryDepth; owner and root_id cycle guards stop re-reads.
// Duplicates — the same root read twice — collapse: exactly one completion
// per root_id. ok=false when any read failed; the completions that WERE
// read are still returned (a failure downgrades, never drops).
func batchRootCompletions(ctx context.Context, src batchNodeSource, visited []string) (comps []BatchCompletionWire, ok bool) {
	comps = []BatchCompletionWire{}
	ok = true
	seenRoot := map[string]bool{}
	seenOwner := map[string]bool{}
	frontier := make([]string, 0, len(visited))
	for _, owner := range visited {
		if !seenOwner[owner] {
			seenOwner[owner] = true
			frontier = append(frontier, owner)
		}
	}
	for depth := 0; depth <= batchHistoryDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, owner := range frontier {
			roots, err := src.BatchHistoryRootsForOwner(ctx, owner)
			if err != nil {
				ok = false
				continue
			}
			for _, r := range roots {
				if r.NodeID != r.RootID || !batchTerminal(r.State) || seenRoot[r.RootID] {
					continue
				}
				seenRoot[r.RootID] = true
				comps = append(comps, BatchCompletionWire{
					RootID:         r.RootID,
					NodeID:         r.RootID,
					OutcomeCode:    r.OutcomeCode,
					OutcomeRef:     r.ResponseEnvelopeRef.String,
					State:          r.State,
					OutcomeSummary: r.OutcomeSummary,
					ToolCallID:     r.ToolCallID.String,
				})
				// The terminal root's own node rows name its delegation
				// children; their owners enter the next frontier.
				nodes, err := src.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: owner, RootID: r.RootID})
				if err != nil {
					ok = false
					continue
				}
				for _, n := range nodes {
					child := n.ChildSessionID.String
					if n.ChildSessionID.Valid && child != "" && !seenOwner[child] {
						seenOwner[child] = true
						next = append(next, child)
					}
				}
			}
		}
		frontier = next
	}
	return comps, ok
}

// rootCompleteAll is false when any carried root is incomplete — an
// uncertain root makes the whole scope uncertain.
func rootCompleteAll(roots []BatchRootWire) bool {
	for _, r := range roots {
		if !r.Complete {
			return false
		}
	}
	return true
}

// add tallies one node's task-kind state (structural batches never count).
func (c *BatchCountsWire) add(n db.BatchNode) {
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

// handleGetSessionBatchWork replies with one EventSessionBatchWork snapshot
// for the requested session's delegation tree.
func handleGetSessionBatchWork(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p GetSessionBatchWorkPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	snap := buildBatchWorkSnapshot(ctx, a, p.SessionID)
	c.reply(msg.ID, EventSessionBatchWork, snap, "")
}
