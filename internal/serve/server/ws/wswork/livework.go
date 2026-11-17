package wswork

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	wire "github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Batch-derived live-work snapshot emitter for the existing flat web panel.

// titleMaxLen bounds the abbreviated command/prompt shown in the panel
// (in RUNES — byte slicing would corrupt multi-byte UTF-8).
const titleMaxLen = 120

// liveWorkTerminalLimit is how many finished rows per list (Commands /
// Agents) a snapshot carries: every running row plus the most recently
// updated finished ones. Without it a long-lived session's panel grows
// without bound.
const liveWorkTerminalLimit = 50

const (
	// liveWorkCoalesceDelay is the dirty-window: marks arriving within it
	// collapse into ONE snapshot per session.
	liveWorkCoalesceDelay = 250 * time.Millisecond
	// liveWorkMarkBuffer bounds the mark channel; a saturated channel means
	// a sweep is already overdue, and dropping a mark is always safe (the
	// next event re-marks).
	liveWorkMarkBuffer = 256
)

// handleGetSessionLiveWork replies to CmdGetSessionLiveWork with one full
// EventSessionLiveWork snapshot for the requested session and its delegation
// descendants. A session that does not exist is an explicit error — the
// snapshot must never be synthesized from other sessions' rows (isolation:
// only the requested session's tree is ever walked, and a job row never
// leaves its owner's tree).
func handleGetSessionLiveWork(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	var p wire.GetSessionLiveWorkPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.Reply(msg.ID, wire.EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.Reply(msg.ID, wire.EventError, nil, "session not found")
		return
	}
	snap, err := buildSessionLiveWork(ctx, a, c.Hub().LiveWorkTitles(), p.SessionID, seams)
	if err != nil {
		c.Reply(msg.ID, wire.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wire.EventSessionLiveWork, snap, "")
}

// buildSessionLiveWork reads the flat projection from batch_nodes and
// session_notices. A partial read is marked incomplete and is never used to
// replace a previously shown snapshot.
func buildSessionLiveWork(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, sessionID string, s Seams) (wire.SessionLiveWorkPayload, error) {
	return buildSessionLiveWorkFromBatch(ctx, a, cache, sessionID, s)
}

// liveWorkTitleJob is the batch-root identity a title lookup is for: the
// owner session, the root's tool call id and input hash (the same three
// facts the old async_jobs row carried — the batch root row carries them
// as tool_call_id / input_hash).
type liveWorkTitleJob struct {
	owner      string
	toolCallID string
	inputHash  string
}

// titles fills the requested rows' titles from the cache and, ONLY for
// keys the cache is missing, reads the owner sessions' history (the
// command/prompt text a job was started with lives only in the assistant
// message's tool_call part — the batch root row stores just its hash). Missing
// text (no message, or an input shape with nothing readable) falls back to
// the row's result_summary at the caller.
//
// Sprint 0 L4 (S4): was the (c *titleCache) titles method; the cache moved to
// hub.TitleCache (handlers_livework.go:135-179 at HEAD) and this stays as a
// free function because it reads internal/app history (§2.6 forbids app in
// hub). Body otherwise verbatim; c.get/c.put became cache.Get/cache.Put and
// the key construction is explicit because liveWorkTitleJob no longer
// converts to the (renamed, field-renamed) hub.TitleKey by identity.
func titles(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, jobs []liveWorkTitleJob) map[hub.TitleKey]string {
	out := make(map[hub.TitleKey]string, len(jobs))
	missing := make(map[hub.TitleKey]struct{})
	for _, j := range jobs {
		key := hub.TitleKey{Owner: j.owner, ToolCallID: j.toolCallID, InputHash: j.inputHash}
		if t, ok := cache.Get(key); ok {
			out[key] = t
		} else {
			missing[key] = struct{}{}
		}
	}
	if len(missing) == 0 {
		return out
	}

	owners := make(map[string]struct{})
	for key := range missing {
		owners[key.Owner] = struct{}{}
	}
	for sessionID := range owners {
		msgs, _, err := a.Messages.ListWithWatermark(ctx, sessionID)
		if err != nil {
			slog.Warn("live_work: could not read history for job titles", "session", sessionID, "err", err)
			continue
		}
		for _, m := range msgs {
			for _, part := range m.Parts {
				tc, ok := part.(message.ToolCall)
				if !ok || tc.Input == "" {
					continue
				}
				for key := range missing {
					if key.ToolCallID != tc.ID || key.Owner != m.SessionID {
						continue
					}
					title := abbreviateToolInput(tc.Input)
					cache.Put(key, title)
					out[key] = title
					delete(missing, key)
				}
			}
		}
	}
	return out
}

// abbreviateToolInput extracts the human-readable payload of a tool call's
// input JSON ("command" for bash/run_command, "prompt"/"url" otherwise) and
// collapses it to one bounded line.
func abbreviateToolInput(input string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return oneLine(input)
	}
	for _, key := range []string{"command", "prompt", "url"} {
		if v, ok := fields[key].(string); ok && v != "" {
			return truncateTitle(oneLine(v))
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateTitle cuts by RUNES, never bytes: s[:n] would split a multi-byte
// UTF-8 sequence (Cyrillic, emoji) into invalid bytes.
func truncateTitle(s string) string {
	if utf8.RuneCountInString(s) <= titleMaxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:titleMaxLen]) + "…"
}

// shouldPushLiveWork reports whether a message event signals async-work
// state change: a notice (job completion, session notice) or a message
// carrying tool parts (the "started" ack, the tool_call itself). Plain
// prose never earns a snapshot; there is no polling loop.
func shouldPushLiveWork(m message.Message) bool {
	if m.NoticeKind != "" {
		return true
	}
	for _, part := range m.Parts {
		switch part.(type) {
		case message.ToolCall, message.ToolResult:
			return true
		}
	}
	return false
}

// liveWorkPusher coalesces per-session dirty marks and builds the snapshots
// OFF the broadcast hot path: the event forwarder only calls mark, which
// never blocks; one coalescing window per burst produces one snapshot per
// dirty session.
type liveWorkPusher struct {
	ctx     context.Context
	a       *appPkg.App
	h       *hub.Hub
	marks   chan string
	stopped chan struct{}
	s       Seams
}

func startLiveWorkPusher(ctx context.Context, a *appPkg.App, h *hub.Hub, seams ...Seams) *liveWorkPusher {
	s := Seams{}
	if len(seams) > 0 {
		s = seams[0]
	}
	p := &liveWorkPusher{
		ctx:     ctx,
		a:       a,
		h:       h,
		marks:   make(chan string, liveWorkMarkBuffer),
		stopped: make(chan struct{}),
		s:       s,
	}
	go p.run()
	return p
}

// mark records a dirty session. Non-blocking by contract: a full channel
// means a sweep is overdue anyway and the next event re-marks the session.
func (p *liveWorkPusher) mark(sessionID string) {
	select {
	case p.marks <- sessionID:
	default:
	}
}

// run drains marks into the dirty set and sweeps once per coalescing
// window. Exits when the server ctx is done; stopped signals that.
func (p *liveWorkPusher) run() {
	defer close(p.stopped)
	dirty := make(map[string]struct{})
	var timer <-chan time.Time
	for {
		select {
		case <-p.ctx.Done():
			return
		case id := <-p.marks:
			dirty[id] = struct{}{}
			if timer == nil {
				timer = time.After(liveWorkCoalesceDelay)
			}
		case <-timer:
			timer = nil
			for id := range dirty {
				p.pushOne(id)
			}
			dirty = make(map[string]struct{})
		}
	}
}

func (p *liveWorkPusher) pushOne(sessionID string) {
	// A session deleted between the mark and the sweep gets NO snapshot:
	// its rows are gone (session delete cascades) and an empty snapshot
	// would only be noise.
	if _, err := p.a.Sessions.Get(p.ctx, sessionID); err != nil {
		return
	}
	snap, err := buildSessionLiveWork(p.ctx, p.a, p.h.LiveWorkTitles(), sessionID, p.s)
	if err != nil {
		slog.Warn("live_work: could not build the change snapshot", "session", sessionID, "err", err)
		return
	}
	// R12-C: an incomplete ON-path read is not broadcast — the client KEEPS
	// the work it already shows instead of replacing it with a partial
	// snapshot (a partial read never silently drops active work). Complete
	// is a pointer: nil means the flag-OFF path built the snapshot, which is
	// always broadcast unchanged.
	if snap.Complete != nil && !*snap.Complete {
		slog.Warn("live_work: skipping an incomplete batch-read snapshot", "session", sessionID)
	} else {
		p.h.Broadcast(wire.EventSessionLiveWork, snap)
	}
	// The flat batch projection also carries structured snapshots in the same coalescing window.
	if bsnap := buildBatchWorkSnapshot(p.ctx, p.a, sessionID, p.s); bsnap.SessionID != "" {
		p.h.Broadcast(wire.EventSessionBatchWork, bsnap)
	}
	// Stage 5b: the same coalescing window also carries the wake-schedule
	// snapshot — a schedule's create/fire/cancel always arrives as a notice
	// or tool message, so the existing marks cover it with no extra
	// subscription and no DB polling.
	pushWakeSchedules(p.ctx, p.a, p.h, sessionID)
}

// R12-C: the batch-runtime read path for the live-work panel — the ONLY
// path. Active work comes from batch_nodes (live roots, delegation tree
// walked exactly like the structured snapshot); TERMINAL roots additionally
// come from the bounded BatchHistoryRootsForOwner history read. NO
// async_jobs SQL exists anywhere on this path.
//
// Incomplete-read rule: a partial or failed read carries complete=false
// (and the pusher skips broadcasting it), so the client KEEPS the work it
// already shows — an incomplete read never silently drops active work.

// batchAgentTaskKind reports whether a batch task belongs in the Agents list.
func batchAgentTaskKind(kind string) bool { return kind == "agent" || kind == "agentic_fetch" }

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
func buildSessionLiveWorkFromBatch(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, sessionID string, seams Seams) (wire.SessionLiveWorkPayload, error) {
	snap := wire.SessionLiveWorkPayload{SessionID: sessionID, Commands: []wire.LiveWorkItemWire{}, Agents: []wire.LiveWorkItemWire{}}
	var sourceFor func(*appPkg.App) BatchNodeSource
	if seams.SourceFor == nil {
		sourceFor = func(a *appPkg.App) BatchNodeSource { return defaultBatchSourceFor(a) }
	} else {
		sourceFor = seams.SourceFor()
	}
	var src BatchNodeSource
	if sourceFor != nil {
		src = sourceFor(a)
	}
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

	var pendingQuestions func(context.Context, *appPkg.App, []string) (map[string]sessionnotice.ChildQuestion, bool)
	if seams.PendingQuestions == nil {
		pendingQuestions = batchPendingQuestions
	} else {
		pendingQuestions = seams.PendingQuestions()
	}
	questions, questionsComplete := pendingQuestions(ctx, a, visited)
	if !questionsComplete {
		complete = false
	}
	snap.Complete = &complete

	// A root started by one direct call carries its own input: only the other
	// roots (batch_run, rows without a payload) need the history lookup.
	jobs := make([]liveWorkTitleJob, 0, len(roots))
	payloadTitles := make(map[hub.TitleKey]string, len(roots))
	for _, m := range roots {
		job := liveWorkTitleJob{
			owner:      m.root.OwnerSessionID,
			toolCallID: m.root.ToolCallID.String,
			inputHash:  m.root.InputHash.String,
		}
		if t := m.payloadTitle(); t != "" {
			payloadTitles[hub.TitleKey{Owner: job.owner, ToolCallID: job.toolCallID, InputHash: job.inputHash}] = t
			continue
		}
		jobs = append(jobs, job)
	}
	titles := titles(ctx, a, cache, jobs)
	for key, t := range payloadTitles {
		titles[key] = t
	}

	for _, m := range roots {
		item := batchRootItem(m, titles, questions)
		if batchAgentTaskKind(m.taskKind) {
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
func collectBatchHistoryTerminalRoots(ctx context.Context, src BatchNodeSource, visited []string, liveRootIDs map[string]bool) ([]batchRootMeta, bool) {
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
func batchRootItem(m batchRootMeta, titles map[hub.TitleKey]string, questions map[string]sessionnotice.ChildQuestion) wire.LiveWorkItemWire {
	root := m.root
	key := hub.TitleKey{Owner: root.OwnerSessionID, ToolCallID: root.ToolCallID.String, InputHash: root.InputHash.String}
	item := wire.LiveWorkItemWire{
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
func batchPendingQuestions(ctx context.Context, a *appPkg.App, visited []string) (map[string]sessionnotice.ChildQuestion, bool) {
	return readBatchPendingQuestions(ctx, a, visited)
}

func readBatchPendingQuestions(ctx context.Context, a *appPkg.App, visited []string) (map[string]sessionnotice.ChildQuestion, bool) {
	questions := map[string]sessionnotice.ChildQuestion{}
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
			if cq, parsed := sessionnotice.ParseBatchChildQuestionNotice(row.Text); parsed {
				questions[cq.NodeID] = cq
				continue
			}
			cq, parsed := sessionnotice.ParseChildQuestionNotice(row.Text, row.JobToolCallID.String)
			if parsed {
				questions[cq.DelegationToolCallID] = cq
			}
		}
	}
	return questions, ok
}
