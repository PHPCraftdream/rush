package server

// R12-C: the batch-runtime read path for the live-work panel — the ONLY
// path. Active work comes from batch_nodes (live roots, delegation tree
// walked exactly like the structured snapshot); TERMINAL roots additionally
// come from the bounded BatchHistoryRootsForOwner history read. NO
// async_jobs SQL exists anywhere on this path.
//
// Incomplete-read rule: a partial or failed read carries complete=false
// (and the pusher skips broadcasting it), so the client KEEPS the work it
// already shows — an incomplete read never silently drops active work.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchAgentTaskKinds maps a batch task_kind to the flat panel's Agents list
// (everything else — run, fetch-style shells — stays under Commands, like
// the old JobKindCommand/JobKindAgent split).
var batchAgentTaskKinds = map[string]bool{
	"agent":         true,
	"agentic_fetch": true,
}

// batchRootMeta is one live root's flat-panel facts: the root row plus the
// task kind, delegation child session and executing task node id read from
// its task child (a root row carries task_kind NULL by schema CHECK).
type batchRootMeta struct {
	root      db.BatchNode
	taskKind  string
	child     string
	askNodeID string
	// taskChildren counts the task nodes under the root and taskPayload is
	// the first one's task_payload: a direct tool call stores its own input
	// there, which names the title without any history lookup.
	taskChildren int
	taskPayload  string
}

// payloadTitle is the title of a root started by ONE direct tool call, read
// from the input the call stored on its task node ("" when the root has not
// exactly one task node, no payload, or nothing readable in the input). It
// identifies the incarnation exactly, where the history lookup can only match
// a reused tool_call_id to the first message that carries it.
func (m batchRootMeta) payloadTitle() string {
	if m.taskChildren != 1 || m.taskPayload == "" {
		return ""
	}
	var p struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(m.taskPayload), &p); err != nil || len(p.Input) == 0 {
		return ""
	}
	return abbreviateToolInput(string(p.Input))
}

// buildSessionLiveWorkFromBatch is the ON-path replacement for the flat
// live-work snapshot: one item per live root (the tool call the web knows —
// task nodes carry no tool_call_id by schema), states straight from the
// node columns, awaiting-answer badges matched from session_notices (never
// the old store). Complete is always set (a pointer): true on a full read,
// false whenever any chunk or notice read failed.
func buildSessionLiveWorkFromBatch(ctx context.Context, a *appPkg.App, cache *titleCache, sessionID string) (SessionLiveWorkPayload, error) {
	snap := SessionLiveWorkPayload{SessionID: sessionID, Commands: []LiveWorkItemWire{}, Agents: []LiveWorkItemWire{}}
	src := batchSourceFor(a)
	if src == nil || sessionID == "" {
		return snap, nil // conservative: no proof of absence, nothing replaced
	}
	rowsByOwner, visited, walkComplete := batchCollectRows(ctx, src, sessionID)
	complete := walkComplete

	// Terminal roots are absent from the live feed, so the history read
	// adds them before the cap; a root seen live (by root_id) is skipped so
	// an incarnation that went terminal between the two reads is never
	// duplicated.
	roots := collectBatchRoots(rowsByOwner)
	liveRootIDs := make(map[string]bool, len(roots))
	for _, m := range roots {
		liveRootIDs[m.root.RootID] = true
	}
	historyRoots, historyComplete := collectBatchHistoryTerminalRoots(ctx, src, visited, liveRootIDs)
	if !historyComplete {
		complete = false
	}
	roots = append(roots, historyRoots...)
	roots = capBatchRoots(roots)

	questions, questionsComplete := batchPendingQuestionsFor(ctx, a, visited)
	if !questionsComplete {
		complete = false
	}
	snap.Complete = &complete

	// A root started by one direct call carries its own input: only the other
	// roots (batch_run, rows without a payload) need the history lookup.
	jobs := make([]liveWorkTitleJob, 0, len(roots))
	payloadTitles := make(map[liveWorkTitleKey]string, len(roots))
	for _, m := range roots {
		job := liveWorkTitleJob{
			owner:      m.root.OwnerSessionID,
			toolCallID: m.root.ToolCallID.String,
			inputHash:  m.root.InputHash.String,
		}
		if t := m.payloadTitle(); t != "" {
			payloadTitles[liveWorkTitleKey(job)] = t
			continue
		}
		jobs = append(jobs, job)
	}
	titles := cache.titles(ctx, a, jobs)
	for key, t := range payloadTitles {
		titles[key] = t
	}

	for _, m := range roots {
		item := batchRootItem(m, titles, questions)
		if batchAgentTaskKinds[m.taskKind] {
			snap.Agents = append(snap.Agents, item)
		} else {
			snap.Commands = append(snap.Commands, item)
		}
	}
	return snap, nil
}

