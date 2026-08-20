package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// mockSessionService is a minimal mock of session.Service for testing resolveSession.
type mockSessionService struct {
	sessions []session.Session
	created  []session.Session
}

func (m *mockSessionService) Subscribe(context.Context) <-chan pubsub.Event[session.Session] {
	return make(chan pubsub.Event[session.Session])
}

func (m *mockSessionService) Create(_ context.Context, title string) (session.Session, error) {
	s := session.Session{ID: "new-session-id", Title: title}
	m.created = append(m.created, s)
	return s, nil
}

// createWithIDErr lets a test simulate a UNIQUE-constraint or DB failure.
var _createWithIDErr error // populated per-test if non-nil

// _createWithIDErrLandsRow simulates task #605's actual race: our own
// CreateWithID lost the race and failed with a constraint violation
// because ANOTHER process's INSERT for the same id already committed.
// When true and _createWithIDErr is set, CreateWithID also plants the
// "winner's" row into m.sessions before returning the error — exactly
// what a losing process's subsequent Get(id) would observe against the
// real DB, since SQLite's single-writer model guarantees the winning
// transaction is already durable by the time ours is rejected.
var _createWithIDErrLandsRow bool

func (m *mockSessionService) CreateWithID(_ context.Context, id, title string) (session.Session, error) {
	if _createWithIDErr != nil {
		if _createWithIDErrLandsRow {
			m.sessions = append(m.sessions, session.Session{ID: id, Title: "winner-title"})
		}
		return session.Session{}, _createWithIDErr
	}
	s := session.Session{ID: id, Title: title}
	m.created = append(m.created, s)
	m.sessions = append(m.sessions, s)
	return s, nil
}

func (m *mockSessionService) CreateTitleSession(context.Context, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) CreateTaskSession(context.Context, string, string, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) Get(_ context.Context, id string) (session.Session, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return session.Session{}, sql.ErrNoRows
}

func (m *mockSessionService) GetLast(_ context.Context) (session.Session, error) {
	if len(m.sessions) > 0 {
		return m.sessions[0], nil
	}
	return session.Session{}, sql.ErrNoRows
}

