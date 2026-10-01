// Hand-written run-cost queries (#1130). sqlc's query editor cannot compile
// a two-column recursive CTE (the ancestor must be carried through the
// recursive member), so these four read queries — subtree-activity ordering
// and the subtree-budget stats aggregates — live here, against the same
// schema the migrations define. Everything else stays sqlc-generated.

package db

import (
	"context"
	"database/sql"
)

// dbtx resolves the transaction the Queries instance is bound to (WithTx)
// or the plain pool, exactly like the generated queryRow/query helpers do.
func (q *Queries) dbtx() DBTX {
	if q.tx != nil {
		return q.tx
	}
	return q.db
}

const getLastSessionSubtree = `
WITH RECURSIVE sub(node, anc) AS (
    SELECT s.id, s.id FROM sessions s WHERE s.parent_session_id IS NULL
    UNION
    SELECT c.id, anc FROM sessions c JOIN sub ON c.cost_parent_id = sub.node
),
activity(anc, active) AS (
    SELECT sub.anc, MAX(n.updated_at) FROM sub JOIN sessions n ON n.id = sub.node GROUP BY sub.anc
)
SELECT s.id, s.parent_session_id, s.title, s.message_count, s.prompt_tokens, s.completion_tokens, s.cost, s.updated_at, s.created_at, s.summary_message_id, s.todos, s.smart_model_provider, s.smart_model_id, s.fast_model_provider, s.fast_model_id, s.system_prompt, s.yolo_enabled, s.smart_model_reasoning_effort, s.fast_model_reasoning_effort, s.cancel_requested, s.ended_reason, s.budget_max_cost, s.budget_max_tokens, s.budget_timeout_sec, s.deleted_todos, s.parent_cost_accounted, s.worker_model_provider, s.worker_model_id, s.worker_model_reasoning_effort, s.reviewer_model_provider, s.reviewer_model_id, s.reviewer_model_reasoning_effort, s.origin, s.cost_self, s.cost_base, s.cost_parent_id
FROM sessions s JOIN activity a ON a.anc = s.id
ORDER BY a.active DESC
LIMIT 1`

// GetLastSession returns the most recently ACTIVE top-level session
// (`rush run --continue`): activity is max(updated_at) over the root's
// delegation subtree, so a busy child keeps its root the continuation
// target without ever writing the parent's updated_at.
func (q *Queries) GetLastSession(ctx context.Context) (Session, error) {
	row := q.dbtx().QueryRowContext(ctx, getLastSessionSubtree)
	var i Session
	err := row.Scan(
		&i.ID,
		&i.ParentSessionID,
		&i.Title,
		&i.MessageCount,
		&i.PromptTokens,
		&i.CompletionTokens,
		&i.Cost,
		&i.UpdatedAt,
		&i.CreatedAt,
		&i.SummaryMessageID,
		&i.Todos,
		&i.SmartModelProvider,
		&i.SmartModelID,
		&i.FastModelProvider,
		&i.FastModelID,
		&i.SystemPrompt,
		&i.YoloEnabled,
		&i.SmartModelReasoningEffort,
		&i.FastModelReasoningEffort,
		&i.CancelRequested,
		&i.EndedReason,
		&i.BudgetMaxCost,
		&i.BudgetMaxTokens,
		&i.BudgetTimeoutSec,
		&i.DeletedTodos,
		&i.ParentCostAccounted,
		&i.WorkerModelProvider,
		&i.WorkerModelID,
		&i.WorkerModelReasoningEffort,
		&i.ReviewerModelProvider,
		&i.ReviewerModelID,
		&i.ReviewerModelReasoningEffort,
		&i.Origin,
		&i.CostSelf,
		&i.CostBase,
		&i.CostParentID,
	)
	return i, err
}

// The subtree-budget CTE shared by the stats aggregates: per ROOT, the
// budget = max(subtree spent - cost_base, 0); the UNION terminates a
// corrupted cost_parent_id cycle.
const rootBudgetCTE = `
WITH RECURSIVE sub(node, anc) AS (
    SELECT s.id, s.id FROM sessions s WHERE s.parent_session_id IS NULL
    UNION
    SELECT c.id, anc FROM sessions c JOIN sub ON c.cost_parent_id = sub.node
),
spend(anc, spent) AS (
    SELECT sub.anc, SUM(n.cost_self) FROM sub JOIN sessions n ON n.id = sub.node GROUP BY sub.anc
),
root_budget(anc, budget) AS (
    SELECT s.id, MAX(spend.spent - s.cost_base, 0) FROM sessions s JOIN spend ON spend.anc = s.id GROUP BY s.id
)`

