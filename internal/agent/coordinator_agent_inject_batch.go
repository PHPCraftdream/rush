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
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

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
		seq := batchQuestionMarkerSequence(notice.text)
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
	raw, err := json.Marshal(batchAgentInjectMessage{Depth: hit.Depth, Sequence: hitSeq, Text: mustJSONString(text)})
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
		seq := batchQuestionMarkerSequence(notice.text)
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

// batchQuestionMarkerDepth extracts the depth field ("depth=D") of a
// published question marker header; 0 when absent or unparseable (the
// publisher never writes depth <= 0, so 0 disqualifies the row, like an
// unparseable marker).
func batchQuestionMarkerDepth(text string) int {
	if !strings.HasPrefix(text, "[batch-question ") {
		return 0
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		if name, value, ok := strings.Cut(field, "="); ok && name == "depth" {
			d, err := strconv.Atoi(value)
			if err != nil {
				return 0
			}
			return d
		}
	}
	return 0
}

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
