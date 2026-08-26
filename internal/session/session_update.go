// Column-scoped session state updates: cost accrual and transfer, usage
// and summary pointers, todos, model slots, reasoning effort, system
// prompt, and rename — plus the cross-process cancel flag and the
// fork-patch budget/ended-reason persistence.

package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// IncrementCost adds delta to the session cost atomically. See interface
// doc on Service.IncrementCost for rationale.
func (s *service) IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error) {
	if delta == 0 {
		return s.Get(ctx, sessionID)
	}
	dbSession, err := s.q.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:   sessionID,
		Cost: delta,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// IncrementCostIfUnderMax — see interface doc on
// Service.IncrementCostIfUnderMax for rationale. maxCost <= 0 means
// "unlimited": the predicate would otherwise reject every charge (cost +
// delta < 0 is never true for non-negative cost/delta), so that case falls
// through to the same unconditional path as IncrementCost.
func (s *service) IncrementCostIfUnderMax(ctx context.Context, sessionID string, delta, maxCost float64) (Session, bool, error) {
	if maxCost <= 0 {
		sess, err := s.IncrementCost(ctx, sessionID, delta)
		return sess, err == nil, err
	}
	if delta == 0 {
		sess, err := s.Get(ctx, sessionID)
		return sess, err == nil, err
	}
	rows, err := s.q.IncrementSessionCostIfUnderMax(ctx, db.IncrementSessionCostIfUnderMaxParams{
		ID:      sessionID,
		Delta:   delta,
		MaxCost: maxCost,
	})
	if err != nil {
		return Session{}, false, err
	}
	if rows == 0 {
		sess, getErr := s.Get(ctx, sessionID)
		if getErr != nil {
			return Session{}, false, getErr
		}
		return sess, false, nil
	}
	sess, err := s.Get(ctx, sessionID)
	if err != nil {
		return Session{}, false, err
	}
	s.Publish(pubsub.UpdatedEvent, sess)
	return sess, true, nil
}

// TransferChildCostToParent — see Service.TransferChildCostToParent doc.
//
// The whole operation runs in one transaction so the parent charge and the
// child's accounted marker advance together or not at all: a crash between
// them can neither double-charge the parent nor lose the child's delta. The
// parent is always touched (even when delta is 0) so a deleted parent still
// surfaces as an error via the RETURNING clause — preserving the not-found
// semantics the previous IncrementCost(id, 0) short-circuit gave callers.
func (s *service) TransferChildCostToParent(ctx context.Context, childSessionID, parentSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	accounting, err := qtx.GetSessionCostAccounting(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	delta := accounting.Cost - accounting.ParentCostAccounted
	if delta < 0 {
		// Should not happen (cost only grows), but never charge negative.
		delta = 0
	}

	// Always run the parent UPDATE: for delta 0 it is a no-op write, but the
	// RETURNING clause still surfaces sql.ErrNoRows if the parent was deleted
	// between the child finishing and this call.
	if _, err := qtx.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:   parentSessionID,
		Cost: delta,
	}); err != nil {
		return fmt.Errorf("increment parent session cost: %w", err)
	}

	// Advance the child's accounted marker to its current cost so the next
	// call charges only newly accrued cost. Inside the same tx as the parent
	// charge, so a crash cannot leave the parent billed but the child lagging.
	if err := qtx.SetParentCostAccounted(ctx, db.SetParentCostAccountedParams{
		ID:                  childSessionID,
		ParentCostAccounted: accounting.Cost,
	}); err != nil {
		return fmt.Errorf("set child accounted cost: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transfer: %w", err)
	}

	// Publish refreshed snapshots so the UI reflects both new balances.
	if sess, err := s.Get(ctx, childSessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	if sess, err := s.Get(ctx, parentSessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateSystemPrompt saves a custom system prompt for a session.
func (s *service) UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	if err := s.q.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		ID:           sessionID,
		SystemPrompt: prompt,
	}); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetUsage overwrites only the prompt/completion token counters for a
// session. It does not touch title, todos, summary, or cost, so it cannot
// clobber concurrent edits to those fields the way a full Save did. Used by
// the agent's per-step finalization to persist the latest context-window
// token snapshot.
func (s *service) SetUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetSummaryAndUsage overwrites summary_message_id together with the
// prompt/completion token counters in one UPDATE. Used by the summarization
// paths (manual and silent compaction) and by `sessions reset`, which must
// flip the summary pointer and reset token counters as one logical op. Like
// SetUsage it leaves title, todos, and cost untouched, so it cannot lose
// concurrent edits to those columns.
//
// NULL vs empty-string note: `sessions reset` calls this with
// summaryMessageID equal to the Go zero value to clear the pointer, which
// writes a SQL empty string rather than NULL to summary_message_id —
// unlike the old generic Save/UpdateSession path, which stored a Go
// zero-value string as NULL via sql.NullString{Valid: false}. This is
// intentionally NOT treated as a bug: every reader of SummaryMessageID
// compares it against the empty string (Session.SummaryMessageID != ""),
// and no SQL query anywhere filters or joins on
// `summary_message_id IS NULL`. An empty string and NULL are therefore
// equivalent for every consumer of this column today. Do not "fix" this
// without first auditing for a new IS NULL usage.
func (s *service) SetSummaryAndUsage(ctx context.Context, sessionID, summaryMessageID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary_message_id = ?, prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		summaryMessageID, promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetTodos overwrites the todos and deleted_todos (tombstone) columns for a
// session in one UPDATE. It leaves title, token counters, summary, and cost
// untouched, so a todos edit can no longer clobber a concurrent rename or
// agent step the way a full Save did.
func (s *service) SetTodos(ctx context.Context, sessionID string, todos []Todo, deletedTodos []string) error {
	todosJSON, err := marshalTodos(todos)
	if err != nil {
		return err
	}
	deletedTodosJSON, err := marshalDeletedTodos(deletedTodos)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		sql.NullString{String: todosJSON, Valid: todosJSON != ""},
		deletedTodosJSON,
		sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateModels writes explicit per-session model overrides. A nil large or
// small argument leaves that slot completely untouched in the DB row —
// neither sets nor clears it. A non-nil argument with an empty
// Provider/Model clears the slot back to inheriting the folder/system
// default (the "" = inherit convention resolveSessionModels already applies
// when reading the row back); a non-nil argument with values sets an
// explicit override.
//
// The nil-means-untouched distinction exists because a caller changing only
// ONE slot (e.g. the web UI's per-slot model picker) must not silently wipe
// the OTHER slot's override back to unset — before this signature, every
// caller had to pass all four strings, so "leave the other slot alone" and
// "no override" were indistinguishable at this layer, and the web UI ended
// up pinning both smart and small on every single-slot switch (task #461).
func (s *service) UpdateModels(ctx context.Context, sessionID string, smart, fast *ModelSlotUpdate) error {
	params := db.UpdateSessionModelsParams{ID: sessionID}
	if smart != nil {
		params.SmartModelProvider = sql.NullString{String: smart.Provider, Valid: true}
		params.SmartModelID = sql.NullString{String: smart.Model, Valid: true}
	}
	if fast != nil {
		params.FastModelProvider = sql.NullString{String: fast.Provider, Valid: true}
		params.FastModelID = sql.NullString{String: fast.Model, Valid: true}
	}
	err := s.q.UpdateSessionModels(ctx, params)
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateWorkerReviewerModels is UpdateModels' sibling for the optional
// worker/reviewer slots (task #466) — same nil-means-untouched semantics.
func (s *service) UpdateWorkerReviewerModels(ctx context.Context, sessionID string, worker, reviewer *ModelSlotUpdate) error {
	params := db.UpdateSessionWorkerReviewerModelsParams{ID: sessionID}
	if worker != nil {
		params.WorkerModelProvider = sql.NullString{String: worker.Provider, Valid: true}
		params.WorkerModelID = sql.NullString{String: worker.Model, Valid: true}
	}
	if reviewer != nil {
		params.ReviewerModelProvider = sql.NullString{String: reviewer.Provider, Valid: true}
		params.ReviewerModelID = sql.NullString{String: reviewer.Model, Valid: true}
	}
	if err := s.q.UpdateSessionWorkerReviewerModels(ctx, params); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateWorkerReviewerReasoningEffort is UpdateReasoningEffort's sibling for
// the worker/reviewer slots — same always-touch semantics (an empty string
// clears the effort field) as the smart/fast original.
func (s *service) UpdateWorkerReviewerReasoningEffort(ctx context.Context, sessionID, workerEffort, reviewerEffort string) error {
	err := s.q.UpdateSessionWorkerReviewerReasoningEffort(ctx, db.UpdateSessionWorkerReviewerReasoningEffortParams{
		ID:                           sessionID,
		WorkerModelReasoningEffort:   sql.NullString{String: workerEffort, Valid: workerEffort != ""},
		ReviewerModelReasoningEffort: sql.NullString{String: reviewerEffort, Valid: reviewerEffort != ""},
	})
	if err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateReasoningEffort updates the reasoning effort for large and fast models.
func (s *service) UpdateReasoningEffort(ctx context.Context, sessionID, smartEffort, fastEffort string) error {
	err := s.q.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		ID:                        sessionID,
		SmartModelReasoningEffort: sql.NullString{String: smartEffort, Valid: smartEffort != ""},
		FastModelReasoningEffort:  sql.NullString{String: fastEffort, Valid: fastEffort != ""},
	})
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// Rename updates only the title of a session without touching updated_at or
// usage fields.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	return s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	})
}

// RequestCancel sets the cancel_requested flag for a session so a
// running agent (possibly in a different process) stops gracefully.
func (s *service) RequestCancel(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 1 WHERE id = ?",
		sessionID,
	)
	return err
}

// IsCancelRequested checks whether a cancel signal is set on the session.
func (s *service) IsCancelRequested(ctx context.Context, sessionID string) (bool, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT cancel_requested FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&v)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// ClearCancelRequest resets the cancel_requested flag. Called when a
// new run starts so a stale flag from a previous run does not kill it.
func (s *service) ClearCancelRequest(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 0 WHERE id = ?",
		sessionID,
	)
	return err
}

func (s *service) SetEndedReason(ctx context.Context, sessionID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET ended_reason = ?, updated_at = strftime('%s', 'now') WHERE id = ?",
		reason, sessionID,
	)
	return err
}

func (s *service) SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET budget_max_cost = ?, budget_max_tokens = ?, budget_timeout_sec = ?,
		 updated_at = strftime('%s', 'now') WHERE id = ?`,
		maxCost, maxTokens, timeoutSec, sessionID,
	)
	return err
}