// collectBatchRoots keeps the live walk's ROOT rows (node_id = root_id) with
// the task kind and child session of each root's task child, in the SQL's
// root-creation order.
func collectBatchRoots(rowsByOwner map[string][]db.BatchNode) []batchRootMeta {
	var metas []batchRootMeta
	for _, rows := range rowsByOwner {
		for _, n := range rows {
			if n.NodeID != n.RootID {
				continue
			}
			m := batchRootMeta{root: n}
			for _, c := range rows {
				if c.NodeID == c.RootID || !c.ParentID.Valid || c.ParentID.String != n.RootID {
					continue
				}
				if c.Kind == "task" {
					m.taskChildren++
					if m.taskChildren == 1 {
						m.taskPayload = c.TaskPayload.String
					}
				}
				if m.taskKind == "" && c.Kind == "task" {
					m.taskKind = c.TaskKind.String
				}
				// The executing task node is the identity the agent family
				// launched on (batch_exec_agent.go uses Launch.ID = the task
				// child's node id, root.1 for one-item roots), so batch
				// questions are asked under IT, not the root.
				if m.askNodeID == "" && c.Kind == "task" {
					m.askNodeID = c.NodeID
				}
				if m.child == "" && c.ChildSessionID.String != "" {
					m.child = c.ChildSessionID.String
				}
			}
			metas = append(metas, m)
		}
	}
	return metas
}

// batchHistoryTerminalBound caps the per-owner terminal-root history read:
// BatchHistoryRootsForOwner has no SQL LIMIT, so the newest
// liveWorkTerminalLimit terminal roots per owner are kept after the read and
// the rest are discarded before any per-root node read. This bounds the
// polled endpoint's cost to one history query + at most
// liveWorkTerminalLimit level queries per owner per snapshot. A root cut by
// this bound takes its delegation children with it, even when a child's own
// history is newer than the retained roots (backlog P2: the SQL has no LIMIT
// to push the bound into).
const batchHistoryTerminalBound = liveWorkTerminalLimit

