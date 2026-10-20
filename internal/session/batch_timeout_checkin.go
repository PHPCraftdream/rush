// W3: the ONE App-owned wake_only check-in timer for the batch runtime.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlitedriver "modernc.org/sqlite"
)

// DefaultBatchTimeoutCheckinPageSize bounds one scan page: the scanner is
// level-readable and makes progress every pass, so a bounded page keeps a
// single pass cheap without needing to finish the whole backlog.
const DefaultBatchTimeoutCheckinPageSize = 200

// DefaultBatchTimeoutCheckinScanBudget is the DEFAULT per-scan candidate
// budget: one pass may touch at most this many candidate rows across the
// pages it visits, so per-pass cost is bounded even when live work exceeds
// one page. Because receipted nodes drop out of the SQL candidate set,
// every pass makes progress through any backlog.
const DefaultBatchTimeoutCheckinScanBudget = 200

// DefaultBatchTimeoutCheckinInterval is the ticker period between scans.
const DefaultBatchTimeoutCheckinInterval = 10 * time.Second

// batchTimeoutCheckinStateFence is the nonterminal live set a node and its
// root must be in for a check-in to fire (batch_nodes state CHECK domain).
const batchTimeoutCheckinStateFence = "('pending','starting','running','cancelling')"

// batchTimeoutCheckinSelect is the keyset-paged candidate query. The
// candidate set is exactly the live task nodes that can still need a
// check-in: payload is valid wake_only JSON, already due (created_at +
// seconds <= now), and without a receipt for this incarnation — so after a
// committed check-in the node leaves the set and every pass makes progress
// through any backlog. Malformed or NULL payloads are excluded by the
// json_valid guard, never failing the scan. The root join (still
// nonterminal on a non-void root) feeds the root's tool_call_id so the
// committed notice pairs with the existing SessionNoticeStore void
// condition (GetLiveBatchRoot by owner+job_tool_call_id).
const batchTimeoutCheckinSelect = `SELECT n.created_at, n.node_id, n.root_id, n.owner_session_id, n.host_id, n.token, n.task_payload, r.tool_call_id
FROM batch_nodes n
JOIN batch_nodes r ON r.root_id = n.root_id AND r.node_id = n.root_id
     AND r.owner_session_id = n.owner_session_id AND r.host_id = n.host_id
WHERE n.kind = 'task'
  AND n.state IN ` + batchTimeoutCheckinStateFence + `
  AND r.state IN ` + batchTimeoutCheckinStateFence + `
  AND r.delivery != 'void'
  AND CASE WHEN json_valid(CAST(n.task_payload AS TEXT))
           THEN json_extract(CAST(n.task_payload AS TEXT), '$.timeout.kind') END = 'wake_only'
  AND CASE WHEN json_valid(CAST(n.task_payload AS TEXT))
           THEN n.created_at + CAST(json_extract(CAST(n.task_payload AS TEXT), '$.timeout.seconds') AS INTEGER) END <= ?
  AND NOT EXISTS (SELECT 1 FROM batch_timeout_checkins c
                  WHERE c.root_id = n.root_id AND c.node_id = n.node_id
                    AND c.host_id = n.host_id AND c.token = n.token
                    AND c.kind = '` + NoticeKindWakeOnly + `')
  AND (n.created_at, n.node_id) > (?, ?)
ORDER BY n.created_at, n.node_id
LIMIT ?`

