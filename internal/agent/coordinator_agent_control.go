package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

// Coordinator-side implementation of tools.AgentControl — the
// inspect_agent/inject_agent/stop_agent tool family (task #1024, stage 3 of
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md; contract:
// docs/plans/2026-09-27-wake-tools-contract.md §4). Every method routes
// through agentFor (task #1054), so a delegated child's driver SessionAgent —
// not c.currentAgent — answers busy checks, injects and cancels.

var _ tools.AgentControl = (*coordinator)(nil)

// controlChild is the shared ownership check (§4): the child must exist and
// be a direct child of the caller's session. Same wording as runSubAgent's
// resume_session_id check, with the action verb swapped in.
func (c *coordinator) controlChild(ctx context.Context, callerSessionID, childSessionID, verb string) error {
	child, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("child_session_id %q not found: %s", childSessionID, err)
	}
	if child.ParentSessionID != callerSessionID {
		return fmt.Errorf(
			"child_session_id %q is not a child of this session; refusing to %s a session that does not belong to this agent call",
			childSessionID, verb,
		)
	}
	return nil
}

// childDelegationState reports childID's LIVE delegation state from the
// batch facts: "running" when the parent's live tree still names childID as
// its delegation child. ok is false when no live delegation exists — a
// child that was never delegated, or one whose delegation already delivered
// (invisible here by design; inspect_agent then falls back to the child's
// message history).
func (c *coordinator) childDelegationState(ctx context.Context, childID string) (state string, ok bool) {
	parent := c.childParentSession(childID)
	if parent == "" {
		return "", false
	}
	live, err := coordinatorBatchCutoverFacts(c).LiveDelegationChildOf(ctx, parent, childID)
	if err != nil || !live {
		return "", false
	}
	return "running", true
}

// childHasActiveWork reports whether childID has anything stop_agent could
// stop: a live turn (via its driver-aware busy check), live batch work it
// owns (its own task roots), or owned background shells. A failed live read
// is conservative (active).
func (c *coordinator) childHasActiveWork(childID string) bool {
	if c.agentFor(childID).IsSessionBusy(childID) {
		return true
	}
	own, desc, err := coordinatorBatchCutoverFacts(c).LiveWorkSummary(context.Background(), childID)
	if err != nil || own+desc > 0 {
		return true
	}
	return c.background != nil && (c.background.ActiveOwned(childID) > 0 || c.background.PendingCompletionsOwned(childID) > 0)
}

// childLastActivity reads the child's last FINISHED assistant message for
// inspect_agent's last_activity_* fields (the same List and the same
// finished-assistant scan refreshSubAgentCompletion uses) and reports
// whether it ends on a question-tool stop (§4.1's awaiting_answer signal).
// #1157: the literal "last message" of a paused child is its ask_question
// tool RESULT -- a tool message with no finish part -- which made a
// question-paused child read as idle; the question lives on the assistant
// message before it.
func (c *coordinator) childLastActivity(ctx context.Context, childID string) (msg *message.Message, awaiting bool) {
	msgs, err := c.messages.List(ctx, childID)
	if err != nil {
		return nil, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant || !m.IsFinished() {
			continue
		}
		fp := m.FinishPart()
		awaiting = fp != nil && fp.Reason == message.FinishReasonError &&
			fp.Message == awaitingAnswerStoppedTitle
		return &m, awaiting
	}
	return nil, false
}

