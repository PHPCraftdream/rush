package wswork

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wire "github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// Structured batch-node wire (R15a): the web half of the durable batch tree
// (db.batch_nodes) — nodes, counts, delivery state, and structured
// completions. The flat session_live_work projection is also batch-derived
// until H5 retires that wire shape.

// ── Wire (v: 1) ──────────────────────────────────────────────────────────────

// batchCountsWire counts task-kind nodes of one root by state; Active sums
// the nonterminal states. Structural batches never double count (mirrors
// session.BatchLiveCounts).
type batchCountsWire struct {
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

// batchDeliveryWire summarizes the R8 notice-delivery state of one root's
// Counts notice state on every node, including suppressed and delayed selection.
type batchDeliveryWire struct {
	Selected   int `json:"selected"`
	Pending    int `json:"pending"`
	Delivered  int `json:"delivered"`
	Suppressed int `json:"suppressed"`
}

// batchNodeWire is one batch_nodes row of a live root. Host is the
// conservative probe verdict ("alive"|"dead"|"unknown"), never a guess:
// "unknown" keeps the whole snapshot incomplete.
type batchNodeWire struct {
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
	HeldQuestion   string `json:"heldQuestion,omitempty"`
	NoticeSelected bool   `json:"noticeSelected"`
	NoticeState    string `json:"noticeState,omitempty"`
}

// batchRootWire is one live (nonterminal) root of the requested session's
// tree with its full node subtree. Complete is conservative: false whenever
// any node's host could not be proven alive or dead.
type batchRootWire struct {
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
	Counts         batchCountsWire   `json:"counts"`
	Delivery       batchDeliveryWire `json:"delivery"`
	Nodes          []batchNodeWire   `json:"nodes"`
}

// batchCompletionWire is one structured completion: a TERMINAL node inside
// a returned live root, or a TERMINAL ROOT (R12-C: NodeID == RootID; the
// live-node read excludes terminal roots, so each finished root's outcome —
// its state and result-summary columns — is delivered here, read from the
// batch_nodes history projection). No prose is parsed anywhere on this path.
type batchCompletionWire struct {
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

// batchWorkSnapshotWire is both the session_batch_work push and the
// get_session_batch_work reply (V: 1). Complete=false means the read was
// partial (host unknown, chunk failed, depth overflow, root-history read
// failed): clients must MERGE such a snapshot into the previous one, never
// replace. Completions are keyed by root_id (roots) or (root_id, node_id)
// (nodes inside live roots); a terminal root is carried exactly once per
// snapshot, and a reload re-reads it from the structured store — never
// from message prose. Root completions carry the root's toolCallID so the
// web can join without legacy prose metadata.
type batchWorkSnapshotWire struct {
	V           int                   `json:"v"`
	SessionID   string                `json:"sessionID"`
	Complete    bool                  `json:"complete"`
	Roots       []batchRootWire       `json:"roots"`
	Completions []batchCompletionWire `json:"completions"`
}

// ── Injection points (test seams) ───────────────────────────────────────────

// batchNodeSource is the read-only node row boundary — the same sqlc reader
// session.BatchLiveSource uses.
type BatchNodeSource interface {
	BatchLiveNodesForOwners(context.Context, []string) ([]db.BatchNode, error)
	// BatchHistoryRootsForOwner is the R12-C terminal-root read: every root
	// incarnation of one owner, terminal included (batch_history.sql).
	BatchHistoryRootsForOwner(context.Context, string) ([]db.BatchNode, error)
	// BatchLevelNodes reads every node row of one (owner, root_id)
	// incarnation (batch_read.sql) — the history BFS uses it to discover
	// delegation child sessions behind terminal roots.
	BatchLevelNodes(context.Context, db.BatchLevelNodesParams) ([]db.BatchNode, error)
}

func defaultBatchSourceFor(a *appPkg.App) BatchNodeSource {
	if a == nil || a.DB == nil {
		return nil
	}
	return db.New(a.DB())
}

func defaultBatchDataDir(a *appPkg.App) string {
	if a == nil || a.Config() == nil {
		return ""
	}
	return a.Config().Options.DataDirectory
}

func defaultBatchProbeHost(dataDir, host string) session.HostLockStatus {
	if dataDir == "" || host == "" {
		return session.HostStatusUnknown
	}
	st, err := session.ProbeHostShared(dataDir, host)
	if err != nil || (st != session.HostStatusAlive && st != session.HostStatusDead) {
		return session.HostStatusUnknown
	}
	return st
}

type Seams struct {
	SourceFor        func() func(*appPkg.App) BatchNodeSource
	DataDir          func() func(*appPkg.App) string
	ProbeHost        func() func(string, string) session.HostLockStatus
	IsOwnHost        func() func(string) bool
	PendingQuestions func() func(context.Context, *appPkg.App, []string) (map[string]session.ChildQuestion, bool)
	AddForWeb        func() func(context.Context, *appPkg.App, string, string, []jobbatch.Spec) error
}

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
func batchCollectRows(ctx context.Context, src BatchNodeSource, sessionID string) (rowsByOwner map[string][]db.BatchNode, visited []string, complete bool) {
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
func buildBatchWorkSnapshot(ctx context.Context, a *appPkg.App, sessionID string, seams Seams) batchWorkSnapshotWire {
	return buildBatchWorkSnapshotWith(ctx, a, sessionID, seams)
}

func buildBatchWorkSnapshotWith(ctx context.Context, a *appPkg.App, sessionID string, seams Seams) batchWorkSnapshotWire {
	sourceFor := func(a *appPkg.App) BatchNodeSource { return defaultBatchSourceFor(a) }
	if seams.SourceFor != nil {
		if provider := seams.SourceFor(); provider != nil {
			sourceFor = provider
		}
	}
	var src BatchNodeSource
	if value := sourceFor(a); value != nil {
		src = value
	}
	snap := batchWorkSnapshotWire{V: 1, SessionID: sessionID, Complete: false, Roots: []batchRootWire{}, Completions: []batchCompletionWire{}}
	if src == nil || sessionID == "" {
		return snap
	}
	dataDirFor := defaultBatchDataDir
	if seams.DataDir != nil {
		if provider := seams.DataDir(); provider != nil {
			dataDirFor = provider
		}
	}
	dataDir := dataDirFor(a)
	statusOf := func(host string) session.HostLockStatus {
		isOwnHost := session.IsOwnHostID
		if seams.IsOwnHost != nil {
			if provider := seams.IsOwnHost(); provider != nil {
				isOwnHost = provider
			}
		}
		if host != "" && isOwnHost(host) {
			return session.HostStatusAlive
		}
		probeHost := defaultBatchProbeHost
		if seams.ProbeHost != nil {
			if provider := seams.ProbeHost(); provider != nil {
				probeHost = provider
			}
		}
		return probeHost(dataDir, host)
	}
	rowsByOwner, visited, walkComplete := batchCollectRows(ctx, src, sessionID)
	pendingQuestions := batchPendingQuestions
	if seams.PendingQuestions != nil {
		if provider := seams.PendingQuestions(); provider != nil {
			pendingQuestions = provider
		}
	}
	questions, questionsComplete := pendingQuestions(ctx, a, visited)
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
	snap.Complete = walkComplete && !missingRoot && questionsComplete
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
		wire := batchRootWire{
			RootID:         rootRow.RootID,
			OwnerSessionID: rootRow.OwnerSessionID,
			ToolCallID:     rootRow.ToolCallID.String,
			Kind:           rootRow.Kind,
			TaskKind:       rootRow.TaskKind.String,
			State:          rootRow.State,
			Cause:          rootRow.Cause,
			StartedAt:      rootRow.CreatedAt * 1000,
			Host:           batchHostString(statusOf(rootRow.HostID)),
			Nodes:          make([]batchNodeWire, 0, len(nodes)),
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
			case "suppressed":
				wire.Delivery.Suppressed++
			}
			question, held := questions[n.NodeID]
			if !held && n.ToolCallID.Valid {
				question, held = questions[n.ToolCallID.String]
			}
			node := batchNodeWire{
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
				NoticeSelected: n.NoticeSelected > 0,
				NoticeState:    n.NoticeState.String,
				HeldQuestion:   heldQuestion(question, held),
			}
			wire.Nodes = append(wire.Nodes, node)
			if batchTerminal(n.State) {
				snap.Completions = append(snap.Completions, batchCompletionWire{
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
func batchRootCompletions(ctx context.Context, src BatchNodeSource, visited []string) (comps []batchCompletionWire, ok bool) {
	comps = []batchCompletionWire{}
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
				comps = append(comps, batchCompletionWire{
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
func rootCompleteAll(roots []batchRootWire) bool {
	for _, r := range roots {
		if !r.Complete {
			return false
		}
	}
	return true
}

// add tallies one node's task-kind state (structural batches never count).
func (c *batchCountsWire) add(n db.BatchNode) {
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

func heldQuestion(q session.ChildQuestion, held bool) string {
	if held {
		return q.Question
	}
	return ""
}

// handleGetSessionBatchWork replies with one EventSessionBatchWork snapshot
// for the requested session's delegation tree.
func handleGetSessionBatchWork(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	var p wire.GetSessionBatchWorkPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.Reply(msg.ID, wire.EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.Reply(msg.ID, wire.EventError, nil, "session not found")
		return
	}
	snap := buildBatchWorkSnapshotWith(ctx, a, p.SessionID, seams)
	c.Reply(msg.ID, wire.EventSessionBatchWork, snap, "")
}

// batchControlResult is the common response envelope for WebSocket controls.
type batchControlResult struct {
	SessionID string `json:"sessionID"`
	RootID    string `json:"rootID,omitempty"`
	NodeID    string `json:"nodeID,omitempty"`
	Action    string `json:"action"`
	Result    string `json:"result"`
	OK        bool   `json:"ok"`
}

type batchControlPayload struct {
	SessionID string          `json:"sessionID"`
	RootID    string          `json:"rootID"`
	NodeID    string          `json:"nodeID"`
	WaitMS    int             `json:"waitMS,omitempty"`
	Items     []jobbatch.Spec `json:"items,omitempty"`
}

// defaultBatchAddForWeb preserves the public App boundary while allowing focused
// validation/argument tests; production always delegates to App.BatchAdd.
func defaultBatchAddForWeb(ctx context.Context, a *appPkg.App, root, node string, items []jobbatch.Spec) error {
	return a.BatchAdd(ctx, root, node, items)
}

func handleBatchControl(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	var p batchControlPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.Reply(msg.ID, wire.EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.Reply(msg.ID, wire.EventError, nil, "session not found")
		return
	}
	switch msg.Type {
	case wire.CmdBatchStatus:
		c.Reply(msg.ID, wire.EventSessionBatchWork, buildBatchWorkSnapshotWith(ctx, a, p.SessionID, seams), "")
	case wire.CmdBatchOutput:
		batchControlOutput(ctx, a, c, msg, p)
	case wire.CmdBatchStop:
		batchControlStop(ctx, a, c, msg, p)
	case wire.CmdBatchAdd:
		batchControlAdd(ctx, a, c, msg, p, seams.AddForWeb)
	case wire.CmdBatchDrain:
		batchControlReply(c, msg, p, "batch_drain has no public session-scoped automatic reaction drain API; no shutdown drain was invoked", false)
	}
}

func batchControlReply(c *hub.Client, msg wire.WSMessage, p batchControlPayload, result string, ok bool) {
	c.Reply(msg.ID, wire.EventBatchControlResult, batchControlResult{SessionID: p.SessionID, RootID: p.RootID, NodeID: p.NodeID, Action: msg.Type, Result: result, OK: ok}, "")
}

func batchControlOutput(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, p batchControlPayload) {
	if p.NodeID == "" {
		batchControlReply(c, msg, p, "nodeID required", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || identity.Owner != p.SessionID || (p.RootID != "" && identity.Root != p.RootID) {
		batchControlReply(c, msg, p, "batch node not found in requested session/root", false)
		return
	}
	node, err := fac.Store.BatchCLIGetNode(ctx, identity.Owner, identity.Root, nodeID)
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	if node.OutcomeSummary == "" && (!node.ResponseEnvelope.Valid || node.ResponseEnvelope.String == "") {
		batchControlReply(c, msg, p, "no retained outcome is available", false)
		return
	}
	c.Reply(msg.ID, wire.EventBatchControlResult, map[string]any{"sessionID": p.SessionID, "rootID": identity.Root, "nodeID": nodeID, "action": msg.Type, "result": "retained outcome (not a live stream)", "ok": true, "outcomeSummary": node.OutcomeSummary, "outcomeRef": node.ResponseEnvelopeRef.String, "responseEnvelope": node.ResponseEnvelope.String}, "")
}

func batchControlStop(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, p batchControlPayload) {
	if p.NodeID == "" {
		batchControlReply(c, msg, p, "nodeID required", false)
		return
	}
	if p.WaitMS < 0 || p.WaitMS > 30000 {
		batchControlReply(c, msg, p, "waitMS must be between 0 and 30000", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || identity.Owner != p.SessionID || (p.RootID != "" && identity.Root != p.RootID) {
		batchControlReply(c, msg, p, "batch node not found in requested session/root", false)
		return
	}
	target := session.BatchControlTarget{Owner: identity.Owner, Host: identity.Host, Root: identity.Root, Node: nodeID}
	req, err := fac.Store.BatchRequestStop(ctx, target, batchControlHostProbe(fac.Reader.DataDir))
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	state, ackCode := "queued", ""
	if p.WaitMS > 0 {
		wait := time.Duration(p.WaitMS) * time.Millisecond
		pollCtx, cancel := context.WithTimeout(ctx, wait)
		ack, pollErr := fac.Store.BatchCLIPollControl(pollCtx, target, req.RequestID, wait)
		cancel()
		if pollErr == nil && ack.AnswerCode != "" {
			ackCode = ack.AnswerCode
			if ackCode != "accepted" {
				batchControlReply(c, msg, p, "stop request was not accepted: "+ackCode, false)
				return
			}
			state = "applied"
		} else if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) {
			batchControlReply(c, msg, p, pollErr.Error(), false)
			return
		}
	}
	c.Reply(msg.ID, wire.EventBatchControlResult, map[string]any{"sessionID": p.SessionID, "rootID": identity.Root, "nodeID": nodeID, "action": msg.Type, "result": state, "ok": true, "requestID": req.RequestID, "answerCode": ackCode}, "")
}

func batchControlAdd(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, p batchControlPayload, addProvider func() func(context.Context, *appPkg.App, string, string, []jobbatch.Spec) error) {
	if p.RootID == "" || p.NodeID == "" || len(p.Items) == 0 {
		batchControlReply(c, msg, p, "rootID, nodeID and nonempty items required", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, rootNode, err := fac.Store.BatchCLIFind(ctx, p.RootID)
	if err != nil || identity.Owner != p.SessionID || rootNode != identity.Root {
		batchControlReply(c, msg, p, "root not found in requested session", false)
		return
	}
	nodeIdentity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || nodeIdentity.Owner != identity.Owner || nodeIdentity.Root != identity.Root {
		batchControlReply(c, msg, p, "node not found in requested root", false)
		return
	}
	addForWeb := defaultBatchAddForWeb
	if addProvider != nil {
		if provider := addProvider(); provider != nil {
			addForWeb = provider
		}
	}
	if err := addForWeb(ctx, a, identity.Root, nodeID, p.Items); err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	c.Reply(msg.ID, wire.EventBatchControlResult, batchControlResult{SessionID: p.SessionID, RootID: identity.Root, NodeID: nodeID, Action: msg.Type, Result: "added", OK: true}, "")
}

func batchControlHostProbe(dataDir string) session.BatchHostProbe {
	shared := session.BatchSharedHostProbe(dataDir)
	return func(host string) (session.HostLockStatus, error) {
		if session.IsOwnHostID(host) {
			return session.HostStatusAlive, nil
		}
		return shared(host)
	}
}

// The flat Commands/Agents live-work projection remains on its existing wire
// and is now built from batch_nodes by handlers_livework_batch.go. Its
// incomplete-read marker prevents replacing previously shown work with a
// partial snapshot. H5 may retire this compatibility surface.
//
// Structured replacement:
//   - Push "session_batch_work" (EventSessionBatchWork) and request
//     "get_session_batch_work" (CmdGetSessionBatchWork), both carrying
//     batchWorkSnapshotWire (v: 1): batch_nodes trees, counts, delivery state,
//     and structured completions.
//   - The web store merges incomplete snapshots and replaces complete ones.
//
// Left open:
//   - R15b: hierarchy renderer, output/held-input controls on batch nodes.
type batchDoc struct{}