type GetUsageByDayRow struct {
	Day              interface{}     `json:"day"`
	PromptTokens     sql.NullFloat64 `json:"prompt_tokens"`
	CompletionTokens sql.NullFloat64 `json:"completion_tokens"`
	Cost             sql.NullFloat64 `json:"cost"`
	SessionCount     int64           `json:"session_count"`
}

const getUsageByDaySubtree = rootBudgetCTE + `
SELECT
    date(s.created_at, 'unixepoch') as day,
    SUM(s.prompt_tokens) as prompt_tokens,
    SUM(s.completion_tokens) as completion_tokens,
    SUM(COALESCE(rb.budget, 0)) as cost,
    COUNT(*) as session_count
FROM sessions s LEFT JOIN root_budget rb ON rb.anc = s.id
WHERE s.parent_session_id IS NULL
GROUP BY date(s.created_at, 'unixepoch')
ORDER BY day DESC`

// GetUsageByDay — per-day usage with the cost summed over each root's
// SUBTREE budget (#1130); tokens stay the roots' own snapshot counters.
func (q *Queries) GetUsageByDay(ctx context.Context) ([]GetUsageByDayRow, error) {
	rows, err := q.dbtx().QueryContext(ctx, getUsageByDaySubtree)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []GetUsageByDayRow{}
	for rows.Next() {
		var i GetUsageByDayRow
		if err := rows.Scan(
			&i.Day,
			&i.PromptTokens,
			&i.CompletionTokens,
			&i.Cost,
			&i.SessionCount,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

type GetTotalStatsRow struct {
	TotalSessions         int64       `json:"total_sessions"`
	TotalPromptTokens     interface{} `json:"total_prompt_tokens"`
	TotalCompletionTokens interface{} `json:"total_completion_tokens"`
	TotalCost             interface{} `json:"total_cost"`
	TotalMessages         interface{} `json:"total_messages"`
	AvgTokensPerSession   interface{} `json:"avg_tokens_per_session"`
	AvgMessagesPerSession interface{} `json:"avg_messages_per_session"`
}

const getTotalStatsSubtree = rootBudgetCTE + `
SELECT
    COUNT(*) as total_sessions,
    COALESCE(SUM(s.prompt_tokens), 0) as total_prompt_tokens,
    COALESCE(SUM(s.completion_tokens), 0) as total_completion_tokens,
    COALESCE(SUM(COALESCE(rb.budget, 0)), 0) as total_cost,
    COALESCE(SUM(s.message_count), 0) as total_messages,
    COALESCE(AVG(s.prompt_tokens + s.completion_tokens), 0) as avg_tokens_per_session,
    COALESCE(AVG(s.message_count), 0) as avg_messages_per_session
FROM sessions s LEFT JOIN root_budget rb ON rb.anc = s.id
WHERE s.parent_session_id IS NULL`

// GetTotalStats — totals with the cost summed over each root's SUBTREE
// budget (#1130).
func (q *Queries) GetTotalStats(ctx context.Context) (GetTotalStatsRow, error) {
	row := q.dbtx().QueryRowContext(ctx, getTotalStatsSubtree)
	var i GetTotalStatsRow
	err := row.Scan(
		&i.TotalSessions,
		&i.TotalPromptTokens,
		&i.TotalCompletionTokens,
		&i.TotalCost,
		&i.TotalMessages,
		&i.AvgTokensPerSession,
		&i.AvgMessagesPerSession,
	)
	return i, err
}

type GetRecentActivityRow struct {
	Day          interface{}     `json:"day"`
	SessionCount int64           `json:"session_count"`
	TotalTokens  sql.NullFloat64 `json:"total_tokens"`
	Cost         sql.NullFloat64 `json:"cost"`
}

const getRecentActivitySubtree = rootBudgetCTE + `
SELECT
    date(s.created_at, 'unixepoch') as day,
    COUNT(*) as session_count,
    SUM(s.prompt_tokens + s.completion_tokens) as total_tokens,
    SUM(COALESCE(rb.budget, 0)) as cost
FROM sessions s LEFT JOIN root_budget rb ON rb.anc = s.id
WHERE s.parent_session_id IS NULL
  AND s.created_at >= strftime('%s', 'now', '-30 days')
GROUP BY date(s.created_at, 'unixepoch')
ORDER BY day ASC`

// GetRecentActivity — 30-day activity with subtree-budget cost (#1130).
func (q *Queries) GetRecentActivity(ctx context.Context) ([]GetRecentActivityRow, error) {
	rows, err := q.dbtx().QueryContext(ctx, getRecentActivitySubtree)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []GetRecentActivityRow{}
	for rows.Next() {
		var i GetRecentActivityRow
		if err := rows.Scan(
			&i.Day,
			&i.SessionCount,
			&i.TotalTokens,
			&i.Cost,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
