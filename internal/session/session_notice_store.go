// The ON-runtime session-notice machinery (package R12-D chunk 2): the
// session-notice half of notice_pull.go (pull, void condition, insert,
// list, pending-inclusive debt) re-implemented WITHOUT any async_jobs
// query. Since the H4 cutover the legacy AsyncJobStore is deleted and this
// store is the only session-notice machinery. Selection happens by
// CONSTRUCTION, never by reading RUSH_BATCH_RUNTIME: the store takes its
// bound-notice check, live-roots read, host id and host-liveness predicate
// as injected function fields; NewSessionNoticeStore wires the batch
// implementations (batch_nodes root rows, live-state set
// pending/starting/running/cancelling -- the same set GetLiveBatchRoot and
// ListLiveBatchRootsForOwner encode).
//
// Behavior mirrors the legacy session half exactly: same per-row
// transaction, same CAS, same void/keep decisions, same error propagation
// (a pull error logs and skips; the row stays pending), same publish-after-
// commit rule. The one intentional semantic change is the void condition's
// FACT SOURCE, not its shape: where the legacy condition reads the
// async_jobs row (state != "running" || delivery == "void"), this store
// reads the batch root row and voids when no LIVE root row exists or its
// delivery is 'void'.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// SessionNoticeStore is the job-table-independent session-notice pull/
// resolve/debt machinery. All async_jobs couplings are replaced by
// injected function fields, chosen by whoever constructs the store:
//
//   - boundNoticeGone: the void check for a notice bound to a work item by
//     job_tool_call_id. Batch implementation (default): the root row lookup
//     by (owner_session_id, tool_call_id) on batch_nodes via
//     GetLiveBatchRoot; gone/void when no row exists OR the row's state is
//     not in the live set (the query's own filter) OR delivery='void'.
//   - liveRootsForOwner: the supervision arm's read. Batch implementation
//     (default): ListLiveBatchRootsForOwner (same live-state set).
//   - hostID / hostNotDead: the caller's own host id and liveness
//     predicate, captured before any transaction opens. Defaults: empty id
//     and "equal ids are alive" (fail-open keeps the notice only for the own
//     host); production wiring passes HostStore-backed implementations via
//     SetHostIDFunc/SetHostNotDeadFunc.
type SessionNoticeStore struct {
	sqlDB   *sql.DB
	q       *db.Queries
	readQ   atomic.Pointer[db.Queries]
	hostIDF func() string

	boundNoticeGone   func(ctx context.Context, q *db.Queries, ownerSessionID, toolCallID string) (bool, error)
	liveRootsForOwner func(ctx context.Context, q *db.Queries, ownerSessionID string) ([]db.BatchNode, error)
	hostNotDeadF      func(ownHostID, hostID string) bool
}

// NewSessionNoticeStore builds the store over sqlDB with the batch
// implementations of boundNoticeGone and liveRootsForOwner. SetReadConn
// and the host setters refine it after construction.
func NewSessionNoticeStore(sqlDB *sql.DB) *SessionNoticeStore {
	s := &SessionNoticeStore{
		sqlDB:   sqlDB,
		q:       db.New(sqlDB),
		hostIDF: func() string { return "" },
		hostNotDeadF: func(ownHostID, hostID string) bool {
			return hostID != "" && hostID == ownHostID
		},
	}
	s.boundNoticeGone = batchBoundNoticeGone
	s.liveRootsForOwner = func(ctx context.Context, q *db.Queries, ownerSessionID string) ([]db.BatchNode, error) {
		return q.ListLiveBatchRootsForOwner(ctx, ownerSessionID)
	}
	return s
}