// InspectAgent implements tools.AgentControl (§4.1). Priority: a CONFIRMED
// held batch question (the fenced actionable route; carries the fenced
// held_question identity) beats everything; then running (driver-aware
// busy); then awaiting_answer (the exact
// question-stop finish literal); then a terminal registry state; then
// armed-but-not-running (idle); then a history-derived terminal for an
// already-delivered (gc'd) delegation; then idle for a never-run child.
func (c *coordinator) InspectAgent(ctx context.Context, callerSessionID, childSessionID string) (tools.AgentInspection, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "inspect"); err != nil {
		return tools.AgentInspection{}, err
	}
	insp := tools.AgentInspection{ChildSessionID: childSessionID}
	last, awaiting := c.childLastActivity(ctx, childSessionID)
	// A confirmed CURRENT held batch question outranks the generic busy
	// state: the batch ask parks the run INSIDE a tool call, so
	// IsSessionBusy alone masks the question as "running" and the parent
	// never learns it must answer. Only the fenced actionable route (live
	// incarnation, exact token) promotes the status; an ordinary executing
	// child stays "running"; an unreadable read falls through to the old
	// behaviour (never a fabricated awaiting_answer); answered/stale/
	// terminal/foreign-scope notices never change the status.
	var heldRoute *batchlife.BatchHeldQuestionRoute
	if src, ok := coordinatorBatchCutoverFacts(c).(batchlife.BatchHeldQuestionSource); ok {
		if routes, rerr := src.ActionableHeldQuestions(ctx, callerSessionID); rerr == nil {
			for i := range routes {
				if routes[i].ChildSessionID == childSessionID {
					heldRoute = &routes[i]
					break
				}
			}
		}
	}
	switch {
	case heldRoute != nil:
		insp.Status = "awaiting_answer"
		insp.HeldQuestion = &tools.AgentHeldQuestion{
			NodeID: heldRoute.NodeID, Token: heldRoute.Token,
			Depth: heldRoute.Depth, Sequence: heldRoute.Sequence,
		}
	case c.agentFor(childSessionID).IsSessionBusy(childSessionID):
		insp.Status = "running"
	case awaiting:
		insp.Status = "awaiting_answer"
	default:
		state, ok := c.childDelegationState(ctx, childSessionID)
		if ok {
			// §4.1: "running" means the child executes a turn (the busy case
			// above). A live delegation whose child is between turns is idle.
			if state == "running" {
				state = "idle"
			}
			insp.Status = state
		} else if last != nil {
			// Delivered (gc'd) delegation: derive the terminal state from
			// the last message's finish reason.
			insp.Status = finishedStatusFor(last)
		} else {
			insp.Status = "idle"
		}
	}
	if ag := c.agentFor(childSessionID); ag != nil {
		insp.QueuedMessages = len(ag.QueuedPromptsList(childSessionID))
	}
	if last != nil {
		insp.LastActivityAt = time.Unix(last.CreatedAt, 0).UTC().Format(time.RFC3339)
		insp.LastActivitySummary = tools.TruncateOutput(last.FullText())
	}
	return insp, nil
}

// ListDelegations implements tools.AgentControl: the no-argument
// inspect_agent listing. Live means the caller still owns the delegation in
// the batch facts (a live delegation node naming a child session); a
// delivered delegation is terminal in the facts by design and is not
// listed. Ordered oldest-first by start time.
func (c *coordinator) ListDelegations(ctx context.Context, callerSessionID string) ([]tools.AgentDelegationSummary, error) {
	nodes, err := coordinatorBatchCutoverFacts(c).LiveDelegationNodes(ctx, callerSessionID)
	if err != nil {
		return nil, err
	}
	type liveEntry struct {
		childID   string
		toolName  string
		startedAt time.Time
	}
	entries := make([]liveEntry, 0, len(nodes))
	for _, node := range nodes {
		if node.ChildSessionID == "" {
			continue
		}
		toolName := node.TaskKind
		if toolName == "" {
			toolName = "agent"
		}
		entries = append(entries, liveEntry{childID: node.ChildSessionID, toolName: toolName, startedAt: node.StartedAt})
	}
	// Oldest first, with the child id breaking ties: two delegations armed
	// inside the same clock tick read an equal startedAt (the process clock
	// is far coarser than the gap between two launches), and an unstable
	// sort would then list them in random map order.
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].startedAt.Equal(entries[j].startedAt) {
			return entries[i].startedAt.Before(entries[j].startedAt)
		}
		return entries[i].childID < entries[j].childID
	})
	summaries := make([]tools.AgentDelegationSummary, 0, len(entries))
	for _, e := range entries {
		insp, err := c.InspectAgent(ctx, callerSessionID, e.childID)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, tools.AgentDelegationSummary{
			ChildSessionID: e.childID,
			ToolName:       e.toolName,
			Status:         insp.Status,
			StartedAt:      e.startedAt.UTC().Format(time.RFC3339),
			AgeSeconds:     int(time.Since(e.startedAt).Seconds()),
			LastActivityAt: insp.LastActivityAt,
		})
	}
	return summaries, nil
}