// ListSubSessions: mock that filters in-memory by ParentSessionID.
// Added when Service.ListSubSessions was introduced for the
// reduction-loss warning and --aggregation=attach plumbing.
func (m *mockSessionService) ListSubSessions(_ context.Context, parentID string) ([]session.Session, error) {
	var out []session.Session
	for _, s := range m.sessions {
		if s.ParentSessionID == parentID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockSessionService) List(context.Context) ([]session.Session, error) {
	return m.sessions, nil
}

// IncrementCost: mock that mutates the in-memory session if found,
// returns it (or an empty one with the id-not-found semantics matching
// the real DB layer). Added when Service.IncrementCost was introduced
// as part of the parallel-process cost-race fix.
func (m *mockSessionService) IncrementCost(_ context.Context, id string, delta float64) (session.Session, error) {
	for i, s := range m.sessions {
		if s.ID == id {
			m.sessions[i].Cost += delta
			return m.sessions[i], nil
		}
	}
	return session.Session{}, nil
}

func (m *mockSessionService) TransferChildCostToParent(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) GetCallTreeActivity(context.Context, string) (session.CallTreeActivity, bool, error) {
	return session.CallTreeActivity{}, false, nil
}

func (m *mockSessionService) GetCallTreeActivityBatch(context.Context, []string) (map[string]session.CallTreeActivity, error) {
	return nil, nil
}

func (m *mockSessionService) Rename(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) SetUsage(context.Context, string, int64, int64) error { return nil }

func (m *mockSessionService) SetSummaryAndUsage(context.Context, string, string, int64, int64) error {
	return nil
}

func (m *mockSessionService) SetTodos(context.Context, string, []session.Todo, []string) error {
	return nil
}

func (m *mockSessionService) Delete(context.Context, string) error {
	return nil
}

func (m *mockSessionService) ForkSession(_ context.Context, _ string, title string) (session.Session, error) {
	return session.Session{ID: "forked-session-id", Title: title}, nil
}

func (m *mockSessionService) ForkSessionTx(_ context.Context, _ string, o session.ForkOptions) (session.Session, int, error) {
	title := o.Title
	if title == "" {
		title = "forked-session-id"
	}
	return session.Session{ID: "forked-session-id", Title: title, ParentSessionID: o.ParentID}, 0, nil
}

func (m *mockSessionService) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

func (m *mockSessionService) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (m *mockSessionService) IsAgentToolSession(sessionID string) bool {
	_, _, ok := m.ParseAgentToolSessionID(sessionID)
	return ok
}

func (m *mockSessionService) UpdateModels(context.Context, string, *session.ModelSlotUpdate, *session.ModelSlotUpdate) error {
	return nil
}

func (m *mockSessionService) UpdateReasoningEffort(context.Context, string, string, string) error {
	return nil
}

func (m *mockSessionService) UpdateWorkerReviewerModels(context.Context, string, *session.ModelSlotUpdate, *session.ModelSlotUpdate) error {
	return nil
}

func (m *mockSessionService) UpdateWorkerReviewerReasoningEffort(context.Context, string, string, string) error {
	return nil
}

func (m *mockSessionService) UpdateSystemPrompt(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) RequestCancel(context.Context, string) error { return nil }
func (m *mockSessionService) IsCancelRequested(context.Context, string) (bool, error) {
	return false, nil
}
func (m *mockSessionService) ClearCancelRequest(context.Context, string) error     { return nil }
func (m *mockSessionService) SetEndedReason(context.Context, string, string) error { return nil }
func (m *mockSessionService) SetBudget(context.Context, string, float64, int64, int64) error {
	return nil
}

func (m *mockSessionService) ListAll(context.Context) ([]session.Session, error) {
	return m.sessions, nil
}

func (m *mockSessionService) CreatePendingInject(context.Context, session.PendingInject) error {
	return nil
}

func (m *mockSessionService) DrainPendingInjects(context.Context, string) ([]session.PendingInject, bool, error) {
	return nil, false, nil
}

func (m *mockSessionService) PeekInterruptInject(context.Context, string) (*session.PendingInject, error) {
	return nil, nil
}

func (m *mockSessionService) ConsumeInterruptInjectAndEnqueue(context.Context, string, string, string, []byte) (*session.PendingInject, error) {
	return nil, nil
}

func (m *mockSessionService) DeleteInterruptInject(context.Context, string) error {
	return nil
}

func (m *mockSessionService) EnqueueRunQueueEntry(context.Context, string, string, []byte) error {
	return nil
}

func (m *mockSessionService) LeaseRunQueueEntry(context.Context, string, string, time.Duration) (*session.RunQueueEntry, error) {
	return nil, nil
}

func (m *mockSessionService) RenewRunQueueLease(context.Context, string, string, int64) (bool, error) {
	return false, nil
}

func (m *mockSessionService) AckRunQueueEntry(context.Context, string, string) (string, error) {
	return "", nil
}

func (m *mockSessionService) NackRunQueueEntry(context.Context, string, string, string) error {
	return nil
}

func (m *mockSessionService) NackRunQueueEntryNoAttemptPenalty(context.Context, string, string, string) error {
	return nil
}

func (m *mockSessionService) TerminalFailRunQueueEntry(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) ListPendingRunQueueEntries(context.Context) ([]session.RunQueueEntry, error) {
	return nil, nil
}

func (m *mockSessionService) ListStaleLeasedRunQueueEntries(context.Context, int64) ([]session.RunQueueEntry, error) {
	return nil, nil
}

func (m *mockSessionService) GetRunQueueEntry(context.Context, string) (*session.RunQueueEntry, error) {
	return nil, nil
}

func (m *mockSessionService) CleanupExpiredLeases(context.Context, int64) error {
	return nil
}

func (m *mockSessionService) WriteToOrphanOutbox(context.Context, string, string, []byte) error {
	return nil
}

func (m *mockSessionService) ListPendingOrphanOutboxEntries(context.Context) ([]db.OrphanCallOutbox, error) {
	return nil, nil
}

func (m *mockSessionService) GetOrphanOutboxEntry(context.Context, string) (*db.OrphanCallOutbox, error) {
	return nil, nil
}

func (m *mockSessionService) DrainOrphanOutboxEntry(context.Context, string) (bool, error) {
	return false, nil
}

func (m *mockSessionService) RecordOrphanOutboxFailure(context.Context, string, string) (session.OrphanOutboxFailureOutcome, error) {
	return session.OrphanOutboxFailureOutcome{}, nil
}

func newTestApp(sessions session.Service) *App {
	return &App{Sessions: sessions}
}

func TestResolveSession_NewSession(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "", false)
	require.NoError(t, err)
	require.Equal(t, "new-session-id", sess.ID)
	require.Len(t, mock.created, 1)
}

