package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
)

// Cross-process message inject queue (pending_injects): plain merge
// injects drained at PrepareStep, and interrupt-style injects owned by
// the interrupt ticker via its peek/consume/delete protocol.

// PendingInject is one row of the cross-process inject queue. It is a
// signal pointing at an already-created messages row (MessageID); Content is
// carried only for debugging/logging. Interrupt distinguishes a plain merge
// (false) from an interrupt-style inject (true) owned by the interrupt
// ticker.
type PendingInject struct {
	ID        string
	SessionID string
	MessageID string
	Content   string
	Interrupt bool
	CreatedAt int64
}

// CreatePendingInject enqueues a cross-process inject signal for sessionID.
// The caller (e.g. `rush sessions inject`) is responsible for having
// already created the referenced messages row so it is immediately visible
// in the web UI; this only records the request to splice it into the live
// prompt of whatever process is running the session.
func (s *service) CreatePendingInject(ctx context.Context, inject PendingInject) (retErr error) {
	if inject.ID == "" {
		inject.ID = uuid.NewString()
	}
	if inject.CreatedAt == 0 {
		inject.CreatedAt = time.Now().Unix()
	}
	if FinalizationScoped(ctx) {
		// A duplicate id is not productive work: refuse before admission can
		// mint a revision. Unscoped callers keep the writer's own error.
		var present bool
		if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE id=?)`, inject.ID).Scan(&present); err != nil {
			return err
		}
		if present {
			return fmt.Errorf("pending inject %q already exists", inject.ID)
		}
	}
	release, admission, err := AdmitFinalizationMutation(ctx, s.DB, inject.SessionID)
	if release != nil {
		defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	if err != nil {
		return err
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	return tx.Commit()
}

// DrainPendingInjects consumes the non-interrupt (interrupt = 0) inject rows
// for sessionID, deleting them in the same transaction (delete-after-read),
// and returns them ordered oldest-first for merging into the current prompt.
// The second return value reports whether an interrupt (interrupt = 1) row is
// also pending; those rows are NOT returned or deleted here — they are owned
// by the interrupt ticker, which is expected to consume them before the next
// PrepareStep. Reporting their presence lets PrepareStep log a defensive
// warning if one slipped through.
//
// SQLite serialises writers, so there is no cross-process race; the enclosing
// transaction guards against two goroutines inside this process draining the
// same rows concurrently.
func (s *service) DrainPendingInjects(ctx context.Context, sessionID string) (merge []PendingInject, hasInterrupt bool, retErr error) {
	var admission *BatchFinalizationAdmission
	if FinalizationScoped(ctx) {
		// An empty drain is not productive work and must not mint a revision.
		var productive bool
		if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE session_id=? AND interrupt=0)`, sessionID).Scan(&productive); err != nil {
			return nil, false, err
		}
		if productive {
			var release func() error
			var err error
			release, admission, err = AdmitFinalizationMutation(ctx, s.DB, sessionID)
			if release != nil {
				defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
			}
			if err != nil {
				return nil, false, err
			}
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects WHERE session_id = ? ORDER BY created_at ASC, rowid ASC`,
		sessionID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var consumedIDs []string
	for rows.Next() {
		var (
			pi        PendingInject
			interrupt int64
		)
		if scanErr := rows.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
			return nil, false, scanErr
		}
		if interrupt != 0 {
			pi.Interrupt = true
			hasInterrupt = true
			continue
		}
		merge = append(merge, pi)
		consumedIDs = append(consumedIDs, pi.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	if len(consumedIDs) > 0 {
		if err := ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
			return nil, false, err
		}
	}
	for _, id := range consumedIDs {
		if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, id); delErr != nil {
			return nil, false, delErr
		}
	}
	if len(consumedIDs) > 0 {
		if err := ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return merge, hasInterrupt, nil
}

// PeekInterruptInject reads the OLDEST interrupt=true pending_injects row
// for sessionID WITHOUT deleting it. Used by handleInterruptTick to read
// the message reference before building call data, so it can call
// ConsumeInterruptInjectAndEnqueue atomically.
//
// Uses the standalone WAL reader so a busy writer cannot time out CLI waiting.
// Returns (nil, nil) when no committed interrupt row is pending.
func (s *service) PeekInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error) {
	var (
		pi        PendingInject
		interrupt int64
	)
	row := s.readDB.QueryRowContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects
		 WHERE session_id = ? AND interrupt = 1
		 ORDER BY created_at ASC, rowid ASC LIMIT 1`,
		sessionID,
	)
	scanErr := row.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, scanErr
	}
	pi.Interrupt = interrupt != 0
	return &pi, nil
}