// finishedStatusFor derives a terminal inspect status from a delivered
// delegation's last child message.
func finishedStatusFor(msg *message.Message) string {
	if fp := msg.FinishPart(); fp != nil {
		switch fp.Reason {
		case message.FinishReasonError:
			return "failed"
		case message.FinishReasonCanceled:
			return "cancelled"
		}
	}
	return "finished"
}

func (c *coordinator) DrainCredentialBoundary(ctx context.Context, sessionID string) bool {
	return c.drainCredentialBoundary(ctx, sessionID)
}

// be refused for lack of its tenant credentials. The in-process root marker is
// always consulted; the durable lineage walk (an extra session read per drain)
// runs only when a durable marker store is bound, so a coordinator without one
// keeps its previous behaviour exactly.
func (c *coordinator) drainCredentialBoundary(ctx context.Context, sessionID string) bool {
	if c.isCredentialedRoot(sessionID) {
		return true
	}
	if c.credentialReqs == nil {
		return false
	}
	return c.credentialedSessionOrAncestor(ctx, sessionID)
}

func (c *coordinator) credentialedSessionOrAncestor(ctx context.Context, sessionID string) bool {
	for sessionID != "" {
		if c.isCredentialedRoot(sessionID) {
			return true
		}
		if c.credentialReqs != nil {
			hasRequirement, err := c.credentialReqs.HasSessionCredentialRequirement(ctx, sessionID)
			if err != nil {
				return true // A durable lookup failure must not permit configured-provider fallback.
			}
			if hasRequirement {
				return true
			}
		}
		sess, err := c.sessions.Get(ctx, sessionID)
		if err != nil {
			return true
		}
		sessionID = sess.ParentSessionID
	}
	return false
}

