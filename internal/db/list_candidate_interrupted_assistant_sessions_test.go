package db

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// candDialectOnce mirrors pagDialectOnce in messages_pagination_test.go: the
// goose dialect is a package-global set once via sync.Once, so these tests
// stay non-parallel and share the guard rather than racing on it.
var candDialectOnce sync.Once

const candFixedCreatedAt = int64(1700000000)

func setupCandidateDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/cand.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	candDialectOnce.Do(func() {
		require.NoError(t, goose.SetDialect("sqlite3"))
	})
	require.NoError(t, goose.Up(conn, "migrations"))
	return ctx, conn
}

func insertCandSession(t *testing.T, ctx context.Context, conn *sql.DB, id, parentID string) {
	t.Helper()
	var parent sql.NullString
	if parentID != "" {
		parent = sql.NullString{String: parentID, Valid: true}
	}
	_, err := conn.ExecContext(ctx,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, parent, id, candFixedCreatedAt, candFixedCreatedAt,
	)
	require.NoError(t, err)
}

// insertCandMessage inserts a message with an explicit created_at (seconds)
// and finished_at (NULL when finished is false). createdAt lets tests force
// same-second ties, the exact scenario ListCandidateInterruptedAssistantSessions
// must rank correctly via its rowid tie-breaker.
func insertCandMessage(t *testing.T, ctx context.Context, conn *sql.DB, id, sessionID, role string, createdAt int64, finished bool) {
	t.Helper()
	var finishedAt sql.NullInt64
	if finished {
		finishedAt = sql.NullInt64{Int64: createdAt * 1000, Valid: true}
	}
	_, err := conn.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, finished_at)
		 VALUES (?, ?, ?, '[]', ?, ?, ?)`,
		id, sessionID, role, createdAt, createdAt, finishedAt,
	)
	require.NoError(t, err)
}

// TestListCandidateInterruptedAssistantSessions_TieBreak proves the required
// correctness nuance from task #774: two assistant messages in the SAME
// session at the IDENTICAL created_at second, where the OLDER one (by
// insertion order / rowid) is unfinished and the NEWER one is finished. The
// query must rank by (created_at DESC, rowid DESC) -- the same tie-break
// idiom as ListMessagesBySession/ListUserMessagesBySession -- and conclude
// the session's last message is the finished one, so it must NOT appear in
// the candidate results.
func TestListCandidateInterruptedAssistantSessions_TieBreak(t *testing.T) {
	ctx, conn := setupCandidateDB(t)
	q := New(conn)

	insertCandSession(t, ctx, conn, "sess-tie", "")
	// Older (lower rowid), unfinished -- would wrongly flag the session if
	// rowid were not used as a tie-breaker within the shared second.
	insertCandMessage(t, ctx, conn, "msg-old-unfinished", "sess-tie", "assistant", candFixedCreatedAt, false)
	// Newer (higher rowid), same created_at second, finished -- this is the
	// session's true last message.
	insertCandMessage(t, ctx, conn, "msg-new-finished", "sess-tie", "assistant", candFixedCreatedAt, true)

	rows, err := q.ListCandidateInterruptedAssistantSessions(ctx)
	require.NoError(t, err)

	for _, r := range rows {
		assert.NotEqual(t, "sess-tie", r.SessionID,
			"session whose true last message (by rowid tie-break) is finished must not be a candidate")
	}
}

// TestListCandidateInterruptedAssistantSessions_TieBreak_ReverseOrder is the
// companion case: the NEWER message (higher rowid) at the same created_at
// second is the UNFINISHED one. The session must be reported as a candidate,
// with message_id pointing at that newer row.
func TestListCandidateInterruptedAssistantSessions_TieBreak_ReverseOrder(t *testing.T) {
	ctx, conn := setupCandidateDB(t)
	q := New(conn)

	insertCandSession(t, ctx, conn, "sess-tie-2", "")
	insertCandMessage(t, ctx, conn, "msg-old-finished", "sess-tie-2", "assistant", candFixedCreatedAt, true)
	insertCandMessage(t, ctx, conn, "msg-new-unfinished", "sess-tie-2", "assistant", candFixedCreatedAt, false)

	rows, err := q.ListCandidateInterruptedAssistantSessions(ctx)
	require.NoError(t, err)

	var found bool
	for _, r := range rows {
		if r.SessionID == "sess-tie-2" {
			found = true
			assert.Equal(t, "msg-new-unfinished", r.MessageID,
				"must surface the newer (higher-rowid) message as the candidate, not the older finished one")
		}
	}
	assert.True(t, found, "session whose true last message is unfinished must be a candidate")
}

// TestListCandidateInterruptedAssistantSessions_OldOrphanSuperseded is the
// non-tied variant of the correctness nuance: an OLD unfinished assistant
// message (different, earlier created_at second) later superseded by a
// NEWER finished message must NOT flag the session.
func TestListCandidateInterruptedAssistantSessions_OldOrphanSuperseded(t *testing.T) {
	ctx, conn := setupCandidateDB(t)
	q := New(conn)

	insertCandSession(t, ctx, conn, "sess-superseded", "")
	insertCandMessage(t, ctx, conn, "msg-old-orphan", "sess-superseded", "assistant", candFixedCreatedAt, false)
	insertCandMessage(t, ctx, conn, "msg-retry-finished", "sess-superseded", "assistant", candFixedCreatedAt+5, true)

	rows, err := q.ListCandidateInterruptedAssistantSessions(ctx)
	require.NoError(t, err)

	for _, r := range rows {
		assert.NotEqual(t, "sess-superseded", r.SessionID,
			"an old unfinished assistant message later superseded by a newer finished one must not flag the session")
	}
}

// TestListCandidateInterruptedAssistantSessions_ParentSessionID verifies
// parent_session_id is surfaced correctly for both top-level and child
// candidate sessions, which app.recoverInterruptedTurns relies on to
// partition its two passes without a second per-candidate lookup.
func TestListCandidateInterruptedAssistantSessions_ParentSessionID(t *testing.T) {
	ctx, conn := setupCandidateDB(t)
	q := New(conn)

	insertCandSession(t, ctx, conn, "parent-1", "")
	insertCandMessage(t, ctx, conn, "msg-parent-orphan", "parent-1", "assistant", candFixedCreatedAt, false)

	insertCandSession(t, ctx, conn, "child-1", "parent-1")
	insertCandMessage(t, ctx, conn, "msg-child-orphan", "child-1", "assistant", candFixedCreatedAt, false)

	rows, err := q.ListCandidateInterruptedAssistantSessions(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byID := make(map[string]ListCandidateInterruptedAssistantSessionsRow, len(rows))
	for _, r := range rows {
		byID[r.SessionID] = r
	}

	require.Contains(t, byID, "parent-1")
	assert.False(t, byID["parent-1"].ParentSessionID.Valid,
		"top-level session must have a NULL/invalid parent_session_id")

	require.Contains(t, byID, "child-1")
	require.True(t, byID["child-1"].ParentSessionID.Valid)
	assert.Equal(t, "parent-1", byID["child-1"].ParentSessionID.String)
}

// TestListCandidateInterruptedAssistantSessions_UserMessageNotFlagged proves
// a session whose last message is a non-assistant role (e.g. user) is never
// a candidate, regardless of finished_at.
func TestListCandidateInterruptedAssistantSessions_UserMessageNotFlagged(t *testing.T) {
	ctx, conn := setupCandidateDB(t)
	q := New(conn)

	insertCandSession(t, ctx, conn, "sess-user-only", "")
	insertCandMessage(t, ctx, conn, "msg-user", "sess-user-only", "user", candFixedCreatedAt, false)

	rows, err := q.ListCandidateInterruptedAssistantSessions(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, "sess-user-only", r.SessionID)
	}
}
