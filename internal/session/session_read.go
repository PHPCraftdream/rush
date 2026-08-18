// Standalone read paths served through the read-only connection: Get,
// GetLast, List, ListAll, sub-session listing, and call-tree activity
// queries. These publish no pubsub events and need no
// read-your-own-write consistency with a later write.

package session

import (
	"context"
	"database/sql"
	"errors"

	"github.com/charmbracelet/crush/internal/db"
)

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.qRead.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.qRead.GetLastSession(ctx)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

// ListSubSessions implementation: thin wrapper around the sqlc-
// generated query. Returns an empty slice when no sub-sessions exist.
func (s *service) ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error) {
	dbSessions, err := s.qRead.ListSubSessions(ctx, sql.NullString{
		String: parentSessionID,
		Valid:  parentSessionID != "",
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// GetCallTreeActivity implementation: see the Service interface doc. A
// sql.ErrNoRows result (no messages anywhere in the tree) is reported as
// (zero-value, false, nil) rather than propagated as an error — an empty
// tree is a normal, expected state (e.g. a session that was just created),
// not a failure.
func (s *service) GetCallTreeActivity(ctx context.Context, rootID string) (CallTreeActivity, bool, error) {
	row, err := s.qRead.GetCallTreeActivity(ctx, rootID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CallTreeActivity{}, false, nil
		}
		return CallTreeActivity{}, false, err
	}
	return CallTreeActivity{
		SessionID:  row.SessionID,
		Role:       row.Role,
		LatestUnix: row.LatestUnix,
	}, true, nil
}

// callTreeActivityBatchChunkSize caps how many root IDs are passed to the
// underlying sqlc-generated GetCallTreeActivityBatch in a single query. The
// generated query expands rootIDs via sqlc.slice into one SQL parameter per
// id, so an unbounded list would eventually hit SQLite's
// SQLITE_MAX_VARIABLE_NUMBER ceiling (999 on older builds). 500 stays well
// below that with headroom for the query's other bound parameters, and keeps
// each recursive-CTE fan-out bounded to a reasonable batch. Because every
// root's tree is independent (the CTE partitions by root_session_id),
// splitting roots across chunks and merging the per-root maps is exact.
const callTreeActivityBatchChunkSize = 500

// GetCallTreeActivityBatch implementation: see the Service interface doc.
// rootIDs are split into callTreeActivityBatchChunkSize-sized chunks, each run
// as a separate underlying query, and the per-root results merged into one map.
func (s *service) GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]CallTreeActivity, error) {
	out := make(map[string]CallTreeActivity, len(rootIDs))
	if len(rootIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(rootIDs); start += callTreeActivityBatchChunkSize {
		end := start + callTreeActivityBatchChunkSize
		if end > len(rootIDs) {
			end = len(rootIDs)
		}
		rows, err := s.qRead.GetCallTreeActivityBatch(ctx, rootIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.RootSessionID] = CallTreeActivity{
				SessionID:  row.SessionID,
				Role:       row.Role,
				LatestUnix: row.LatestUnix,
			}
		}
	}
	return out, nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.qRead.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// Fork merge note (origin/main 2736e487 "fix(ui): mark estimated context
// usage" + 9595d1f0 "fix(session): preserve estimated usage marker"):
// upstream added applyEstimatedUsageState / setEstimatedUsageState /
// clearEstimatedUsageState as backend infrastructure for their TUI's
// "estimated context usage" marker. Rejected — the whole feature drives
// a TUI widget we do not ship; our WebUI handles usage display via the
// WebSocket events stream (internal/server/events.go) without per-session
// estimated-state tracking. See CHANGELOG.fork.md Section 2.
func (s *service) ListAll(ctx context.Context) ([]Session, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, parent_session_id, title, message_count,
		prompt_tokens, completion_tokens, cost, updated_at, created_at,
		summary_message_id, todos,
		large_model_provider, large_model_id,
		small_model_provider, small_model_id,
		system_prompt, yolo_enabled,
		large_model_reasoning_effort, small_model_reasoning_effort,
		worker_model_provider, worker_model_id, worker_model_reasoning_effort,
		reviewer_model_provider, reviewer_model_id, reviewer_model_reasoning_effort,
		cancel_requested,
		COALESCE(ended_reason, ''), COALESCE(budget_max_cost, 0),
		COALESCE(budget_max_tokens, 0), COALESCE(budget_timeout_sec, 0)
		FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var item db.Session
		var cancelRequested int64
		var endedReason string
		var budgetMaxCost float64
		var budgetMaxTokens, budgetTimeoutSec int64
		if err := rows.Scan(
			&item.ID, &item.ParentSessionID, &item.Title, &item.MessageCount,
			&item.PromptTokens, &item.CompletionTokens, &item.Cost,
			&item.UpdatedAt, &item.CreatedAt, &item.SummaryMessageID, &item.Todos,
			&item.LargeModelProvider, &item.LargeModelID,
			&item.SmallModelProvider, &item.SmallModelID,
			&item.SystemPrompt, &item.YoloEnabled,
			&item.LargeModelReasoningEffort, &item.SmallModelReasoningEffort,
			&item.WorkerModelProvider, &item.WorkerModelID, &item.WorkerModelReasoningEffort,
			&item.ReviewerModelProvider, &item.ReviewerModelID, &item.ReviewerModelReasoningEffort,
			&cancelRequested,
			&endedReason, &budgetMaxCost, &budgetMaxTokens, &budgetTimeoutSec,
		); err != nil {
			return nil, err
		}
		sess := s.fromDBItem(item)
		sess.CancelRequested = cancelRequested != 0
		sess.EndedReason = endedReason
		sess.BudgetMaxCost = budgetMaxCost
		sess.BudgetMaxTokens = budgetMaxTokens
		sess.BudgetTimeoutSec = budgetTimeoutSec
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}