// InjectAgent implements tools.AgentControl (§4.2).
func (c *coordinator) InjectAgent(ctx context.Context, callerSessionID, childSessionID, msg string, interrupt bool) (string, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "inject"); err != nil {
		return "", err
	}
	busy := c.agentFor(childSessionID).IsSessionBusy(childSessionID)
	if !interrupt {
		// Batch held-question bridge (D1): a committed, CURRENT-incarnation
		// held question of this child releases through the node's execution
		// (Runner.Inject) instead of the mailbox. A sink-less coordinator or
		// a "no held question" answer keeps the legacy path byte-identical.
		fact, batchErr := c.deliverBatchHeldAnswer(ctx, callerSessionID, childSessionID, msg)
		if batchErr == nil {
			return fmt.Sprintf(
				"Answer delivered to the held question of the existing batch node %s (token %d, depth %d); the sub-agent continues in the SAME batch execution - no new generation.",
				fact.NodeID, fact.Token, fact.Depth,
			), nil
		}
		if !errors.Is(batchErr, errBatchInjectBridgeMiss) {
			return "", batchErr
		}
		if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
			return "", err
		}
		if busy {
			return "Message delivered into the sub-agent's running turn; it merges at the next step boundary.", nil
		}
		return fmt.Sprintf(
			"Message saved to the sub-agent's session; it is idle, so nothing runs until you call agent with resume_session_id=%q.",
			childSessionID,
		), nil
	}
	ctx, ownedScope := ownDeferredCallScope(ctx)
	scopeTransferred := false
	if ownedScope != nil {
		defer func() {
			if !scopeTransferred {
				ownedScope.Close()
			}
		}()
	}
	driver, hasDriver := c.subAgentDrivers.Get(childSessionID)
	credentialed := c.credentialedSessionOrAncestor(ctx, childSessionID) || hasDriver && driver.Call.Credentials != nil
	if credentialed {
		if !hasDriver || driver.Call.Credentials == nil {
			return "", fmt.Errorf("%w: child session %s has no driver credential binding", diskport.ErrCredentialBindingUnavailable, childSessionID)
		}
		// Credentialed child work may only be replaced from its registered
		// driver's frozen call template. Resolving against coordinator config
		// here could silently switch the tenant to the operator's provider.
		if driver.Call.SmartModel == nil || driver.Call.SmartModel.Model == nil {
			return "", fmt.Errorf("%w: child session %s has no usable driver template", diskport.ErrCredentialBindingUnavailable, childSessionID)
		}
		if err := driver.Call.Credentials.Validate(); err != nil {
			return "", fmt.Errorf("%w: child session %s has unusable driver credentials: %v", diskport.ErrCredentialBindingUnavailable, childSessionID, err)
		}
		call := driver.CallFor(msg)
		call.SessionID = childSessionID
		call.LogicalCallID = uuid.NewString()
		callOptions := call.CallOptions
		if callOptions == nil {
			callOptions = callOptionsFrom(ctx)
		}
		if callOptions != nil {
			if ownedScope != nil {
				ownedScope.Close()
			}
			ownedOptions := *callOptions
			ownedOptions.BuildScope = newBuildResourceScope()
			call.CallOptions = &ownedOptions
			ownedScope = ownedOptions.BuildScope
		}
		if ownedScope != nil {
			call.OnQueueResolved = func(_ *fantasy.AgentResult, _ error) { ownedScope.Close() }
		}
		if c.agentFor(childSessionID).InterruptAndReplace(childSessionID, call) {
			scopeTransferred = true
			return "Interrupted the sub-agent's current turn; it will process this message next.", nil
		}
		if ownedScope != nil {
			ownedScope.Close()
			scopeTransferred = true
		}
		if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
			return "", err
		}
		return "child session was idle, nothing to interrupt — message queued instead; call agent with resume_session_id to resume it", nil
	}
	pinned, err := c.resolveSessionModels(ctx, childSessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models for inject: %w", err)
	}
	call, err := c.buildCall(ctx, childSessionID, msg, pinned, nil)
	if err != nil {
		return "", err
	}
	if ownedScope != nil {
		call.OnQueueResolved = func(_ *fantasy.AgentResult, _ error) { ownedScope.Close() }
	}
	if c.agentFor(childSessionID).InterruptAndReplace(childSessionID, call) {
		scopeTransferred = true
		return "Interrupted the sub-agent's current turn; it will process this message next.", nil
	}
	// Idle fallback does not accept this built call; release its privately
	// resolved scope before preserving the historical InjectMessage behavior.
	if ownedScope != nil {
		ownedScope.Close()
		scopeTransferred = true
	}
	// Idle: nothing to interrupt — fall back to the queue behavior and say so.
	if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
		return "", err
	}
	return "child session was idle, nothing to interrupt — message queued instead; call agent with resume_session_id to resume it", nil
}

// StopAgent implements tools.AgentControl (§4.3): batch Stop via Cancel
// (stopTree: the child's live work, its own roots, wake zeroing and
// auto-resume suspension) plus the driver-aware Cancel for the child's live
// generation. Idempotent: a second call finds nothing left to stop.
func (c *coordinator) StopAgent(ctx context.Context, callerSessionID, childSessionID string) (string, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "stop"); err != nil {
		return "", err
	}
	if !c.childHasActiveWork(childSessionID) {
		return "", fmt.Errorf("child_session_id %q has no active turn or pending delegation to stop", childSessionID)
	}
	c.Cancel(childSessionID)
	return fmt.Sprintf(
		"Sub-agent session %s stopped. Its history is preserved; resume with agent(resume_session_id=%q, prompt=\"...\") if needed.",
		childSessionID, childSessionID,
	), nil
}