// BatchTimeoutCheckin is the ONE App-owned wake_only check-in timer for the
// batch runtime (W3). It scans nonterminal batch task nodes whose durable
// payload carries an overdue wake_only timeout (batchLeafTimeoutSpec JSON
// in task_payload — cross-package contract with internal/agent's
// batchLeafPayload), and for each writes, in ONE transaction fenced on
// owner/root/node/token, nonterminal node state, live non-void root, and a
// one-shot receipt row (batch_timeout_checkins, keyed by root/node/host/
// token/kind): a retained session_notices row (kind 'timeout_wake_only',
// wake=1, job_tool_call_id = the root's tool_call_id so the existing
// SessionNoticeStore void condition drops it once the root is no longer
// live). It NEVER chooses a terminal state and NEVER stops work; after the
// notice commits it calls the injectable wake hook (W2 wires the
// coordinator wake). A crash between commit and wake is safe: retained
// notice debt is level-readable. The absolute due time is
// node.created_at + payload seconds: created_at is the durable call-start
// anchor (set once at reservation, never updated), so restarting the
// service resets neither the due time nor the one-shot receipt.
type BatchTimeoutCheckin struct {
	sqlDB *sql.DB

	mu         sync.Mutex
	now        func() time.Time
	wake       func(ctx context.Context, ownerSessionID string)
	pageSize   int
	scanBudget int

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	// started records whether Start actually launched the goroutine, so
	// Stop can skip its join (and never block) when Start never ran — the
	// app runtime may Stop before ever Starting.
	started atomic.Bool
}

// NewBatchTimeoutCheckin builds a scanner on the given *sql.DB with the
// production clock, no wake hook, and the default page size. Construction
// has no side effects; Start arms the ticker.
func NewBatchTimeoutCheckin(sqlDB *sql.DB) *BatchTimeoutCheckin {
	return &BatchTimeoutCheckin{
		sqlDB:      sqlDB,
		now:        time.Now,
		pageSize:   DefaultBatchTimeoutCheckinPageSize,
		scanBudget: DefaultBatchTimeoutCheckinScanBudget,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// SetClock injects the clock (tests). Call before Start.
func (s *BatchTimeoutCheckin) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// SetWakeFunc installs the wake hook called AFTER each notice commits. Nil
// (the default) skips waking; the committed retained notice is still
// level-readable debt.
func (s *BatchTimeoutCheckin) SetWakeFunc(wake func(ctx context.Context, ownerSessionID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wake = wake
}

// Start arms the ticker goroutine. Idempotent; safe to call before or after
// Stop (a post-Stop Start does not resurrect the goroutine — the scanner
// exits on the closed stop channel).
func (s *BatchTimeoutCheckin) Start() {
	s.startOnce.Do(func() {
		// Stored BEFORE the goroutine exists, so Stop's later Load observes
		// the launch decision without racing.
		s.started.Store(true)
		go func() {
			defer close(s.done)
			ticker := time.NewTicker(DefaultBatchTimeoutCheckinInterval)
			defer ticker.Stop()
			for {
				select {
				case <-s.stop:
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					// Per-pass errors are logged, never fatal: the next
					// tick rescans.
					if _, err := s.ScanOnce(ctx); err != nil {
						slog.Error("Batch timeout check-in scan failed", "err", err)
					}
					cancel()
				}
			}
		}()
	})
}

// Stop disarms the scanner and waits for the goroutine to finish. It is
// idempotent, and the wait is SKIPPED when Start never launched the
// goroutine (Stop before Start must not block forever).
func (s *BatchTimeoutCheckin) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.started.Load() {
			<-s.done
		}
	})
}

// batchTimeoutCheckinRow is one candidate row of the keyset scan.
type batchTimeoutCheckinRow struct {
	CreatedAt      int64
	NodeID         string
	RootID         string
	OwnerSessionID string
	HostID         string
	Token          int64
	Payload        []byte
	RootToolCallID sql.NullString
}

// batchTimeoutCheckinPayload is the minimal task_payload shape the scanner
// reads (the agent package's batchLeafPayload, cross-package contract).
type batchTimeoutCheckinPayload struct {
	Tool    string `json:"tool"`
	Timeout *struct {
		Kind    string `json:"kind"`
		Seconds int    `json:"seconds"`
	} `json:"timeout"`
}