// collectBatchHistoryTerminalRoots reads the TERMINAL root incarnations
// reachable from the live walk's visited sessions and maps each to a
// batchRootMeta, so finished work stays visible on the flat panel. It
// mirrors batchRootCompletions' bounded BFS: a terminal root is not in the
// live read, so its delegation children would never be visited — each kept
// terminal root's node rows are read (BatchLevelNodes) and the child
// sessions they name form the next owner frontier, depth capped at
// batchHistoryDepth with owner and root_id cycle guards. Roots already in
// the live feed (liveRootIDs, by root_id — a reused tool_call_id is a
// distinct incarnation with its own root_id) are skipped so a root that
// went terminal between the two reads is never duplicated. Each owner's
// read is bounded to the batchHistoryTerminalBound newest terminal roots
// (UpdatedAt desc, tool_call_id tie-break) BEFORE the per-root node reads.
// A session reached only through a terminal root was never live-read, so its
// non-terminal roots (live work behind a finished delegation) are taken from
// the same history read; a session the live walk visited already contributed
// them. An unvisited frontier at the depth cap downgrades (ok=false), like
// the live walk. A failed history or level read only downgrades (ok=false); a root whose
// row WAS read is still returned, and a failed level read skips only that
// root's child frontier — a downgrade never drops a row that was read.
func collectBatchHistoryTerminalRoots(ctx context.Context, src batchNodeSource, visited []string, liveRootIDs map[string]bool) ([]batchRootMeta, bool) {
	metas := []batchRootMeta{}
	ok := true
	seenRoot := map[string]bool{}
	seenOwner := map[string]bool{}
	liveRead := map[string]bool{}
	frontier := make([]string, 0, len(visited))
	for _, owner := range visited {
		liveRead[owner] = true
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
				slog.Warn("live_work: could not read batch root history", "session", owner, "err", err)
				ok = false
				continue
			}
			var terminal, behind []db.BatchNode
			for _, r := range roots {
				if r.NodeID != r.RootID || seenRoot[r.RootID] || liveRootIDs[r.RootID] {
					continue
				}
				switch {
				case batchTerminal(r.State):
					terminal = append(terminal, r)
				case !liveRead[owner]:
					behind = append(behind, r)
				}
			}
			sort.Slice(terminal, func(i, j int) bool {
				if terminal[i].UpdatedAt != terminal[j].UpdatedAt {
					return terminal[i].UpdatedAt > terminal[j].UpdatedAt
				}
				return terminal[i].ToolCallID.String < terminal[j].ToolCallID.String
			})
			if len(terminal) > batchHistoryTerminalBound {
				terminal = terminal[:batchHistoryTerminalBound]
			}
			for _, r := range append(behind, terminal...) {
				seenRoot[r.RootID] = true
				m := batchRootMeta{root: r}
				nodes, err := src.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: owner, RootID: r.RootID})
				if err != nil {
					slog.Warn("live_work: could not read terminal root nodes", "session", owner, "root", r.RootID, "err", err)
					ok = false
					metas = append(metas, m)
					continue
				}
				for _, c := range nodes {
					if c.NodeID == c.RootID {
						continue
					}
					if c.Kind == "task" {
						m.taskChildren++
						if m.taskChildren == 1 {
							m.taskPayload = c.TaskPayload.String
						}
					}
					if m.taskKind == "" && c.Kind == "task" {
						m.taskKind = c.TaskKind.String
					}
					if m.askNodeID == "" && c.Kind == "task" {
						m.askNodeID = c.NodeID
					}
					if m.child == "" && c.ChildSessionID.String != "" {
						m.child = c.ChildSessionID.String
					}
					child := c.ChildSessionID.String
					if c.ChildSessionID.Valid && child != "" && !seenOwner[child] {
						seenOwner[child] = true
						next = append(next, child)
					}
				}
				metas = append(metas, m)
			}
		}
		frontier = next
	}
	if len(frontier) > 0 {
		ok = false
	}
	return metas, ok
}

// capBatchRoots is the flat panel's terminal cap: every running root plus
// the liveWorkTerminalLimit most recently updated finished roots, newest
// first (deterministic tie-break on tool_call_id).
func capBatchRoots(metas []batchRootMeta) []batchRootMeta {
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].root.UpdatedAt != metas[j].root.UpdatedAt {
			return metas[i].root.UpdatedAt > metas[j].root.UpdatedAt
		}
		return metas[i].root.ToolCallID.String < metas[j].root.ToolCallID.String
	})
	kept := make([]batchRootMeta, 0, len(metas))
	terminal := 0
	for _, m := range metas {
		if m.root.State != "running" {
			terminal++
			if terminal > liveWorkTerminalLimit {
				continue
			}
		}
		kept = append(kept, m)
	}
	return kept
}

