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
// must be a pending-or-delivered, unreacted, wake=1 child_question row of
// the owner carrying a parseable production marker, and the named
// batch_nodes row must exist for this owner with a live state and the
// marker's exact token — the incarnation fence. On top of that fence the
// sink filters to the marker's child field naming THIS child session and
// extracts the marker's depth (the answer addresses exactly the held depth;
// inject_agent's schema has no depth parameter).
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

// heldQuestionNotices returns the texts of the owner's unreacted wake=1
// child_question notices, oldest first. Same retained-debt predicate
// LiveHeldQuestions runs (pending OR already-delivered rows still hold the
// session while unreacted and wake=1). The result set is fully read and
// closed before the caller issues another query on the pool.
func (s *batchHeldAnswerStoreSink) heldQuestionNotices(ctx context.Context, owner string) ([]string, error) {
	rows, err := s.writer.QueryContext(ctx,
		`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0 ORDER BY id`,
		owner, session.NoticeKindChildQuestion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []string
	for rows.Next() {
		var notice string
		if err := rows.Scan(&notice); err != nil {
			return nil, err
		}
		notices = append(notices, notice)
	}
	return notices, rows.Err()
}

func (s *batchHeldAnswerStoreSink) DeliverHeldAnswer(ctx context.Context, owner, childSessionID, text string) (BatchHeldQuestionFact, error) {
	if s.closed.Load() {
		return BatchHeldQuestionFact{}, closedErr()
	}
	// Phase 1: collect ALL candidate notice texts; the rows are closed when
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
		fenceErr  error // deterministic fence failure of the newest candidate
	)
	for _, notice := range notices {
		if childSessionID == "" || batchQuestionMarkerChild(notice) != childSessionID {
			continue
		}
		node, token, ok := parseBatchQuestionMarker(notice)
		depth := batchQuestionMarkerDepth(notice)
		if !ok || depth <= 0 {
			continue
		}
		candidate = true
		hit, fenceErr = nil, nil
		var rowToken int64
		var state string
		ferr := s.writer.QueryRowContext(ctx,
			`SELECT token, state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, owner).Scan(&rowToken, &state)
		switch {
		case ferr != nil:
			fenceErr = fmt.Errorf("batch node %s: %w: no live batch_nodes row for this owner", node, ErrBatchAgentStaleGeneration)
		case !batchNodeStateLive(state) || rowToken != token:
			fenceErr = fmt.Errorf("batch node %s: %w: notice token %d vs node token %d state %q", node, ErrBatchAgentStaleGeneration, token, rowToken, state)
		default:
			hit = &BatchHeldQuestionFact{Owner: owner, NodeID: node, Token: rowToken, Depth: depth}
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
	// must name the held depth (deliver refuses any other depth) and the
	// text is the JSON-encoded answer string (batchAgentAskQuestionTool
	// unmarshals it back).
	raw, err := json.Marshal(batchAgentInjectMessage{Depth: hit.Depth, Text: mustJSONString(text)})
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