// inject_agent bridge over the batch runtime's held questions (design D1:
// inject_agent is a direct control that must stay available to release a
// held execution — docs/plans/2026-10-12-job-batches-rush-integration.md).
// Production publishes a held child question with the marker header
// "[batch-question node=X token=N sequence=S depth=D child=C]"
// (batch_install_question.go) and the notice text tells the model to answer
// with inject_agent — but nothing wired Runner.Inject, so the answer could
// never release the child's held Ask and the child stayed parked until
// Stop/timeout. This file adds that bridge: a narrow sink installed at App
// install time (flag ON only), registered per coordinator, and consumed by
// the non-interrupt branch of InjectAgent. Flag OFF or a runnerless install
// never installs a sink, so InjectAgent keeps its legacy behaviour
// byte-identically.
//
// Pool discipline: the batch writer pool has exactly ONE connection
// (internal/db/connect.go SetMaxOpenConns(1)), so this file NEVER issues a
// second query on the pool while a rows/stmt/tx is open, and NEVER holds
// one across Runner.Inject — the notice rows are collected and closed
// first, then the per-candidate fence queries run (the LiveHeldQuestions
// shape).

// BatchHeldQuestionFact is the fenced identity of the held question an
// answer was delivered to. Token is the CURRENT incarnation's token from
// batch_nodes (int64 — the column's type), Depth the held nesting depth the
// answer addressed.
type BatchHeldQuestionFact struct {
	Owner, NodeID string
	Token         int64
	Depth         int
}

var (
	// ErrBatchHeldAnswerNone: no committed held question names this child
	// session for this owner. The coordinator treats it as a bridge MISS and
	// takes the legacy InjectMessage path unchanged.
	ErrBatchHeldAnswerNone = errors.New("batch inject bridge: no committed held question for this child session")
	// errBatchInjectBridgeMiss is the internal sentinel InjectAgent uses to
	// fall back to the legacy path; never returned to callers.
	errBatchInjectBridgeMiss = errors.New("batch inject bridge: miss")
)

// BatchHeldAnswerSink routes one inject_agent answer into the CURRENT
// incarnation's held question of the child's batch node. Implemented over
// the session_notices + batch_nodes fenced read and jobbatch.Runner.Inject.
// The lookup is REACTED-INDEPENDENT: the owner's reaction flow marks every
// notice reacted as soon as the parent produces any message, so `reacted`
// never means "answered". Instead the sink CLOSES the notice rows on
// acceptance (delivery='void', wake=0), so an answered question is gone
// from every future lookup — a reacted question whose answer was never
// accepted stays routable (the production hang).
type BatchHeldAnswerSink interface {
	DeliverHeldAnswer(ctx context.Context, owner, childSessionID, text string) (BatchHeldQuestionFact, error)
	// Close disarms the sink permanently (the shutdown/drain path). After
	// Close every delivery is refused deterministically — a sink that was
	// already loaded by an in-flight inject_agent must fail like a released
	// execution (jobbatch.ErrReleased), never degrade silently to the
	// mailbox: a half-delivered answer must not be double-routed.
	Close()
}

// batchInjectRunner is the sink's runner seam: only Inject. *jobbatch.Runner
// satisfies it; tests inject a recording fake.
type batchInjectRunner interface {
	Inject(ctx context.Context, id jobbatch.ID, msg json.RawMessage) error
}

// NewBatchHeldAnswerSink builds the production sink over the batch writer
// pool and the installed Runner. Nil arguments fail closed: the App then
// installs no sink and inject_agent keeps its legacy behaviour.
func NewBatchHeldAnswerSink(writer *sql.DB, runner batchInjectRunner) (BatchHeldAnswerSink, error) {
	if writer == nil || runner == nil {
		return nil, errors.New("batch inject bridge: writer and runner are required")
	}
	return &batchHeldAnswerStoreSink{writer: writer, runner: runner}, nil
}