// ScanOnce pages every nonterminal candidate once and commits a check-in
// (receipt + retained notice) for each DUE one. It returns the number of
// check-ins committed. Only context/page errors abort with an error; a
// single row's failure is logged and skipped (it retries next scan).
// Each page's rows are fully drained and the iterator closed BEFORE any
// write transaction opens (the db writer is a single pooled connection,
// so querying while a transaction waits on the same connection would
// deadlock). The candidate budget is per scan, not per page: one pass processes at
// most s.scanBudget candidate rows across all pages it visits. Because
// receipted nodes drop out of the SQL candidate set, the scan is
// genuinely level-readable: the next pass resumes exactly where the
// backlog stands (a committed check-in removes the node from the set).
func (s *BatchTimeoutCheckin) ScanOnce(ctx context.Context) (int, error) {
	s.mu.Lock()
	now, pageSize := s.now(), s.pageSize
	budget := s.scanBudget
	s.mu.Unlock()
	var cursorTime int64
	var cursorNode string
	committed := 0
	for {
		// The page is fully drained and its iterator closed BEFORE any write
		// transaction: checkinRow opens a BeginTx on the same single pooled
		// connection a live iterator would hold, so checking in inside
		// rows.Next() would deadlock.
		page, err := s.readCandidatePage(ctx, now.Unix(), cursorTime, cursorNode, pageSize, budget)
		if err != nil {
			return committed, fmt.Errorf("batch timeout check-in scan: %w", err)
		}
		// Keyset cursor advances past every row read, skipped or not.
		if n := len(page); n > 0 {
			cursorTime, cursorNode = page[n-1].CreatedAt, page[n-1].NodeID
		}
		for _, row := range page {
			ok, err := s.checkinRow(ctx, now, row)
			if err != nil {
				// One row's failure never aborts the scan: it retries on
				// the next pass.
				slog.Error("Batch timeout check-in failed for node; skipping", "root_id", row.RootID, "node_id", row.NodeID, "err", err)
				continue
			}
			if ok {
				committed++
			}
		}
		budget -= len(page)
		if len(page) < pageSize || budget <= 0 {
			return committed, nil
		}
	}
}

