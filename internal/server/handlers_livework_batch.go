package server

// R12-C: the batch-runtime ON read path for the live-work panel. Active work
// comes ONLY from batch_nodes (live roots, delegation tree walked exactly
// like the structured snapshot); NO async_jobs SQL executes on this path.
// The OFF path (buildSessionLiveWork below the flag branch) is unchanged.
//
// Incomplete-read rule: a partial or failed read carries complete=false
// (and the pusher skips broadcasting it), so the client KEEPS the work it
// already shows — an incomplete read never silently drops active work.

import (
	"context"
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

	roots := collectBatchRoots(rowsByOwner)
	roots = capBatchRoots(roots)

	questions, questionsComplete := batchPendingQuestions(ctx, a, visited)
	if !questionsComplete {
		complete = false
	}
	snap.Complete = &complete

	jobs := make([]db.AsyncJob, 0, len(roots))
	for _, m := range roots {
		jobs = append(jobs, db.AsyncJob{
			OwnerSessionID: m.root.OwnerSessionID,
			ToolCallID:     m.root.ToolCallID.String,
			InputHash:      m.root.InputHash.String,
		})
	}
	titles := cache.titles(ctx, a, jobs)

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

// capBatchRoots mirrors capLiveWorkRows: every running root plus the
// liveWorkTerminalLimit most recently updated finished roots, newest first
// (deterministic tie-break on tool_call_id).
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