// batchHeldAnswerStoreSink is the production sink. The lookup reuses
// LiveHeldQuestions' exact fence (batch_cutover_facts_node.go): the notice
// must be a pending-or-delivered, wake=1 child_question row of the owner
// (NO reacted predicate — see the file header) carrying a parseable
// production marker, and the named batch_nodes row must exist for this
// owner with a live state and the marker's exact token — the incarnation
// fence. On top of that fence the sink filters to the marker's child field
// naming THIS child session and extracts the marker's depth (the answer
// addresses exactly the held depth; inject_agent's schema has no depth
// parameter). On acceptance the matched notice rows are closed, which is
// what keeps the reacted-independent lookup set from growing forever.
type batchHeldAnswerStoreSink struct {
	writer *sql.DB
	runner batchInjectRunner
	closed atomic.Bool
}

// Close disarms the sink; see the interface doc. Called by the App's
// release gate BEFORE the Runner drains.
func (s *batchHeldAnswerStoreSink) Close() { s.closed.Store(true) }

// closedErr is the deterministic post-Close rejection: the execution this
// answer was headed for is being released under the caller.
func closedErr() error {
	return fmt.Errorf("batch inject bridge closed: %w", jobbatch.ErrReleased)
}

// batchHeldQuestionNoticeRow is one candidate notice: its row id (for the
// close-on-acceptance UPDATE) and text.
type batchHeldQuestionNoticeRow struct {
	id   int64
	text string
}