// readCandidatePage reads one keyset page of at most limit candidate rows
// and closes the iterator before returning (see ScanOnce).
func (s *BatchTimeoutCheckin) readCandidatePage(ctx context.Context, nowUnix, cursorTime int64, cursorNode string, pageSize, limit int) ([]batchTimeoutCheckinRow, error) {
	rows, err := s.sqlDB.QueryContext(ctx, batchTimeoutCheckinSelect, nowUnix, cursorTime, cursorNode, pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]batchTimeoutCheckinRow, 0, pageSize)
	for rows.Next() && len(page) < limit {
		var row batchTimeoutCheckinRow
		if err := rows.Scan(&row.CreatedAt, &row.NodeID, &row.RootID, &row.OwnerSessionID, &row.HostID, &row.Token, &row.Payload, &row.RootToolCallID); err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// checkinRow handles one candidate: parse, due check, then the fenced
// one-shot transaction. Returns whether a check-in was committed.
func (s *BatchTimeoutCheckin) checkinRow(ctx context.Context, now time.Time, row batchTimeoutCheckinRow) (bool, error) {
	var p batchTimeoutCheckinPayload
	if len(row.Payload) == 0 {
		// Most nonterminal task nodes carry no timeout payload at all: the
		// common skip is silent, not an error.
		return false, nil
	}
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		// Unparseable payload is not a check-in candidate; log and move on
		// rather than aborting the scan for one bad row.
		slog.Error("Batch timeout check-in: unparseable task payload; skipping", "root_id", row.RootID, "node_id", row.NodeID, "err", err)
		return false, nil
	}
	if p.Timeout == nil || p.Timeout.Kind != "wake_only" ||
		row.CreatedAt+int64(p.Timeout.Seconds) > now.Unix() {
		return false, nil
	}
	if !row.RootToolCallID.Valid || row.RootToolCallID.String == "" {
		return false, fmt.Errorf("live root %s has no tool_call_id", row.RootID)
	}
	text := fmt.Sprintf("Timeout reached for batch task %s (%s) — it is still running (requested %ds). This was a one-time check-in; it will not repeat automatically. Decide whether to keep waiting, check again later, or stop it with batch_stop.",
		row.RootID, p.Tool, p.Timeout.Seconds)

	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only after commit; commit below decides the outcome

	// Fence the node: same incarnation (token) and still nonterminal.
	var state string
	var token int64
	err = tx.QueryRowContext(ctx,
		`SELECT state, token FROM batch_nodes WHERE node_id=? AND root_id=? AND owner_session_id=? AND host_id=?`,
		row.NodeID, row.RootID, row.OwnerSessionID, row.HostID).Scan(&state, &token)
	if err != nil {
		return false, err
	}
	if token != row.Token || !batchTimeoutCheckinStateLive(state) {
		return false, nil
	}
	// Fence the root: still nonterminal, not void, and carrying the
	// tool_call_id the notice's void condition needs.
	var rootState, rootDelivery string
	var rootToolCallID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT state, delivery, tool_call_id FROM batch_nodes WHERE node_id=root_id AND root_id=? AND owner_session_id=? AND host_id=?`,
		row.RootID, row.OwnerSessionID, row.HostID).Scan(&rootState, &rootDelivery, &rootToolCallID)
	if err != nil {
		return false, err
	}
	if !batchTimeoutCheckinStateLive(rootState) || rootDelivery == "void" {
		return false, nil
	}
	if !rootToolCallID.Valid || rootToolCallID.String == "" {
		return false, fmt.Errorf("live root %s has no tool_call_id", row.RootID)
	}
	// One-shot receipt, committed atomically with the notice: a UNIQUE
	// violation here means this exact incarnation already checked in.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO batch_timeout_checkins (root_id, node_id, host_id, token, kind, owner_session_id, created_at) VALUES (?,?,?,?,?,?,?)`,
		row.RootID, row.NodeID, row.HostID, row.Token, NoticeKindWakeOnly, row.OwnerSessionID, now.Unix()); err != nil {
		if isSQLiteUniqueConstraint(err) {
			return false, nil
		}
		return false, err
	}
	// The retained notice: wake=1, delivery=pending, job_tool_call_id set
	// so SessionNoticeStore's void condition drops it once the root is no
	// longer live. NEVER a terminal state on the node: the scanner reports,
	// it does not stop work.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at) VALUES (?,?,?,1,'pending',?,?,?)`,
		row.OwnerSessionID, NoticeKindWakeOnly, text, rootToolCallID.String, now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// Wake AFTER the commit, with a context independent of the scan: a scan
	// cancellation must not skip wakes of already-committed notices, and a
	// wake failure must not fail the scan (the notice is retained debt).
	s.mu.Lock()
	wake := s.wake
	s.mu.Unlock()
	if wake != nil {
		wakeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		wake(wakeCtx, row.OwnerSessionID)
	}
	return true, nil
}

// batchTimeoutCheckinStateLive reports whether a batch_nodes state is in
// the nonterminal live set.
func batchTimeoutCheckinStateLive(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// isSQLiteUniqueConstraint reports whether err is a UNIQUE constraint
// violation (the one-shot receipt's duplicate signal). Classification is
// by the SQLite result code (SQLITE_CONSTRAINT, masked to its primary 8
// bits), with a textual fallback for drivers without a typed error; the
// receipt insert's only constraint failure shape is the PRIMARY KEY
// duplicate, so CONSTRAINT here IS the already-checked-in signal.
func isSQLiteUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlitedriver.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19 {
		return strings.Contains(err.Error(), "UNIQUE constraint")
	}
	return strings.Contains(err.Error(), "UNIQUE constraint")
}
