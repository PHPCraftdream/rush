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

// parseBatchQuestionMarkerFull extracts (node, token, sequence, child) from
// a published question marker header; ok=false when the header is absent,
// unparseable, or any of the identity fields is zero/empty (the publisher
// never writes those). Mirrors the agent-side parsers; the duplicate
// exists because session cannot import agent.
func parseBatchQuestionMarkerFull(text string) (node string, token, sequence int64, child string, ok bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, 0, "", false
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
			node = value
		case "child":
			child = value
		case "token":
			t, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, 0, "", false
			}
			token = t
		case "sequence":
			sq, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, 0, "", false
			}
			sequence = sq
		}
	}
	if node == "" || child == "" || token <= 0 || sequence <= 0 {
		return "", 0, 0, "", false
	}
	return node, token, sequence, child, true
}

// BatchResolveChildQuestions durably closes (delivery='void', wake=0) every
// pending-or-delivered child_question notice of owner whose marker names
// EXACTLY this incarnation (node, token, child) with a sequence in
// [1, sequence]: the notice must never outlive the hold it advertises. Rows
// of a NEWER sequence — a question asked after this resolution began — are
// untouched; rows naming a different child are not ours to close. Returns
// the closed count; an unknown identity is a no-op (0, nil), never an
// error. Runs in one serialized writer transaction like
// BatchPublishQuestion (the writer pool has exactly ONE connection).
func (s *BatchStore) BatchResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) (int64, error) {
	if owner == "" || node == "" || child == "" || token <= 0 || sequence <= 0 {
		return 0, nil
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("batch store: question resolve begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ? AND kind = ? AND delivery IN ('pending','done') AND wake = 1 ORDER BY id`,
		owner, NoticeKindChildQuestion)
	if err != nil {
		return 0, fmt.Errorf("batch store: question resolve list: %w", err)
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return 0, fmt.Errorf("batch store: question resolve scan: %w", err)
		}
		mnode, mtoken, msequence, mchild, ok := parseBatchQuestionMarkerFull(text)
		if !ok || mnode != node || mtoken != token || msequence > sequence || mchild != child {
			continue
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("batch store: question resolve iterate: %w", err)
	}
	// Fully drained before any write: the writer pool has exactly ONE
	// connection, so the UPDATE cannot start while rows are open.
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("batch store: question resolve close: %w", err)
	}
	var closed int64
	if len(stale) > 0 {
		placeholders := make([]string, len(stale))
		args := make([]any, 0, len(stale)+3)
		args = append(args, time.Now().Unix(), NoticeKindChildQuestion)
		for i, id := range stale {
			placeholders[i] = fmt.Sprintf("?%d", i+3)
			args = append(args, id)
		}
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = ?1 WHERE kind = ?2 AND id IN (%s)`,
			strings.Join(placeholders, ", ")), args...)
		if err != nil {
			return 0, fmt.Errorf("batch store: question resolve update: %w", err)
		}
		if closed, err = res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("batch store: question resolve affected: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("batch store: question resolve commit: %w", err)
	}
	return closed, nil
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
