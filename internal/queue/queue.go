// Package queue provides persistent task queue CRUD for batched rush run
// invocations. The backing store is a SQLite table (queue_tasks) created by
// migration 20260520000002.
package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TaskStatus represents the lifecycle state of a queue task.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusDone      TaskStatus = "done"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
)

// Task is a row from the queue_tasks table.
type Task struct {
	ID            string
	SessionID     string
	Prompt        string
	Role          string
	MaxCost       float64
	MaxTokens     int64
	TimeoutSec    int64
	Status        TaskStatus
	Cost          float64
	Tokens        int64
	ExitReason    string
	CreatedAt     int64
	StartedAt     sql.NullInt64
	FinishedAt    sql.NullInt64
	WorkspaceRoot string
}

// taskColumns is the explicit column list every queue_tasks read scans
// ("SELECT *" style expansion, matching internal/db/sql). workspace_root is
// part of it since WS-1 (#1142 step C): an operator listing the queue must
// see which checkout a task belongs to.
const taskColumns = `id, COALESCE(session_id,''), prompt, COALESCE(role,''), COALESCE(max_cost,0), COALESCE(max_tokens,0), COALESCE(timeout_sec,0), status, cost, tokens, COALESCE(exit_reason,''), created_at, started_at, finished_at, workspace_root`

// Service provides CRUD for the queue_tasks table.
type Service struct {
	db *sql.DB

	// workspaceRoot is the canonical checkout root of the process this
	// service belongs to ("" = home). WS-1 (#1142 step C, P1 of
	// docs/plans/2026-10-01-shared-data-dir.md): Add stamps it on every row,
	// and the claim/reclaim ownership filters below scope a runner to its own
	// workspace's rows, so one shared data dir does not hand one checkout's
	// tasks to another checkout's runner.
	workspaceRoot string
	// home is the explicit "owns its own data directory" flag
	// (config.WorkspaceHome() in production, true until SD-D #1143) -- it,
	// not the workspace root, decides whether legacy '' rows are claimable
	// here.
	home bool
}

// NewService creates a queue service backed by the given database. It is a
// home service (home=true, workspace "") -- the pre-WS-1 behaviour, kept for
// operator-facing callers.
func NewService(db *sql.DB) *Service {
	return NewServiceWithWorkspace(db, "", true)
}

// NewServiceWithWorkspace is NewService bound to the process's workspace
// (#1142 step C, WS-1/P1). home is the SEPARATE "owns its own data directory"
// flag, never derived from workspaceRoot: a git-checkout runner today is a
// home process (it must keep claiming its legacy ” rows) even though its
// root is non-empty.
func NewServiceWithWorkspace(db *sql.DB, workspaceRoot string, home bool) *Service {
	return &Service{db: db, workspaceRoot: workspaceRoot, home: home}
}

// ownsRow reports whether a queue row's workspace_root belongs to this
// service's workspace -- the same predicate as session.Owns, duplicated here
// because queue is a leaf package and must not import session: a row is ours
// when its workspace_root equals the service's root, or when it is a legacy
// unbound row (”) and the service is a home process.
func (s *Service) ownsRow(rowWorkspaceRoot string) bool {
	if rowWorkspaceRoot == s.workspaceRoot {
		return true
	}
	if rowWorkspaceRoot != "" {
		return false
	}
	return s.home
}

// ownershipFilter is the SQL form of ownsRow, for the claim/reclaim queries.
// Bound through two ? parameters (ownershipArgs): the service's
// workspace_root and its home flag.
const ownershipFilter = ` AND (workspace_root = ? OR (workspace_root = '' AND ? = 1))`

// ownershipArgs are ownershipFilter's parameters: the service's workspace
// root and its home flag (1 for a home process), mirroring session.Owns. The
// flag is the constructor value, not a function of the root.
func (s *Service) ownershipArgs() (workspaceRoot string, home int64) {
	return s.workspaceRoot, homeFlag(s.home)
}

// homeFlag is queue's local copy of the bool->SQL-int mapping (the session
// package's helper of the same name is unexported there).
func homeFlag(home bool) int64 {
	if home {
		return 1
	}
	return 0
}

// Add inserts a new pending task and returns its generated ID.
// WS-1 (#1142 step C): the row is stamped with this service's workspace_root
// so the claim/reclaim ownership filters can keep it with this checkout's
// runner.
func (s *Service) Add(ctx context.Context, sessionID, prompt, role string, maxCost float64, maxTokens, timeoutSec int64) (string, error) {
	id := fmt.Sprintf("q-%d-%s", time.Now().Unix(), uuid.New().String()[:8])
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO queue_tasks (id, session_id, prompt, role, max_cost, max_tokens, timeout_sec, status, created_at, workspace_root)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		id, sql.NullString{String: sessionID, Valid: sessionID != ""},
		prompt, sql.NullString{String: role, Valid: role != ""},
		maxCost, maxTokens, timeoutSec, now, s.workspaceRoot,
	)
	if err != nil {
		return "", fmt.Errorf("queue add: %w", err)
	}
	return id, nil
}