// heldQuestionNotices returns the owner's wake=1 child_question notices,
// oldest first. This is the retained-debt predicate WITHOUT the reacted
// predicate (pending OR already-delivered rows still hold the session while
// wake=1): the owner's reaction flow (BatchReactionMarkReactedWithMessageUpdate
// → MarkSessionNoticesReactedForOwner) marks every notice reacted when the
// parent produces any message, so a reacted question whose answer was never
// accepted must stay routable (the production hang). The close-on-acceptance
// UPDATE below is what prevents this set from growing forever. The result
// set is fully read and closed before the caller issues another query on
// the pool.
func (s *batchHeldAnswerStoreSink) heldQuestionNotices(ctx context.Context, owner string) ([]batchHeldQuestionNoticeRow, error) {
	rows, err := s.writer.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 ORDER BY id`,
		owner, session.NoticeKindChildQuestion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []batchHeldQuestionNoticeRow
	for rows.Next() {
		var n batchHeldQuestionNoticeRow
		if err := rows.Scan(&n.id, &n.text); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}

func (s *batchHeldAnswerStoreSink) DeliverHeldAnswer(ctx context.Context, owner, childSessionID, text string) (BatchHeldQuestionFact, error) {
	if s.closed.Load() {
		return BatchHeldQuestionFact{}, closedErr()
	}
	// Phase 1: collect ALL candidate notice rows; the rows are closed when
	// the helper returns, before any second pool query — the
	// single-connection writer would otherwise deadlock on the nested
	// batch_nodes reads.
	notices, err := s.heldQuestionNotices(ctx, owner)
	if err != nil {
		return BatchHeldQuestionFact{}, err
	}
	// Phase 2: per-candidate fence queries run with NO open rows.
	var (
		candidate bool // some parseable marker names THIS child
		hit       *BatchHeldQuestionFact
		hitSeq    uint64 // the newest fence-in candidate's marker sequence
		fenceErr  error  // deterministic fence failure of the newest candidate
	)
	for _, notice := range notices {
		if childSessionID == "" || batchQuestionMarkerChild(notice.text) != childSessionID {
			continue
		}
		node, token, ok := parseBatchQuestionMarker(notice.text)
		depth := batchQuestionMarkerDepth(notice.text)
		seq := batchlife.QuestionMarkerSequence(notice.text)
		if !ok || depth <= 0 || seq <= 0 {
			continue
		}
		candidate = true
		hit, hitSeq, fenceErr = nil, 0, nil
		var rowToken int64
		var state, rootDelivery string
		ferr := s.writer.QueryRowContext(ctx,
			`SELECT n.token, n.state, r.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id WHERE n.node_id = ?1 AND n.owner_session_id = ?2`,
			node, owner).Scan(&rowToken, &state, &rootDelivery)
		switch {
		case ferr != nil:
			fenceErr = fmt.Errorf("batch node %s: %w: no live batch_nodes row for this owner", node, ErrBatchAgentStaleGeneration)
		case !batchNodeStateLive(state) || rowToken != token || rootDelivery == "void":
			fenceErr = fmt.Errorf("batch node %s: %w: notice token %d vs node token %d state %q", node, ErrBatchAgentStaleGeneration, token, rowToken, state)
		default:
			hit = &BatchHeldQuestionFact{Owner: owner, NodeID: node, Token: rowToken, Depth: depth}
			hitSeq = uint64(seq) // safe: seq > 0 was required above
		}
	}
	if hit == nil {
		if candidate && fenceErr != nil {
			return BatchHeldQuestionFact{}, fenceErr
		}
		return BatchHeldQuestionFact{}, ErrBatchHeldAnswerNone
	}
	// Re-check after the (possibly slow) fence reads: a concurrent drain
	// must not receive an answer routed across the closed boundary.
	if s.closed.Load() {
		return *hit, closedErr()
	}
	// The wire shape is the production batchAgentInjectMessage: the depth
	// must name the held depth AND the sequence the exact published question
	// the answer addresses (deliver refuses any other depth or sequence) and
	// the text is the JSON-encoded answer string
	// (batchAgentAskQuestionTool unmarshals it back).
	raw, err := json.Marshal(batchinst.AgentInjectMessage{Depth: hit.Depth, Sequence: hitSeq, Text: mustJSONString(text)})
	if err != nil {
		return *hit, err
	}
	// No rows/tx is open here: the pool connection is free for
	// Runner.Inject's own control request.
	if err := s.runner.Inject(ctx, jobbatch.ID(hit.NodeID), raw); err != nil {
		// Deterministic rejections (ErrBatchAgentStaleGeneration,
		// ErrBatchAgentStopped, ErrBatchAgentNotHeld,
		// jobbatch.ErrReleased, ...) propagate wrapped with the node —
		// never swallowed, never delivered to a new incarnation.
		return *hit, fmt.Errorf("batch node %s: %w", hit.NodeID, err)
	}
	// Accepted: close the matched notice rows with ONE ExecContext (no rows
	// open here). Every open question of this child's incarnation is stale
	// after the answer, so all matched rows close together. If the close
	// fails, the answer WAS delivered: report it — a retry then fails
	// deterministically (notice gone → ErrBatchHeldAnswerNone → legacy
	// mailbox, or the runner's NotHeld), never double-delivering.
	var matchedNotices []int64
	for _, notice := range notices {
		node, token, ok := parseBatchQuestionMarker(notice.text)
		seq := batchlife.QuestionMarkerSequence(notice.text)
		if ok && node == hit.NodeID && token == hit.Token && seq > 0 && uint64(seq) <= hitSeq &&
			batchQuestionMarkerChild(notice.text) == childSessionID {
			matchedNotices = append(matchedNotices, notice.id)
		}
	}
	placeholders := make([]string, len(matchedNotices))
	args := make([]any, 0, len(matchedNotices)+2)
	args = append(args, time.Now().Unix(), session.NoticeKindChildQuestion)
	for i, id := range matchedNotices {
		placeholders[i] = fmt.Sprintf("?%d", i+3)
		args = append(args, id)
	}
	closeSQL := fmt.Sprintf(
		`UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = ?1 WHERE kind = ?2 AND id IN (%s)`,
		strings.Join(placeholders, ", "))
	if _, cerr := s.writer.ExecContext(ctx, closeSQL, args...); cerr != nil {
		return *hit, fmt.Errorf("batch node %s: close answered question notice: %w", hit.NodeID, cerr)
	}
	return *hit, nil
}

// mustJSONString encodes one answer string as a JSON string value.
// json.Marshal of a string cannot fail; the helper keeps the error shape
// explicit for the RawMessage field.
func mustJSONString(text string) json.RawMessage {
	raw, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}

func batchQuestionMarkerDepth(text string) int { return batchlife.QuestionMarkerDepth(text) }

// coordinatorHeldAnswerSinks maps a coordinator onto its installed sink
// (one App install per coordinator; the App closes and clears the entry on
// shutdown, mirroring the batchTimeoutWake dispatcher lifecycle). Keyed per
// coordinator — never a process global — so separate App instances stay
// isolated like the rest of the batch binding surface.
var coordinatorHeldAnswerSinks sync.Map

// SetCoordinatorBatchHeldAnswerSink installs (sink != nil) or clears (sink
// == nil) the coordinator's held-answer sink. Called by the App at agent
// setup and again on shutdown.
func SetCoordinatorBatchHeldAnswerSink(c Coordinator, sink BatchHeldAnswerSink) {
	if c == nil {
		return
	}
	if sink == nil {
		coordinatorHeldAnswerSinks.Delete(c)
		return
	}
	coordinatorHeldAnswerSinks.Store(c, sink)
}

func coordinatorHeldAnswerSinkFor(c *coordinator) BatchHeldAnswerSink {
	if c == nil {
		return nil
	}
	if v, ok := coordinatorHeldAnswerSinks.Load(c); ok {
		if sink, ok := v.(BatchHeldAnswerSink); ok {
			return sink
		}
	}
	return nil
}

// BatchHeldAnswerer is implemented by coordinators that can answer a held
// batch question strictly: unlike inject_agent it never falls back to the
// mailbox, so a stale or absent question is an error the caller can report.
type BatchHeldAnswerer interface {
	AnswerBatchHeldQuestion(ctx context.Context, owner, childSessionID, text string) (BatchHeldQuestionFact, error)
}

// AnswerBatchHeldQuestion delivers text to the CURRENT incarnation's held
// question of owner's direct child. ErrBatchHeldAnswerNone means no committed
// question names the child; the stale/stopped sentinels of the execution pass
// through unchanged.
func (c *coordinator) AnswerBatchHeldQuestion(ctx context.Context, owner, childSessionID, text string) (BatchHeldQuestionFact, error) {
	if err := c.controlChild(ctx, owner, childSessionID, "answer"); err != nil {
		return BatchHeldQuestionFact{}, err
	}
	fact, err := c.deliverBatchHeldAnswer(ctx, owner, childSessionID, text)
	if errors.Is(err, errBatchInjectBridgeMiss) {
		return BatchHeldQuestionFact{}, ErrBatchHeldAnswerNone
	}
	return fact, err
}

// deliverBatchHeldAnswer routes one non-interrupt inject_agent message
// through the installed sink. A sink-less coordinator (flag OFF, install
// absent, or shutdown cleared) or a "no held question" answer returns
// errBatchInjectBridgeMiss so InjectAgent falls back to the legacy path
// byte-identically; any other error — including a sink closed mid-flight —
// is deterministic and propagates.
func (c *coordinator) deliverBatchHeldAnswer(ctx context.Context, callerSessionID, childSessionID, msg string) (BatchHeldQuestionFact, error) {
	sink := coordinatorHeldAnswerSinkFor(c)
	if sink == nil {
		return BatchHeldQuestionFact{}, errBatchInjectBridgeMiss
	}
	fact, err := sink.DeliverHeldAnswer(ctx, callerSessionID, childSessionID, msg)
	if err != nil {
		if errors.Is(err, ErrBatchHeldAnswerNone) {
			return BatchHeldQuestionFact{}, errBatchInjectBridgeMiss
		}
		return BatchHeldQuestionFact{}, err
	}
	return fact, nil
}