// batchRootItem maps one root to the flat wire item. Reason carries the
// node cause (the batch vocabulary for why a row left "running"); the title
// falls back to the root's outcome summary like the old result_summary.
// Awaiting-answer keys, in order: legacy notices key by the held
// delegation's tool call id; batch-frame notices (ParseBatchChildQuestionNotice)
// carry no delegation tool call, so they key by the ASKING node id — the
// executing task child the agent family launched on (m.askNodeID, the
// production question identity per batch_exec_agent.go's Launch.ID); the
// root's node id stays the last-resort fallback for legacy root-keyed rows.
func batchRootItem(m batchRootMeta, titles map[liveWorkTitleKey]string, questions map[string]session.ChildQuestion) LiveWorkItemWire {
	root := m.root
	key := liveWorkTitleKey{root.OwnerSessionID, root.ToolCallID.String, root.InputHash.String}
	item := LiveWorkItemWire{
		ToolCallID:     root.ToolCallID.String,
		ToolName:       m.taskKind,
		Title:          titles[key],
		ChildSessionID: m.child,
		StartedAt:      root.CreatedAt * 1000,
		Status:         root.State,
		Reason:         root.Cause,
		LastActivityAt: root.UpdatedAt * 1000,
	}
	if item.Title == "" {
		item.Title = root.OutcomeSummary
	}
	q, ok := questions[item.ToolCallID]
	if !ok {
		q, ok = questions[m.askNodeID]
	}
	if !ok {
		q, ok = questions[root.NodeID]
	}
	if ok {
		item.AwaitingAnswer = true
		item.AwaitingQuestion = q.Question
	}
	if root.State != "running" {
		item.FinishedAt = root.UpdatedAt * 1000
	}
	return item
}

// batchPendingQuestions reads the visited sessions' pending child_question
// notices straight from session_notices and keys them: a BATCH-frame notice
// (the marker-first frame BatchPublishQuestion writes, job_tool_call_id
// NULL) is parsed by ParseBatchChildQuestionNotice and keyed by the asking
// node id (the live root's node id); only then does the legacy
// session-parentheses frame fall through, keyed by the held delegation's
// tool call id as before. It never touches async_jobs: unlike the old void
// rule, a notice is kept unless the row itself is delivered/voided —
// liveness comes from the batch-node walk, not from a job row. ok=false
// when a read failed. Unreacted is enforced by the row itself: the select is
// delivery='pending' only (ListPendingSessionNoticesForOwner), and a row
// leaves 'pending' with reacted=1 only via the pull CAS to 'done'. The
// Reacted != 0 skip is defensive: a pending+reacted row is schema-valid and
// must never badge — its question was already answered.
var batchPendingQuestionsFor = func(ctx context.Context, a *appPkg.App, visited []string) (map[string]session.ChildQuestion, bool) {
	return batchPendingQuestions(ctx, a, visited)
}

func batchPendingQuestions(ctx context.Context, a *appPkg.App, visited []string) (map[string]session.ChildQuestion, bool) {
	questions := map[string]session.ChildQuestion{}
	if a == nil || a.DB == nil {
		return questions, false
	}
	q := db.New(a.DB())
	ok := true
	for _, owner := range visited {
		rows, err := q.ListPendingSessionNoticesForOwner(ctx, owner)
		if err != nil {
			slog.Warn("live_work: could not read pending session notices", "session", owner, "err", err)
			ok = false
			continue
		}
		for _, row := range rows {
			if row.Kind != session.NoticeKindChildQuestion || row.Delivery != "pending" {
				continue
			}
			if row.Reacted != 0 {
				continue
			}
			// Real batch questions first: marker frame, keyed by node id.
			if cq, parsed := session.ParseBatchChildQuestionNotice(row.Text); parsed {
				questions[cq.NodeID] = cq
				continue
			}
			cq, parsed := session.ParseChildQuestionNotice(row.Text, row.JobToolCallID.String)
			if parsed {
				questions[cq.DelegationToolCallID] = cq
			}
		}
	}
	return questions, ok
}