// List returns tasks filtered by status. Empty status means all.
//
// Deliberately NOT ownership-filtered (WS-1, #1142 step C): this is the
// operator's view of the whole queue -- every workspace's tasks, exactly like
// the web session list shows every workspace's sessions. A runner must NOT
// use it to pick work; ClaimPending is the ownership-scoped path.
func (s *Service) List(ctx context.Context, status TaskStatus) ([]Task, error) {
	query := "SELECT " + taskColumns + " FROM queue_tasks"
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, string(status))
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
			&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
			&t.Cost, &t.Tokens, &t.ExitReason,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.WorkspaceRoot,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Get returns a single task by ID. No ownership filter: an operator (or the
// web payload) asking for one task by its ID gets it, whatever workspace it
// belongs to -- same operator-view rule as List.
func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM queue_tasks WHERE id = ?",
		id,
	).Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
		&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
		&t.Cost, &t.Tokens, &t.ExitReason,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.WorkspaceRoot,
	)
	return t, err
}

// ClaimPending atomically picks up to n pending tasks and sets them to
// running. Returns the claimed tasks.
//
// WS-1 (#1142 step C, P1): the scan only sees this service's workspace's
// rows (ownershipFilter / ownsRow). A foreign workspace's pending row is not
// even read here -- it stays 'pending' with started_at NULL and its attempts
// never grow, waiting for its own checkout's runner to claim it. A home
// service ("") claims only legacy ” rows; a linked worktree's rows are none
// of its business.
func (s *Service) ClaimPending(ctx context.Context, n int) ([]Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ws, home := s.ownershipArgs()
	rows, err := tx.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM queue_tasks WHERE status = 'pending'"+ownershipFilter+" ORDER BY created_at ASC LIMIT ?", ws, home, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
			&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
			&t.Cost, &t.Tokens, &t.ExitReason,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.WorkspaceRoot,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Release the read cursor before issuing writes on the same tx (SQLite
	// dislikes an open cursor during writes); the deferred Close is then a
	// harmless no-op.
	rows.Close()

	now := time.Now().Unix()
	for _, t := range tasks {
		if _, err := tx.ExecContext(ctx,
			"UPDATE queue_tasks SET status = 'running', started_at = ? WHERE id = ?",
			now, t.ID,
		); err != nil {
			return nil, err
		}
	}
	return tasks, tx.Commit()
}

// UpdateStatus sets the task's final state and metrics.
func (s *Service) UpdateStatus(ctx context.Context, id string, status TaskStatus, cost float64, tokens int64, exitReason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		"UPDATE queue_tasks SET status = ?, cost = ?, tokens = ?, exit_reason = ?, finished_at = ? WHERE id = ?",
		string(status), cost, tokens, exitReason, now, id,
	)
	return err
}

// ReclaimRunning resets every task stuck in 'running' back to 'pending' so a
// future `queue run` can pick it up again, clearing the run-scoped fields
// (cost/tokens/exit_reason/started_at/finished_at) left over from the
// abandoned attempt.
//
// Callers MUST call this only while holding the process-exclusive queue.lock
// OS lock (see acquireSpawnLock in internal/cmd/queue.go) — winning that lock
// is authoritative proof no other `queue run` is alive, because a live
// runner keeps the OS lock held for its entire run (flock/LockFileEx
// auto-releases only on process death, mirroring internal/session/lock.go's
// SessionLock). Given that invariant, EVERY row still marked 'running' at
// this point necessarily belongs to a previous runner that died or was
// killed before it could write a final status; there is no live owner it
// could ever steal from.
//
// This deliberately does NOT use a per-task lease, heartbeat, runner ID, or
// any mtime/PID-based liveness threshold. Task #269 (see
// internal/session/lock.go's package doc) found that reclaiming a lock by
// "PID looks dead" or "mtime looks stale" can steal ownership from a holder
// that is merely busy on one long tool call (up to ~45 minutes is normal),
// producing two simultaneous owners. That failure mode requires multiple
// candidate owners racing a time/liveness guess. The queue has none: only
// one process can ever hold queue.lock at a time, so there is nothing for a
// threshold to disambiguate — the OS lock itself is the exact, zero-latency
// answer "is a previous runner still alive", with no window in which a
// healthy runner can be mistaken for a dead one.
//
// WS-1 (#1142 step C, P1): the UPDATE is ownership-filtered, so this only
// reclaims rows of THIS service's workspace. A foreign workspace's running
// row is left exactly as it is: if its runner is alive, that runner holds its
// own OS lock and will finish the row itself; if it is orphaned, it is the
// other checkout's backlog to reclaim, not this one's — the same ownership
// predicate as ClaimPending (and session.Owns).
func (s *Service) ReclaimRunning(ctx context.Context) (int64, error) {
	ws, home := s.ownershipArgs()
	res, err := s.db.ExecContext(ctx,
		`UPDATE queue_tasks
		    SET status = 'pending', cost = 0, tokens = 0, exit_reason = '',
		        started_at = NULL, finished_at = NULL
		  WHERE status = 'running'`+ownershipFilter,
		ws, home,
	)
	if err != nil {
		return 0, fmt.Errorf("reclaim running tasks: %w", err)
	}
	return res.RowsAffected()
}

// Remove deletes a task by ID (only if in a terminal state).
func (s *Service) Remove(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM queue_tasks WHERE id = ? AND status IN ('pending', 'done', 'failed', 'cancelled')",
		id,
	)
	return err
}

// Clear removes tasks matching the given statuses. Empty means all terminal states.
func (s *Service) Clear(ctx context.Context, statuses ...TaskStatus) error {
	if len(statuses) == 0 {
		statuses = []TaskStatus{StatusPending, StatusDone, StatusFailed, StatusCancelled}
	}
	ph := ""
	args := make([]any, len(statuses))
	for i, s := range statuses {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = string(s)
	}
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM queue_tasks WHERE status IN ("+ph+")",
		args...,
	)
	return err
}