// batchBoundNoticeGone is the batch implementation of the bound-notice
// void check: the root row for (ownerSessionID, toolCallID) must exist AND
// be live (GetLiveBatchRoot's state IN pending/starting/running/cancelling
// filter makes a terminal or missing row sql.ErrNoRows) AND not be
// delivery='void'. GetLiveBatchRoot is the exact query batch_store.go's
// own reserve path uses, so the notice's notion of "the work is gone"
// cannot drift from the engine's.
func batchBoundNoticeGone(ctx context.Context, q *db.Queries, ownerSessionID, toolCallID string) (bool, error) {
	root, err := q.GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{
		OwnerSessionID: ownerSessionID,
		ToolCallID:     sql.NullString{String: toolCallID, Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		// Missing root or non-live (terminal) root: both are "gone", exactly
		// like the legacy condition's GetAsyncJob ErrNoRows and
		// state != "running" arms respectively.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return root.Delivery == "void", nil
}

// SetReadConn wires the read pool for read-only readers (PendingChildQuestions),
// without it, the writer is used.
func (s *SessionNoticeStore) SetReadConn(q *db.Queries) { s.readQ.Store(q) }

// SetHostIDFunc wires the own-host-id accessor (captured before any
// transaction opens).
func (s *SessionNoticeStore) SetHostIDFunc(f func() string) {
	if f != nil {
		s.hostIDF = f
	}
}

// SetHostNotDeadFunc wires the host-liveness predicate (the HostNotDead
// rule, doc sec.3.5/3.6, with the caller's own host id supplied).
func (s *SessionNoticeStore) SetHostNotDeadFunc(f func(ownHostID, hostID string) bool) {
	if f != nil {
		s.hostNotDeadF = f
	}
}

// HostID returns the wired own-host id (or "" before wiring).
func (s *SessionNoticeStore) HostID() string { return s.hostIDF() }

func (s *SessionNoticeStore) readQuerier() *db.Queries {
	if q := s.readQ.Load(); q != nil {
		return q
	}
	return s.q
}

// PullSessionNotices pulls every pending session_notices row for owner
// (oldest first), one transaction per row; a row whose void condition fails
// is marked void in the same transaction and produces no message. A per-row
// error is logged and skipped -- the row stays pending, never failing the
// whole pull.
func (s *SessionNoticeStore) PullSessionNotices(ctx context.Context, messages message.Service, owner string, build func(SessionNoticeRow) message.CreateMessageParams) ([]PulledNotice, error) {
	// Captured before any transaction opens (R2A-2): nothing a writer
	// transaction runs may wait on store state.
	ownHostID := s.HostID()
	rows, err := s.q.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []PulledNotice
	for _, row := range rows {
		pulled, ok, err := s.pullOneSessionNotice(ctx, messages, owner, ownHostID, row, build)
		if err != nil {
			slog.Warn("session notice store: pull session notice failed; row stays pending",
				"session_id", owner, "notice_id", row.ID, "err", err)
			continue
		}
		if ok {
			out = append(out, pulled)
		}
	}
	return out, nil
}

func (s *SessionNoticeStore) pullOneSessionNotice(ctx context.Context, messages message.Service, owner, ownHostID string, row db.SessionNotice, build func(SessionNoticeRow) message.CreateMessageParams) (PulledNotice, bool, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return PulledNotice{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := db.New(tx)

	pulledRow, err := q.PullPendingSessionNotice(ctx, db.PullPendingSessionNoticeParams{
		UpdatedAt: time.Now().Unix(), ID: row.ID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PulledNotice{}, false, nil // another leader already won this row
	}
	if err != nil {
		return PulledNotice{}, false, err
	}

	void, err := s.sessionNoticeVoidCondition(ctx, q, owner, ownHostID, pulledRow)
	if err != nil {
		return PulledNotice{}, false, err
	}
	if void {
		// Same downgrade-with-guard as the legacy pull (notice_pull.go):
		// delivery 'done' -> 'void' inside our own transaction; 0 rows is
		// DB corruption, surfaced as a hard error.
		rows, err := q.VoidPendingSessionNotice(ctx, db.VoidPendingSessionNoticeParams{
			UpdatedAt: time.Now().Unix(), ID: pulledRow.ID,
		})
		if err != nil {
			return PulledNotice{}, false, err
		}
		if rows == 0 {
			return PulledNotice{}, false, fmt.Errorf("void: row %d not in expected delivery='done' state within its own transaction", pulledRow.ID)
		}
		if err := tx.Commit(); err != nil {
			return PulledNotice{}, false, err
		}
		return PulledNotice{}, false, nil
	}

	params := build(SessionNoticeRow{ID: pulledRow.ID, Kind: pulledRow.Kind, Text: pulledRow.Text})
	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return PulledNotice{}, false, err
	}
	if _, err := q.SetSessionNoticeMessageID(ctx, db.SetSessionNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(), ID: pulledRow.ID,
	}); err != nil {
		return PulledNotice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PulledNotice{}, false, err
	}
	messages.PublishCreated(msg)
	return PulledNotice{Message: msg, Wake: pulledRow.Wake != 0}, true, nil
}

// sessionNoticeVoidCondition is the legacy condition (notice_pull.go) with
// the async_jobs reads replaced by the injected batch reads. Shape and
// error semantics are identical: a bound-notice check error aborts the
// pull (the row stays pending); only "the work is gone" voids.
func (s *SessionNoticeStore) sessionNoticeVoidCondition(ctx context.Context, q *db.Queries, owner, ownHostID string, row db.SessionNotice) (bool, error) {
	switch row.Kind {
	case NoticeKindWakeOnly, NoticeKindChildQuestion:
		if !row.JobToolCallID.Valid {
			return false, nil
		}
		return s.boundNoticeGone(ctx, q, owner, row.JobToolCallID.String)
	case NoticeKindSupervision:
		// Same A15/R2A-9 rule as the legacy arm, over batch root rows: a
		// live root on a provably dead host is not open scope; a voided
		// delivery is not open scope either.
		running, err := s.liveRootsForOwner(ctx, q, owner)
		if err != nil {
			return false, err
		}
		for _, r := range running {
			if r.Delivery == "void" {
				continue
			}
			if s.hostNotDeadF(ownHostID, r.HostID) {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, nil
	}
}

// InsertSessionNotice persists a session_notices row with delivery='pending'
// with the given parameters and semantics.
func (s *SessionNoticeStore) InsertSessionNotice(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) error {
	_, err := s.insertSessionNotice(ctx, owner, kind, text, wake, jobToolCallID)
	return err
}

// InsertSessionNoticeReturningID is InsertSessionNotice that also returns
// the new row's id (notice_row_ids.go's legacy twin). A failed insert
// returns (0, err): no row exists.
func (s *SessionNoticeStore) InsertSessionNoticeReturningID(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) (int64, error) {
	row, err := s.insertSessionNotice(ctx, owner, kind, text, wake, jobToolCallID)
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *SessionNoticeStore) insertSessionNotice(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) (db.SessionNotice, error) {
	wakeInt := int64(0)
	if wake {
		wakeInt = 1
	}
	var jobToolCallIDParam sql.NullString
	if jobToolCallID != "" {
		jobToolCallIDParam = sql.NullString{String: jobToolCallID, Valid: true}
	}
	now := time.Now().Unix()
	return s.q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: owner, Kind: kind, Text: text, Wake: wakeInt,
		JobToolCallID: jobToolCallIDParam, CreatedAt: now, UpdatedAt: now,
	})
}

// ListSessionNotices is the legacy reader (notice_pull.go), unchanged:
// every row for owner, oldest first, regardless of delivery state.
func (s *SessionNoticeStore) ListSessionNotices(ctx context.Context, owner string) ([]SessionNoticeRow, error) {
	rows, err := s.q.ListSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]SessionNoticeRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionNoticeRow{ID: row.ID, Kind: row.Kind, Text: row.Text})
	}
	return out, nil
}

