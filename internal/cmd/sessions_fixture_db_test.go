package cmd

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func seedAuditRulesMigratedDB(t *testing.T, dataDir string, fixture auditRulesFixture) {
	t.Helper()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, db.ReleaseConn(conn))
		}
	})
	ctx := t.Context()
	now := timeNowUnix()
	sessionIDs := auditDistinctSessions(fixture.Messages)
	seen := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		seen[id] = true
	}
	for _, job := range fixture.Jobs {
		if !seen[job.OwnerSessionID] {
			sessionIDs = append(sessionIDs, job.OwnerSessionID)
			seen[job.OwnerSessionID] = true
		}
	}
	for _, id := range sessionIDs {
		_, err = conn.ExecContext(ctx, `INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`, id, id+" title", now, now)
		require.NoError(t, err)
	}
	for i, msg := range fixture.Messages {
		createdAt := msg.CreatedAt
		if createdAt == 0 {
			createdAt = now
		}
		id := fmt.Sprintf("m%d-%s", i, msg.SessionID)
		_, err = conn.ExecContext(ctx, `INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, notice_kind) VALUES (?, ?, 'assistant', ?, ?, ?, ?)`, id, msg.SessionID, msg.Parts, createdAt, createdAt, msg.NoticeKind)
		require.NoError(t, err)
	}
	store := session.NewBatchStore(conn)
	for i, job := range fixture.Jobs {
		callID := fmt.Sprintf("call-%d", i)
		_, err = store.Reserve(ctx, session.BatchReservation{
			Owner: job.OwnerSessionID, Host: "audit-fixture-host", RunnerID: "audit-fixture-runner",
			ToolCallID: callID, InputHash: "audit-fixture-hash-" + callID, Sequence: 1,
			Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: job.State, ControlState: "none",
			}},
		})
		require.NoError(t, err)
	}
	require.NoError(t, db.ReleaseConn(conn))
	closed = true
}

func newSessionFactsTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	return conn, db.New(conn)
}
