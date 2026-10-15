package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	err = tx.QueryRowContext(ctx,
		`SELECT token, state FROM batch_nodes WHERE node_id = ? AND owner_session_id = ?`,
		node, owner).Scan(&rowToken, &state)
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