func TestResolveSession_ContinueByID(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "existing-id", Title: "Old session"},
		},
	}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "existing-id", false)
	require.NoError(t, err)
	require.Equal(t, "existing-id", sess.ID)
	require.Equal(t, "Old session", sess.Title)
	require.Empty(t, mock.created)
}

// Fork patch: was "expect error when id not found". The semantic changed to
// get-or-create — see internal/app/app.go's resolveSession.
func TestResolveSession_ContinueByID_NotFound_CreatesNew(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "pr-42", false)
	require.NoError(t, err)
	require.Equal(t, "pr-42", sess.ID, "session must use the caller-supplied id verbatim")
	require.Equal(t, "pr-42", sess.Title, "title defaults to the id for ad-hoc-created sessions")
	require.Len(t, mock.created, 1)
}

func TestResolveSession_ContinueByID_NotFound_CreateError(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_createWithIDErr = fmt.Errorf("UNIQUE constraint failed")
	t.Cleanup(func() { _createWithIDErr = nil })

	_, err := app.resolveSession(t.Context(), "duplicate-id", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not be created")
}

// TestResolveSession_CreationRace_AttachesToWinner is task #605: when two
// `crush run --session <id>` processes both miss the initial Get() for an
// id that has never existed, and then both race CreateWithID's INSERT,
// SQLite's UNIQUE/PRIMARY KEY constraint on sessions.id lets exactly one
// of them win. Before the fix, the loser's raw driver error ("constraint
// failed: UNIQUE constraint failed: sessions.id (1555)", captured verbatim
// from the dynamic gate report at docs/reviews/2026-08-20-dynamic-release-
// gate.md) propagated straight out of resolveSession as "session %q not
// found and could not be created: %w" — a confusing, non-actionable
// message for an orchestrator parsing stderr, given that the identical
// "session already exists" race (continueSessionID hits an existing row)
// produces a clean SessionLockBusyError-driven "session busy" message a
// few lines above in the same switch case.
//
// The fix re-Gets once after a sessions.id constraint violation: by the
// time our own INSERT was rejected, the winner's INSERT has necessarily
// already committed, so the row is there to attach to. This test proves
// the loser now transparently attaches to the winner's row (title
// "winner-title", not the loser's own title) and returns no error at all
// from resolveSession — mirroring TestResolveSession_ContinueByID's
// existing-session path exactly. (The actual mutual-exclusion busy
// rejection then happens one layer up, at the pre-existing OS-level
// session lock in internal/agent/agent_run.go, once both processes try to
// Run() against the now-shared session id — unit-tested there already and
// out of scope for this resolveSession-focused test.)
func TestResolveSession_CreationRace_AttachesToWinner(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_createWithIDErr = fmt.Errorf("constraint failed: UNIQUE constraint failed: sessions.id (1555)")
	_createWithIDErrLandsRow = true
	t.Cleanup(func() {
		_createWithIDErr = nil
		_createWithIDErrLandsRow = false
	})

	sess, err := app.resolveSession(t.Context(), "create-race-id", false)
	require.NoError(t, err, "the loser of a first-ever-creation race must attach to the winner's row, not surface the raw constraint error")
	require.Equal(t, "create-race-id", sess.ID)
	require.Equal(t, "winner-title", sess.Title, "must return the WINNER's row, not synthesize a local one")
}

// TestResolveSession_CreationRace_ChildSessionStillRejected proves the
// re-Get path does not bypass the pre-existing "cannot continue a child
// session" guard: if the row that won the race happens to be a child
// session (imaginable if a session id collides with an internal
// task-session id), attaching to it must still fail the same way the
// existing-session branch already does a few lines above.
func TestResolveSession_CreationRace_ChildSessionStillRejected(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_createWithIDErr = fmt.Errorf("constraint failed: UNIQUE constraint failed: sessions.id (1555)")
	_createWithIDErrLandsRow = true
	t.Cleanup(func() {
		_createWithIDErr = nil
		_createWithIDErrLandsRow = false
	})
	// Overwrite what CreateWithID will plant so the "winner" row looks
	// like a child session instead of a top-level one.
	mock.sessions = append(mock.sessions, session.Session{ID: "create-race-id", ParentSessionID: "some-parent"})

	_, err := app.resolveSession(t.Context(), "create-race-id", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue a child session")
}

// TestResolveSession_CreationRace_OtherConstraintNotSwallowed is the
// revert-check's negative case and the task's explicit "don't swallow the
// wrong error category" requirement: a UNIQUE/constraint violation on some
// OTHER table+column (not sessions.id) must NOT be reclassified as a
// creation race. If isSessionsIDConstraintError's substring match were
// loosened to match any "constraint failed" text, this test would start
// passing incorrectly (silently re-Get-ing and either finding nothing, or
// worse, attaching to an unrelated row) instead of surfacing the real
// error — this is the exact shape of over-broad error classification the
// task warned against repeating.
func TestResolveSession_CreationRace_OtherConstraintNotSwallowed(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_createWithIDErr = fmt.Errorf("constraint failed: UNIQUE constraint failed: messages.id (1555)")
	_createWithIDErrLandsRow = true // even if a row WERE planted, this must not be consulted
	t.Cleanup(func() {
		_createWithIDErr = nil
		_createWithIDErrLandsRow = false
	})

	_, err := app.resolveSession(t.Context(), "unrelated-id", false)
	require.Error(t, err, "a constraint violation on a different table/column must not be silently swallowed")
	require.Contains(t, err.Error(), "could not be created")
	require.Contains(t, err.Error(), "messages.id")
}

// TestResolveSession_CreationRace_ReGetStillFails_ReturnsOriginalError
// covers the fallback branch: if the sessions.id constraint violation
// fires but the immediate re-Get still can't find the row (a vanishingly
// unlikely window — e.g. a concurrent `sessions kill`/delete raced in
// right after — but not impossible), resolveSession must fall through to
// the original wrapped constraint error rather than returning a confusing
// unrelated failure or a zero-value session with no error.
func TestResolveSession_CreationRace_ReGetStillFails_ReturnsOriginalError(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_createWithIDErr = fmt.Errorf("constraint failed: UNIQUE constraint failed: sessions.id (1555)")
	_createWithIDErrLandsRow = false // constraint fires, but no row is ever planted
	t.Cleanup(func() {
		_createWithIDErr = nil
		_createWithIDErrLandsRow = false
	})

	_, err := app.resolveSession(t.Context(), "ghost-id", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not be created")
	require.Contains(t, err.Error(), "sessions.id")
}

func TestResolveSession_ContinueByID_ChildSession(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "child-id", ParentSessionID: "parent-id", Title: "Child session"},
		},
	}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "child-id", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue a child session")
}

func TestResolveSession_ContinueByID_AgentToolSession(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "msg123$$tool456", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue an agent tool session")
}

func TestResolveSession_Last(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "most-recent", Title: "Latest session"},
			{ID: "older", Title: "Older session"},
		},
	}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "", true)
	require.NoError(t, err)
	require.Equal(t, "most-recent", sess.ID)
	require.Empty(t, mock.created)
}

func TestResolveSession_Last_NoSessions(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sessions found")
}
