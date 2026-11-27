package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/session/service/components/rolecontract"
	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
	sqlitedriver "modernc.org/sqlite"
)

// Column-scoped session state updates: cost accrual, usage and summary
// pointers, todos, model slots, reasoning effort, system prompt, rename —
// plus the cross-process cancel flag and the fork-patch budget/ended-reason
// persistence.

// IncrementCost adds delta to the session's OWN cost (cost_self) atomically.
// The only writer of the monotonic per-node ledger (#1130); a negative delta
// is an error — `sessions reset` freezes a budget via ResetCostBase instead
// of lowering anything.
func (s *service) IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error) {
	if delta == 0 {
		return s.Get(ctx, sessionID)
	}
	if delta < 0 {
		return Session{}, fmt.Errorf("increment cost: negative deltas are not supported, reset the budget via ResetCostBase instead")
	}
	dbSession, err := s.q.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:       sessionID,
		Cost:     delta,
		CostSelf: delta,
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
// "unlimited" and falls through to the unconditional path. The budget read
// and the charge share one transaction on the single-connection writer pool
// (SetMaxOpenConns(1)), so two concurrent callers cannot both pass the
// check: the second sees the first one's cost_self — the #782 TOCTOU stays
// closed with the budget now being the SUBTREE budget (#1130).
func (s *service) IncrementCostIfUnderMax(ctx context.Context, sessionID string, delta, maxCost float64) (Session, bool, error) {
	if maxCost <= 0 {
		sess, err := s.IncrementCost(ctx, sessionID, delta)
		return sess, err == nil, err
	}
	if delta == 0 {
		sess, err := s.Get(ctx, sessionID)
		return sess, err == nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)

	spent, err := qtx.GetSubtreeSpent(ctx, sessionID)
	if err != nil {
		return Session{}, false, fmt.Errorf("read subtree spent: %w", err)
	}
	base, err := qtx.GetSessionCostBase(ctx, sessionID)
	if err != nil {
		return Session{}, false, fmt.Errorf("read cost base: %w", err)
	}
	if max(0, spent-base)+delta >= maxCost {
		// Refused: no charge lands. Read the snapshot INSIDE the tx (the
		// writer pool has one connection — a Get after the tx would wait on
		// this very tx), then let the deferred rollback discard it.
		item, getErr := qtx.GetSessionByID(ctx, sessionID)
		if getErr != nil {
			return Session{}, false, getErr
		}
		return s.fromDBItem(item), false, nil
	}
	dbSession, err := qtx.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:       sessionID,
		Cost:     delta,
		CostSelf: delta,
	})
	if err != nil {
		return Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, false, fmt.Errorf("commit charge: %w", err)
	}
	sess := s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, sess)
	return sess, true, nil
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
	if _, err := s.DB.ExecContext(ctx,
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
	if _, err := s.DB.ExecContext(ctx,
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
	if _, err := s.DB.ExecContext(ctx,
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

// UpdateSmartReasoningEffort, UpdateFastReasoningEffort,
// UpdateWorkerReasoningEffort and UpdateReviewerReasoningEffort write ONE
// effort column. The pair methods above always write both columns, so a
// caller that sends a single slot must use these: backfilling the unsent
// slot from an earlier read and writing the pair lets two concurrent
// single-slot requests restore each other's old value. An empty effort
// clears the column to NULL, like the pair methods.
func (s *service) UpdateSmartReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionSmartReasoningEffort(ctx, db.UpdateSessionSmartReasoningEffortParams{
		ID:                        sessionID,
		SmartModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateFastReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionFastReasoningEffort(ctx, db.UpdateSessionFastReasoningEffortParams{
		ID:                       sessionID,
		FastModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateWorkerReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionWorkerReasoningEffort(ctx, db.UpdateSessionWorkerReasoningEffortParams{
		ID:                         sessionID,
		WorkerModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateReviewerReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionReviewerReasoningEffort(ctx, db.UpdateSessionReviewerReasoningEffortParams{
		ID:                           sessionID,
		ReviewerModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

// afterEffortWrite publishes the updated session after a successful write.
func (s *service) afterEffortWrite(ctx context.Context, sessionID string, err error) error {
	if err != nil {
		return err
	}
	if sess, getErr := s.Get(ctx, sessionID); getErr == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// Rename updates only the title of a session and bumps updated_at without
// changing usage fields.
//
// It publishes an UpdatedEvent like every other column-scoped update here.
// That publish is what carries a generated session title to the browser:
// the agent's background title generation (internal/agent's generateTitle)
// has no access to the web server's Hub, so the pubsub bridge in
// internal/server/events.go — which forwards every UpdatedEvent as
// session_updated — is the ONLY path a title save has to the tabs. Rename
// used to be the one session mutation that published nothing, which is why
// a session's tab and sidebar row kept showing the pre-generation name
// until the next 5s sessions_list poll while a hand-rename (whose handler
// broadcasts explicitly) updated instantly.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	if err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	}); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, id); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// RequestCancel sets the cancel_requested flag for a session so a
// running agent (possibly in a different process) stops gracefully.
func (s *service) RequestCancel(ctx context.Context, sessionID string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 1 WHERE id = ?",
		sessionID,
	)
	return err
}

// IsCancelRequested checks whether a cancel signal is set on the session.
func (s *service) IsCancelRequested(ctx context.Context, sessionID string) (bool, error) {
	var v int64
	err := s.DB.QueryRowContext(ctx,
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
	_, err := s.DB.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 0 WHERE id = ?",
		sessionID,
	)
	return err
}

func (s *service) SetEndedReason(ctx context.Context, sessionID, reason string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE sessions SET ended_reason = ?, updated_at = strftime('%s', 'now') WHERE id = ?",
		reason, sessionID,
	)
	return err
}

func (s *service) SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE sessions SET budget_max_cost = ?, budget_max_tokens = ?, budget_timeout_sec = ?,
		 updated_at = strftime('%s', 'now') WHERE id = ?`,
		maxCost, maxTokens, timeoutSec, sessionID,
	)
	return err
}

// Session lifecycle: the Create*/Delete row-management methods, plus the
// agent-tool sub-session ID scheme ("messageID$$toolCallID") that gives
// every agent tool call its own durable session row.

func (s *service) GetRootRoleContract(ctx context.Context, sessionID string) (string, *RoleContract, bool, error) {
	return rolecontract.GetRootRoleContract(ctx, s.DB, sessionID)
}

func (s *service) BindRootRoleContract(ctx context.Context, sessionID, logicalID string, contract *RoleContract) error {
	return rolecontract.BindRootRoleContract(ctx, s.DB, sessionID, logicalID, contract, ErrSessionRoleContractMismatch)
}

func (s *service) GetAgentModelRole(ctx context.Context, id string) (string, bool, error) {
	return rolecontract.GetAgentModelRole(ctx, s.DB, id)
}

func (s *service) BindAgentModelRole(ctx context.Context, id, role string) error {
	return rolecontract.BindAgentModelRole(ctx, s.DB, id, role, ErrAgentModelRoleMismatch)
}

// bindWorkspace is the ONE place a new session row is bound to the process
// that creates it (#1142 step C, invariant WS-1): workspace_root is the
// service's own workspace, git_branch is read from <git-dir>/HEAD at creation
// time. Every creation path (createWithOrigin, the task/title rows, and the
// fork in session_fork.go) fills its CreateSessionParams through here, so a
// session can never end up bound to some other process's workspace.
func (s *service) bindWorkspace(p db.CreateSessionParams) db.CreateSessionParams {
	p.WorkspaceRoot = s.workspaceRoot
	p.GitBranch = gitBranchFromWorkDir(s.workDir)
	return p
}

func (s *service) createWithOrigin(ctx context.Context, id string, title string, origin message.Origin) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:     id,
		Title:  title,
		Origin: string(origin),
	}))
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	return s.createWithOrigin(ctx, uuid.New().String(), title, message.OriginUnspecified)
}

func (s *service) CreateWithID(ctx context.Context, id, title string) (Session, error) {
	return s.createWithOrigin(ctx, id, title, message.OriginUnspecified)
}

// CreateTaskSession creates the durable per-delegation session row keyed by
// toolCallID (deterministic: the batch delegation binds the child session
// under the SAME id, doc sec.3.8). Idempotent for a repeated
// call naming the SAME (toolCallID, parentSessionID) pair -- closes task
// #1038: a delegation whose claim already committed the row (e.g. a retried
// tool call, or an ASYNC-01 dead-host recovery that reused the id) must get
// the EXISTING session back, not a UNIQUE-constraint error. A toolCallID
// that already exists under a DIFFERENT parent stays an error -- id reuse
// across unrelated parents is never silently accepted.
func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
		// The delegation edge IS the cost-tree edge (#1130): set once at
		// creation, never re-pointed.
		CostParentID: parentSessionID,
	}))
	if err != nil {
		if isSessionsIDUniqueConstraintError(err) {
			existing, getErr := s.q.GetSessionByID(ctx, toolCallID)
			if getErr != nil {
				return Session{}, fmt.Errorf("create task session: id %s already exists but re-read failed: %w", toolCallID, getErr)
			}
			if !existing.ParentSessionID.Valid || existing.ParentSessionID.String != parentSessionID {
				return Session{}, fmt.Errorf("create task session: id %s already exists under a different parent", toolCallID)
			}
			return s.fromDBItem(existing), nil
		}
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

// isSessionsIDUniqueConstraintError reports whether err is a SQLite PRIMARY
// KEY/UNIQUE constraint violation on sessions.id -- the shape
// CreateTaskSession's idempotent-retry path (task #1038) needs to
// distinguish from any other insert failure. Same two-layer approach as
// internal/app's isSessionsIDConstraintError (typed Code()&0xff==19 fast
// path, plus a textual "constraint failed"+"sessions.id" check for drivers
// without a typed error) -- duplicated here rather than imported because
// internal/app depends on internal/session, not the other way around.
func isSessionsIDUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	const sqliteConstraintCode = 19
	var sqliteErr *sqlitedriver.Error
	isTypedConstraint := errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraintCode
	msg := err.Error()
	isTextualConstraint := strings.Contains(msg, "constraint failed")
	if !isTypedConstraint && !isTextualConstraint {
		return false
	}
	return strings.Contains(msg, "sessions.id")
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	}))
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	dbSession, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if err = qtx.DeleteSessionMessages(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session messages: %w", err)
	}
	if err = qtx.DeleteSessionFiles(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session files: %w", err)
	}
	if err = qtx.DeleteSession(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.DeletedEvent, session)
	return nil
}

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}

// CreateWithOrigin is Create plus an explicit entry-channel origin
// (message.OriginCLI/Web/SDK) persisted on the session row.
func (s *service) CreateWithOrigin(ctx context.Context, title string, origin message.Origin) (Session, error) {
	return s.createWithOrigin(ctx, uuid.New().String(), title, origin)
}

// CreateWithIDAndOrigin is CreateWithID plus an explicit entry-channel
// origin persisted on the session row.
func (s *service) CreateWithIDAndOrigin(ctx context.Context, id, title string, origin message.Origin) (Session, error) {
	return s.createWithOrigin(ctx, id, title, origin)
}

// OriginCreator is the consuming-package seam for the origin-aware
// Create siblings above. It is deliberately NOT part of Service: adding
// methods to Service would force every test fake implementing it across
// the repo to grow stubs. Callers (internal/app, internal/server)
// type-assert their session.Service value to OriginCreator and fall back
// to the plain Create/CreateWithID when the assertion fails — the same
// pattern internal/agent's credentialRunner uses for
// Coordinator.RunWithCredentials.
type OriginCreator interface {
	CreateWithOrigin(ctx context.Context, title string, origin message.Origin) (Session, error)
	CreateWithIDAndOrigin(ctx context.Context, id, title string, origin message.Origin) (Session, error)
}

var _ OriginCreator = (*service)(nil)

const (
	maxAgentDepth     = 32
	defaultAgentDepth = 2
)

// AgentDepthStore stores the durable remaining delegation budget.
type AgentDepthStore interface {
	CreateTaskSessionWithAgentDepth(context.Context, string, string, string, int) (Session, error)
	ResolveAgentDepth(context.Context, string, int) (int, error)
	ResolveSavedAgentDepth(context.Context, string) (int, error)
	LowerAgentDepth(context.Context, string, int) error
}

func (s *service) CreateTaskSessionWithAgentDepth(ctx context.Context, callID, parentID, title string, remaining int) (Session, error) {
	if remaining < 0 || remaining > maxAgentDepth {
		return Session{}, fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin agent child creation: %w", err)
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	created, err := qtx.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID: callID, ParentSessionID: sql.NullString{String: parentID, Valid: true},
		Title: title, CostParentID: parentID,
	}))
	if err != nil {
		if !isSessionsIDUniqueConstraintError(err) {
			return Session{}, err
		}
		created, err = qtx.GetSessionByID(ctx, callID)
		if err != nil {
			return Session{}, fmt.Errorf("re-read agent child session: %w", err)
		}
		if !created.ParentSessionID.Valid || created.ParentSessionID.String != parentID {
			return Session{}, errors.New("agent child session exists under another parent")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_agent_depth(session_id, remaining_depth) VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET remaining_depth = MIN(remaining_depth, excluded.remaining_depth)`, callID, remaining); err != nil {
		return Session{}, fmt.Errorf("persist agent child depth: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit agent child creation: %w", err)
	}
	result := s.fromDBItem(created)
	s.Publish(pubsub.CreatedEvent, result)
	return result, nil
}

// ResolveAgentDepth fails closed on missing or cyclic ancestry.
func (s *service) ResolveAgentDepth(ctx context.Context, sessionID string, rootCap int) (int, error) {
	if rootCap < 0 || rootCap > maxAgentDepth {
		return 0, fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	current := sessionID
	seen := make(map[string]struct{})
	remainingEdges := 0
	limit := rootCap
	hasSaved := false
	for current != "" {
		if _, exists := seen[current]; exists {
			return 0, errors.New("agent depth lineage cycle")
		}
		seen[current] = struct{}{}
		row, err := s.q.GetSessionByID(ctx, current)
		if err != nil {
			return 0, fmt.Errorf("agent depth lineage unavailable: %w", err)
		}
		var saved int
		err = s.DB.QueryRowContext(ctx, `SELECT remaining_depth FROM session_agent_depth WHERE session_id = ?`, current).Scan(&saved)
		if err == nil {
			hasSaved = true
			if saved < 0 || saved > maxAgentDepth {
				return 0, errors.New("stored agent depth is invalid")
			}
			candidate := saved - remainingEdges
			if candidate < limit {
				limit = candidate
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read stored agent depth: %w", err)
		}
		if !row.ParentSessionID.Valid || row.ParentSessionID.String == "" {
			if !hasSaved {
				candidate := defaultAgentDepth - remainingEdges
				if remainingEdges == 0 {
					candidate = rootCap
				}
				if candidate < limit {
					limit = candidate
				}
			}
			break
		}
		current = row.ParentSessionID.String
		remainingEdges++
	}
	if limit < 0 {
		return 0, nil
	}
	return limit, nil
}

// ResolveSavedAgentDepth returns the durable lineage cap without a new default.
func (s *service) ResolveSavedAgentDepth(ctx context.Context, sessionID string) (int, error) {
	current := sessionID
	seen := make(map[string]struct{})
	remainingEdges := 0
	limit := maxAgentDepth
	hasSaved := false
	for current != "" {
		if _, exists := seen[current]; exists {
			return 0, errors.New("agent depth lineage cycle")
		}
		seen[current] = struct{}{}
		row, err := s.q.GetSessionByID(ctx, current)
		if err != nil {
			return 0, fmt.Errorf("agent depth lineage unavailable: %w", err)
		}
		var saved int
		err = s.DB.QueryRowContext(ctx, `SELECT remaining_depth FROM session_agent_depth WHERE session_id = ?`, current).Scan(&saved)
		if err == nil {
			hasSaved = true
			if saved < 0 || saved > maxAgentDepth {
				return 0, errors.New("stored agent depth is invalid")
			}
			limit = min(limit, saved-remainingEdges)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read stored agent depth: %w", err)
		}
		if !row.ParentSessionID.Valid || row.ParentSessionID.String == "" {
			if !hasSaved {
				limit = defaultAgentDepth - remainingEdges
			}
			break
		}
		current = row.ParentSessionID.String
		remainingEdges++
	}
	return max(limit, 0), nil
}

// LowerAgentDepth only tightens the saved cap.
func (s *service) LowerAgentDepth(ctx context.Context, sessionID string, remaining int) error {
	if remaining < 0 || remaining > maxAgentDepth {
		return fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO session_agent_depth(session_id, remaining_depth) VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET remaining_depth = MIN(remaining_depth, excluded.remaining_depth)`, sessionID, remaining)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return nil
}

var _ AgentDepthStore = (*service)(nil)

// Durable queued-work purge for stopped sessions (task #1153): removes every
// row that could make the next process's RunQueuePump resurrect a session
// the operator stopped via kill / cancel / reset --force.

// PurgedQueuedWork reports how many durable queued-work rows of one session
// were removed by PurgeQueuedWorkForSession, per table.
type PurgedQueuedWork struct {
	RunQueue      int64
	PendingInject int64
	OrphanOutbox  int64
}

// PurgeQueuedWorkForSession deletes every durable queued-work row of
// sessionID in one transaction: all session_run_queue entries (both pending
// and leased — a caller that proved the session's holder dead also proves the
// lease owner is dead), all pending_injects rows (merge and interrupt), and
// all orphan_call_outbox fallback rows (the pump would otherwise move those
// back into the run queue and resurrect the session). Used by
// `rush sessions kill`, `rush sessions cancel`, and `rush sessions reset
// --force` so a stopped session cannot be restarted by the RunQueuePump of
// the next process on the same database. The messages rows behind inject
// signals are NOT touched: kill must not destroy conversation history, only
// the "something should run this" signals.
//
// The session execution requirement and credential marker rows are
// deliberately preserved: both are acked metadata and invisible to queue
// selections; deleting the session cascades them.
func (s *service) PurgeQueuedWorkForSession(ctx context.Context, sessionID string) (PurgedQueuedWork, error) {
	var purged PurgedQueuedWork
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return purged, err
	}
	defer tx.Rollback() //nolint:errcheck

	for _, step := range []struct {
		query  string
		args   []any
		target *int64
	}{
		{
			`DELETE FROM session_run_queue WHERE session_id = ? AND id NOT IN (?, ?)`,
			[]any{sessionID, sesstypes.SessionExecutionRequirementRowID(sessionID), sesstypes.SessionCredentialMarkerRowID(sessionID)},
			&purged.RunQueue,
		},
		{`DELETE FROM pending_injects WHERE session_id = ?`, []any{sessionID}, &purged.PendingInject},
		{`DELETE FROM orphan_call_outbox WHERE session_id = ?`, []any{sessionID}, &purged.OrphanOutbox},
	} {
		res, execErr := tx.ExecContext(ctx, step.query, step.args...)
		if execErr != nil {
			return purged, execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return purged, rowsErr
		}
		*step.target = n
	}
	if err := tx.Commit(); err != nil {
		return purged, err
	}
	return purged, nil
}