// BatchPendingInclusiveDebtRows is PendingInclusiveDebtRows
// (notice_row_ids.go) with the work-debt half re-based on the existing R8
// selected-debt identity-preserving reads: BatchOutboxDebtNodes
// (notice_selected=1, root announced, wake=1, reacted=0, delivery IN
// ('pending','done')) and BatchOutboxDebtNotices (same predicate on
// session_notices). No live-root restriction: a COMPLETED announced root
// with an unanswered wake notice is still debt, and selected CHILD notices
// count -- both are normal R8 outbox facts. Nodes keep their full
// RootID/NodeID/NoticeMessageID identity (BatchDebtNodeRef), exactly like
// BatchReactionPendingInclusiveDebtRows; notices keep id+kind
// (PendingNoticeDebt). Delivery 'void' is never debt (the query filters
// it); delivery 'none' cannot be (notice_selected=0 fails the CHECK).
func (s *SessionNoticeStore) BatchPendingInclusiveDebtRows(ctx context.Context, owner string) (BatchPendingDebt, error) {
	// Go-side predicate: BatchOutboxDebtNodes + the root-void exclusion (see
	// batchOutboxDebtNodesVoidAware in batch_reaction.go; fold back into
	// batch_outbox.sql when sqlc regeneration is available).
	nodes, err := batchOutboxDebtNodesVoidAware(ctx, s.sqlDB, owner)
	if err != nil {
		return BatchPendingDebt{}, fmt.Errorf("session notice store: pending-inclusive debt rows: batch_nodes: %w", err)
	}
	noticeRows, err := s.q.BatchOutboxDebtNotices(ctx, owner)
	if err != nil {
		return BatchPendingDebt{}, fmt.Errorf("session notice store: pending-inclusive debt rows: session_notices: %w", err)
	}
	out := BatchPendingDebt{}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n.ref)
	}
	for _, n := range noticeRows {
		out.Notices = append(out.Notices, PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
	}
	return out, nil
}

// PendingChildQuestions reads the owner's still-pending child_question
// notices and parses each into a ChildQuestion: same drop rules, same
// fail-open on a void-check error -- only the void condition's fact source
// is the injected batch one. Batch-published questions (job_tool_call_id
// NULL, exactly how BatchPublishQuestion inserts them) are parsed with the
// batch frame parser and never run the void rule, mirroring the pull path's
// !row.JobToolCallID short-circuit; bound rows keep the legacy parser and
// the bound-notice void check.
func (s *SessionNoticeStore) PendingChildQuestions(ctx context.Context, owner string) ([]ChildQuestion, error) {
	rq := s.readQuerier()
	rows, err := rq.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]ChildQuestion, 0, len(rows))
	for _, row := range rows {
		if row.Kind != NoticeKindChildQuestion {
			continue
		}
		var q ChildQuestion
		if row.JobToolCallID.Valid {
			// The pull path's own void rule; an unread job fact keeps the
			// notice (fail-open).
			if void, voidErr := s.sessionNoticeVoidCondition(ctx, rq, owner, "", row); voidErr == nil && void {
				continue
			}
			var ok bool
			if q, ok = ParseChildQuestionNotice(row.Text, row.JobToolCallID.String); !ok {
				continue
			}
		} else {
			// Batch-published: unbound by design, so the bound-notice void
			// rule never applies (same as the pull path).
			var ok bool
			if q, ok = ParseBatchChildQuestionNotice(row.Text); !ok {
				continue
			}
		}
		q.NoticeID = row.ID
		out = append(out, q)
	}
	return out, nil
}