// DeleteInterruptInject removes a specific pending inject row by ID.
// Used by detached interrupt runs to delete the durable pending row AFTER
// they have confirmed execution (acquired OS lock). P0-2 fix. It returns
// sql.ErrNoRows when another consumer already removed the row.
func (s *service) DeleteInterruptInject(ctx context.Context, injectID string) (retErr error) {
	var scopedOwner string
	var admission *BatchFinalizationAdmission
	if FinalizationScoped(ctx) {
		// A missing row is not productive work: no admission, legacy result.
		if err := s.DB.QueryRowContext(ctx, `SELECT session_id FROM pending_injects WHERE id=?`, injectID).Scan(&scopedOwner); err != nil {
			return err
		}
		var release func() error
		var err error
		release, admission, err = AdmitFinalizationMutation(ctx, s.DB, scopedOwner)
		if release != nil {
			defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
		}
		if err != nil {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM pending_injects WHERE id = ?`, injectID).Scan(&owner); err != nil {
		return err
	}
	if scopedOwner != "" && owner != scopedOwner {
		return ErrAgentFinalizationChanged
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}

	result, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, injectID)
	if delErr != nil {
		return delErr
	}
	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return rowsErr
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeInterruptInjectAndEnqueue atomically consumes the pending inject row
// identified by injectID and enqueues it to the run queue in a single
// transaction.
//
// injectID must be the ID of a row the caller already read (e.g. via
// PeekInterruptInject) and built callData from. Matching on that specific ID
// — rather than re-selecting "the oldest pending row" — matters because a
// session can have more than one interrupt=1 row queued: if some other
// path (a concurrent tick, a foreign process) deletes the peeked row between
// Peek and this call, re-selecting "oldest" would silently consume a
// DIFFERENT row than the one callData was built from, losing that row's
// content while the peeked row's own deletion goes unnoticed.
//
// Returns (nil, nil) when injectID no longer refers to a pending row (it
// vanished between peek and consume — e.g. already handled by another
// process); the caller should treat this as a no-op, not an error.
// Returns (pi, nil) when the row was successfully consumed and enqueued.
// Returns error on failure — the transaction is rolled back, so the row
// remains for retry.
func (s *service) ConsumeInterruptInjectAndEnqueue(ctx context.Context, sessionID, injectID, idempotencyKey string, callData []byte) (ret *PendingInject, retErr error) {
	if FinalizationScoped(ctx) {
		// A vanished source row is not productive work: no admission.
		var present bool
		if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE id=? AND session_id=? AND interrupt=1)`, injectID, sessionID).Scan(&present); err != nil {
			return nil, err
		}
		if !present {
			return nil, nil
		}
	}
	payload, err := s.prepareFinalizationIngress(ctx, sessionID, callData)
	if err != nil {
		return nil, err
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	release, admission, err := AdmitFinalizationMutation(ctx, s.DB, sessionID)
	if release != nil {
		defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return nil, err
	}
	ancestryHash, err := finalizationIngressAncestryHash(payload, admission)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		pi        PendingInject
		interrupt int64
	)
	row := tx.QueryRowContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects
		 WHERE id = ? AND session_id = ? AND interrupt = 1`,
		injectID, sessionID,
	)
	if scanErr := row.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, scanErr
	}
	pi.Interrupt = interrupt != 0
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
		return nil, err
	}

	// Delete the pending inject row first
	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, pi.ID); delErr != nil {
		return nil, delErr
	}

	// Then enqueue to run queue in the same transaction
	if existing, err := verifyInterruptEnqueueCollisionTx(ctx, tx, idempotencyKey, sessionID, payload); err != nil {
		return nil, err
	} else if !existing {
		if err := validateFreshFinalizationIngressTx(ctx, tx, sessionID, idempotencyKey, payload, ancestryHash, admission); err != nil {
			return nil, err
		}
		now := time.Now().Unix()
		qtx := s.q.WithTx(tx)
		_, enqueueErr := qtx.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
			ID:        idempotencyKey,
			SessionID: sessionID,
			CallData:  string(callData),
			CreatedAt: now,
			UpdatedAt: now,
		})
		if errors.Is(enqueueErr, sql.ErrNoRows) {
			// Idempotency hit: entry already exists with this key, treat as success
			enqueueErr = nil
		}
		if enqueueErr != nil {
			return nil, enqueueErr
		}
		if err := persistFinalizationIngressTx(ctx, tx, idempotencyKey, sessionID, payload, ancestryHash); err != nil {
			return nil, err
		}
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &pi, nil
}

// verifyInterruptEnqueueCollisionTx handles an idempotency hit. A bound
// replacement must match its recorded provenance; an unbound hit keeps the
// historical success and only refuses a newly bound owner scope.
func verifyInterruptEnqueueCollisionTx(ctx context.Context, tx *sql.Tx, key, owner string, payload finalizationIngressPayload) (bool, error) {
	_, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, key)
	if err != nil || !exists {
		return false, err
	}
	if payload.bound {
		return verifyQueueCollisionTx(ctx, tx, key, owner, payload)
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, owner, key, existingRaw, payload); err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileInterruptInjectEnqueue removes the exact durable replacement made
// for inject and restores inject in one transaction. idempotencyKey identifies
// one attempt, not the source inject. A missing replacement is treated as an
// already-completed reconciliation, which makes retry after an ambiguous
// commit safe without deleting another attempt's row or restoring its source.
func (s *service) ReconcileInterruptInjectEnqueue(ctx context.Context, inject PendingInject, idempotencyKey string) (retErr error) {
	var raw string
	if err := s.DB.QueryRowContext(ctx, `SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	payload, err := s.prepareFinalizationIngress(ctx, inject.SessionID, []byte(raw))
	if err != nil {
		return err
	}
	release, admission, err := AdmitFinalizationMutation(ctx, s.DB, inject.SessionID)
	if release != nil {
		defer func() { retErr = BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var actualRaw string
	if err := tx.QueryRowContext(ctx, `SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID).Scan(&actualRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if payload.bound && actualRaw != raw {
		return ErrAgentFinalizationChanged
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, inject.SessionID, idempotencyKey, raw, payload); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	if payload.bound {
		// The replacement is gone, so its unconsumed provenance goes with it.
		cleared, err := tx.ExecContext(ctx, `DELETE FROM session_agent_finalization_continuations WHERE row_id = ? AND consumed = 0`, idempotencyKey)
		if err != nil {
			return err
		}
		if n, err := cleared.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrAgentFinalizationChanged
		}
	}

	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	if err != nil {
		return err
	}
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	return tx.Commit()
}

// Session forking: ForkOptions plus the single-transaction clone
// (ForkSessionTx) and its web-button wrapper (ForkSession), copying a
// session row and its messages into a brand-new session.

// ForkOptions carries the knobs ForkSessionTx supports beyond "copy
// everything from srcID into a new top-level session". See the Service
// interface doc on ForkSessionTx for the zero-value default of each field.
type ForkOptions struct {
	// NewID is the caller-chosen ID for the forked session. Empty means
	// generate a fresh uuid.New().String().
	NewID string
	// Title is the forked session's title. Empty means "<src title> fork".
	Title string
	// ParentID sets the fork's parent_session_id, making it a child session
	// (as `rush sessions fork --child` does). Empty means top-level (no
	// parent) — the web fork button's behavior.
	ParentID string
	// LimitMsgs truncates the copy to the first LimitMsgs messages
	// (1-indexed). 0 means "copy every message". A non-zero value outside
	// 1..len(source messages) is rejected.
	LimitMsgs int
}

// ForkSession is a thin wrapper around ForkSessionTx using the web fork
// button's defaults: server-generated ID, top-level session, every message
// copied. It additionally publishes a pubsub.CreatedEvent after commit,
// since the web path and its subscribers live in the same process.
func (s *service) ForkSession(ctx context.Context, srcID, title string) (Session, error) {
	fork, _, err := s.ForkSessionTx(ctx, srcID, ForkOptions{Title: title})
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.CreatedEvent, fork)
	return fork, nil
}

// ForkSessionTx clones srcID into a brand-new session in a single DB
// transaction. It creates a fresh session row, copies the source's models,
// system prompt, reasoning effort, and todos/deleted_todos, and copies the
// first o.LimitMsgs messages (or all of them, when o.LimitMsgs is 0)
// verbatim — all inside one tx so a failure at any point (e.g. the Nth
// message copy) rolls back the new session row and every message copied so
// far. The caller gets an error and no half-built fork is left behind.
// Mirrors the transactional shape of Delete and TransferChildCostToParent.
//
// See the Service interface doc on ForkSessionTx for ForkOptions defaults
// and the pubsub-publish contract (this method does NOT publish; callers do).
func (s *service) ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, 0, fmt.Errorf("begin fork transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// Read the source inside the tx so the copy is consistent with itself.
	src, err := qtx.GetSessionByID(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("load source session: %w", err)
	}

	srcMsgs, err := qtx.ListMessagesBySession(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("list source messages: %w", err)
	}

	// LimitMsgs == 0 means "copy everything" and is always valid, including
	// against an empty source (forking a brand-new, message-less session is
	// a legitimate operation). A non-zero LimitMsgs must fall within
	// 1..len(srcMsgs).
	limit := o.LimitMsgs
	if limit == 0 {
		limit = len(srcMsgs)
	} else if limit < 1 || limit > len(srcMsgs) {
		return Session{}, 0, fmt.Errorf("--at %d is out of range (1..%d)", limit, len(srcMsgs))
	}
	srcMsgs = srcMsgs[:limit]

	resolvedTitle := o.Title
	if resolvedTitle == "" {
		resolvedTitle = src.Title + " fork"
	}

	forkID := o.NewID
	if forkID == "" {
		forkID = uuid.New().String()
	}

	// The fork belongs to THIS process's workspace, not to the source's: a
	// fork is the one supported way to continue a session's history in a
	// different checkout (#1142 step C). Binding it to the source's workspace
	// would produce a row no process here may drive.
	createParams := s.bindWorkspace(db.CreateSessionParams{
		ID:     forkID,
		Title:  resolvedTitle,
		Origin: src.Origin,
		// No CostParentID: a fork is its OWN cost root (#1130), even when it
		// points at a parent for history.
	})
	if o.ParentID != "" {
		createParams.ParentSessionID = sql.NullString{String: o.ParentID, Valid: true}
	}
	if _, err := qtx.CreateSession(ctx, createParams); err != nil {
		return Session{}, 0, fmt.Errorf("create forked session: %w", err)
	}

	// Carry the secret-free credential marker into a fork when the source or
	// any ancestor has it. Walk the complete parent chain in this transaction;
	// a broken chain or any read error fails closed rather than losing the
	// credential requirement on continuation.
	var credentialMarker string
	marked := false
	ancestorID := srcID
	for ancestorID != "" {
		markerErr := tx.QueryRowContext(ctx,
			`SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
			SessionCredentialMarkerRowID(ancestorID), ancestorID).Scan(&credentialMarker)
		if markerErr == nil {
			marked = true
		} else if markerErr != sql.ErrNoRows {
			return Session{}, 0, fmt.Errorf("load credential marker for session %s: %w", ancestorID, markerErr)
		}

		var parentID sql.NullString
		parentErr := tx.QueryRowContext(ctx,
			`SELECT parent_session_id FROM sessions WHERE id = ?`, ancestorID).Scan(&parentID)
		if parentErr != nil {
			return Session{}, 0, fmt.Errorf("load parent for session %s: %w", ancestorID, parentErr)
		}
		if !parentID.Valid {
			break
		}
		ancestorID = parentID.String
	}
	if marked {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, terminal_failure, created_at, updated_at)
			 VALUES (?, ?, ?, 'acked', 0, 1, strftime('%s', 'now'), strftime('%s', 'now'))`,
			SessionCredentialMarkerRowID(forkID), forkID, `{"kind":"session_credential_requirement","version":1}`); err != nil {
			return Session{}, 0, fmt.Errorf("copy credential marker into fork: %w", err)
		}
	}

	// Copy the source's model selection, system prompt, reasoning effort,
	// and todos onto the fork via column-scoped UPDATEs routed through qtx
	// so they share the tx.
	if err := qtx.UpdateSessionModels(ctx, db.UpdateSessionModelsParams{
		SmartModelProvider: src.SmartModelProvider,
		SmartModelID:       src.SmartModelID,
		FastModelProvider:  src.FastModelProvider,
		FastModelID:        src.FastModelID,
		ID:                 forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy models into fork: %w", err)
	}
	if err := qtx.UpdateSessionWorkerReviewerModels(ctx, db.UpdateSessionWorkerReviewerModelsParams{
		WorkerModelProvider:   src.WorkerModelProvider,
		WorkerModelID:         src.WorkerModelID,
		ReviewerModelProvider: src.ReviewerModelProvider,
		ReviewerModelID:       src.ReviewerModelID,
		ID:                    forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy worker/reviewer models into fork: %w", err)
	}
	if err := qtx.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		SystemPrompt: src.SystemPrompt,
		ID:           forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy system prompt into fork: %w", err)
	}
	if err := qtx.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		SmartModelReasoningEffort: src.SmartModelReasoningEffort,
		FastModelReasoningEffort:  src.FastModelReasoningEffort,
		ID:                        forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy reasoning effort into fork: %w", err)
	}
	if err := qtx.UpdateSessionWorkerReviewerReasoningEffort(ctx, db.UpdateSessionWorkerReviewerReasoningEffortParams{
		WorkerModelReasoningEffort:   src.WorkerModelReasoningEffort,
		ReviewerModelReasoningEffort: src.ReviewerModelReasoningEffort,
		ID:                           forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy worker/reviewer reasoning effort into fork: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		src.Todos,
		src.DeletedTodos,
		forkID,
	); err != nil {
		return Session{}, 0, fmt.Errorf("copy todos into fork: %w", err)
	}

	// Copy every selected message verbatim. Parts is carried across as the
	// raw JSON blob (no decode/re-encode round-trip), so the fork is a
	// faithful copy of the source history. Any copy error aborts the whole
	// transaction.
	//
	// Deliberately no per-message pubsub.CreatedEvent here (unlike
	// message.Service.Create, which the old non-transactional path went
	// through): this loop writes via qtx.CreateMessage directly against the
	// tx, bypassing the message package's Service/Broker entirely, so there
	// is no message.Service handle available to publish through from inside
	// ForkSessionTx. Publishing would also need to wait until AFTER the
	// commit below (a subscriber must never observe a message row from an
	// uncommitted, possibly-rolled-back tx), which would mean re-reading
	// every copied message post-commit just to build event payloads.
	// Checked whether any subscriber actually needs incremental per-message
	// events for a fork: the only consumer is the web UI
	// (internal/server/events.go forwards message.Service's broker to
	// EventMessageCreated/Updated), and its client-side handler for the
	// session-level fork event does NOT rely on incremental message
	// events — web/src/useWS.ts's "session_created" handler unconditionally
	// calls `ws.send("load_messages", { sessionID: s.ID })` for every new
	// session (fork included), which does a full re-fetch of the session's
	// messages. So a client attached at fork time still ends up with a
	// complete, correct transcript. If a future subscriber needs
	// incremental per-message fork events, publish them AFTER tx.Commit()
	// below (loop over the re-read committed rows), not inside the tx.
	// Keep the source-to-fork IDs so session-level pointers can follow the
	// copied rows without referring back into the source session.
	messageIDMap := make(map[string]string, len(srcMsgs))
	for _, m := range srcMsgs {
		copiedMessageID := uuid.New().String()
		if _, err := qtx.CreateMessage(ctx, db.CreateMessageParams{
			ID:                  copiedMessageID,
			SessionID:           forkID,
			Role:                m.Role,
			Parts:               m.Parts,
			Model:               m.Model,
			Provider:            m.Provider,
			ReasoningEffort:     m.ReasoningEffort,
			IsSummaryMessage:    m.IsSummaryMessage,
			Hidden:              m.Hidden,
			AutoResumed:         m.AutoResumed,
			BackgroundJobNotice: m.BackgroundJobNotice,
			Origin:              m.Origin,
		}); err != nil {
			return Session{}, 0, fmt.Errorf("copy message into fork: %w", err)
		}
		messageIDMap[m.ID] = copiedMessageID
		// CreateMessage intentionally omits finished_at; preserve its exact
		// nullable value so completed assistant messages remain deletable as
		// terminal rows and unfinished messages remain streaming.
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET finished_at = ? WHERE id = ?`, m.FinishedAt, copiedMessageID); err != nil {
			return Session{}, 0, fmt.Errorf("copy message finish time into fork: %w", err)
		}
		if m.Pinned != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET pinned = ? WHERE id = ?`, m.Pinned, copiedMessageID); err != nil {
				return Session{}, 0, fmt.Errorf("copy pinned state into fork: %w", err)
			}
		}
	}

	if copiedSummaryID, ok := messageIDMap[src.SummaryMessageID.String]; ok && src.SummaryMessageID.Valid {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET summary_message_id = ? WHERE id = ?`,
			copiedSummaryID, forkID,
		); err != nil {
			return Session{}, 0, fmt.Errorf("copy summary pointer into fork: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Session{}, 0, fmt.Errorf("commit fork transaction: %w", err)
	}

	// Re-read the committed fork to return its final, fully-populated state.
	fork, err := s.q.GetSessionByID(ctx, forkID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("reload forked session: %w", err)
	}
	return s.fromDBItem(fork), len(srcMsgs), nil
}
