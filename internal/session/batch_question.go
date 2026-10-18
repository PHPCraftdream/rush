package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BatchPublishQuestion durably publishes one held batch question. The node's
// root, incarnation token and live state are resolved in the serialized
// writer transaction (any node id of the tree is addressable — the agent
// family launches on child nodes like "root.1"), then the exact-header
// idempotency insert runs into the existing session_notices outbox with
// job_tool_call_id NULL so the pull-time void rule retains the row.
func (s *BatchStore) BatchPublishQuestion(ctx context.Context, owner, node string, token int64, marker, text string) error {
	if owner == "" || node == "" || token <= 0 || marker == "" || !strings.HasSuffix(marker, "\n") {
		return errors.New("batch store: invalid question identity")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("batch store: question begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var rowToken int64
	var state string
	var rootDelivery string
	err = tx.QueryRowContext(ctx,
		`SELECT n.token, n.state, r.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.node_id = ? AND n.owner_session_id = ?`,
		node, owner).Scan(&rowToken, &state, &rootDelivery)
	if err != nil {
		return fmt.Errorf("batch store: question node lookup: %w", err)
	}
	// "starting" is the durable post-admission/pre-EventStarted state
	// (engine admission.go:90); the token fence still pins the exact
	// incarnation, and inject_agent is accepted in this state
	// (runner_requests.go refuses only terminal/cancelling).
	if rowToken != token || (state != "pending" && state != "starting" && state != "running" && state != "cancelling") {
		return errors.New("batch store: question node incarnation is not live")
	}
	// R12-EF fix F3: a root the rerun retired durably (delivery='void') owns
	// no live question channel -- refusing here keeps a still-live child of
	// the retired subtree (stop not yet delivered, or a foreign host) from
	// publishing a question the new branch could never answer.
	if rootDelivery == "void" {
		return errors.New("batch store: question root is durably retired")
	}
	var inserted int64
	err = tx.QueryRowContext(ctx, `INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
SELECT ?, ?, ?, 1, 'pending', NULL, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM session_notices WHERE owner = ? AND kind = ? AND substr(text, 1, length(?)) = ?)`,
		owner, NoticeKindChildQuestion, text, time.Now().Unix(), time.Now().Unix(),
		owner, NoticeKindChildQuestion, marker, marker).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		// Exact identity already durably published: dedup hit, commit the
		// validated read and report success so concurrent publishers agree.
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("batch store: question insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("batch store: question commit: %w", err)
	}
	return nil
}

// parseBatchQuestionMarker extracts (nodeID, token) from a child-question
// notice text whose header line is the marker BatchPublishQuestion dedupes
// on: "[batch-question node=X token=N sequence=S depth=D child=C]\n". The
// identical parser on the agent side (LiveHeldQuestions' incarnation fence)
// reads the same publisher format; the duplicate exists because session
// cannot import agent.
func parseBatchQuestionMarker(text string) (nodeID string, token int64, ok bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, false
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		name, value, isField := strings.Cut(field, "=")
		if !isField {
			continue
		}
		switch name {
		case "node":
			nodeID = value
		case "token":
			t, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, false
			}
			token = t
		}
	}
	if nodeID == "" || token == 0 {
		return "", 0, false
	}
	return nodeID, token, true
}

// batchRerunVoidChildQuestionsOfVoidRoots voids (delivery='void', wake=0)
// every pending-or-delivered child_question notice of owner whose marker
// names a node of a void-delivery root (R12-EF fix F3): the question channel
// of a subtree the rerun retired must never be delivered by the new branch.
// Runs inside BatchRerunReconcile's transaction, after
// BatchRerunVoidNodesOfVoidedRoots, so roots this very transaction retired
// are matched. Notices without a parseable marker (never written by
// BatchPublishQuestion) and questions of surviving roots are untouched; a
// second pass is a no-op (already-void rows never enter the candidate set).
func batchRerunVoidChildQuestionsOfVoidRoots(ctx context.Context, tx *sql.Tx, owner string, now int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ? AND kind = ? AND delivery IN ('pending','done')`,
		owner, NoticeKindChildQuestion)
	if err != nil {
		return fmt.Errorf("batch rerun: list child questions: %w", err)
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return fmt.Errorf("batch rerun: scan child question: %w", err)
		}
		node, token, ok := parseBatchQuestionMarker(text)
		if !ok {
			continue
		}
		var one int64
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.node_id = ? AND n.owner_session_id = ? AND n.token = ? AND r.delivery = 'void'`,
			node, owner, token).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("batch rerun: match child question root: %w", err)
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("batch rerun: iterate child questions: %w", err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = ? WHERE id = ? AND kind = ?`,
			now, id, NoticeKindChildQuestion); err != nil {
			return fmt.Errorf("batch rerun: void child question: %w", err)
		}
	}
	return nil
}
