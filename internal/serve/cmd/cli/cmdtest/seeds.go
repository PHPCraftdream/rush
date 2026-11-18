package cmdtest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

const CyrillicEnvelope = `{"session_id":"s1-review","exit_reason":"end_turn","final_text":"Проверено: замечаний нет.","review":"Замечаний нет, изменения точны.","review_verdict":"pass","usage":{"delta_tokens":120,"delta_cost_usd":0.02},"duration_ms":65000}`

// SeedCmdBatchDelegation reserves and announces a live running batch
// delegation (kind=batch root with tool call + input hash, one running task
// child linked to childID) for owner on the given host, and returns the root
// and child node ids. Copy of seedCmdBatchDelegation
// (sessions_batch_fixture_helpers_test.go); lane K3 deletes the original.
func SeedCmdBatchDelegation(t *testing.T, conn *sql.DB, owner, host, callID, childID string) (rootID, childNodeID string) {
	t.Helper()
	ctx := context.Background()
	bs := session.NewBatchStore(conn)
	res, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "cmd-fixture-runner", ToolCallID: callID,
		InputHash: "cmd-fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
			ChildSessionID: sql.NullString{String: childID, Valid: childID != ""},
			State:          "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	rootID, childNodeID = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := bs.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, rootID,
		message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "fixture delegation started"}},
			// A started delegation is a background-job announcement: the same
			// shape the production announce path persists.
			BackgroundJobNotice: true,
		})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return rootID, childNodeID
}

// AuditMessageFixture is one messages row a rules test seeds.
type AuditMessageFixture struct {
	SessionID string
	Parts     string
	// NoticeKind is written whenever the schema has the column, '' otherwise.
	NoticeKind string
	// CreatedAt overrides "now" so a --since test can put a row outside the
	// window.
	CreatedAt int64
}

// AuditJobFixture is one batch root a rules fixture can seed (kept for the
// migrated-DB seeder below).
type AuditJobFixture struct {
	OwnerSessionID string
	ToolCallID     string
	Kind           string
	State          string
}

// AuditRulesFixture is the whole database one rules test asserts against.
type AuditRulesFixture struct {
	Messages []AuditMessageFixture
	Jobs     []AuditJobFixture
}

// auditDistinctSessions returns the distinct session ids of a fixture's
// messages, in first-seen order.
func auditDistinctSessions(messages []AuditMessageFixture) []string {
	seen := map[string]bool{}
	var ids []string
	for _, message := range messages {
		if !seen[message.SessionID] {
			seen[message.SessionID] = true
			ids = append(ids, message.SessionID)
		}
	}
	return ids
}

// timeNowUnix keeps the fixture's timestamps in Unix seconds, the unit the
// real schema stores.
func timeNowUnix() int64 { return time.Now().Unix() }

// SeedAuditRulesMigratedDB writes a fixture's messages and batch roots into
// the migrated schema via db.Connect (copy of seedAuditRulesMigratedDB,
// sessions_fixture_db_test.go); lane K3 deletes the original.
func SeedAuditRulesMigratedDB(t *testing.T, dataDir string, fixture AuditRulesFixture) {
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

// SeedCmdBatchRootFor reserves one bare pending batch root for owner on the
// given host and returns the root id (copy of seedCmdBatchRootFor; lane K3
// deletes the original).
func SeedCmdBatchRootFor(t *testing.T, a *app.App, owner, host, callID string) string {
	t.Helper()
	res, err := session.NewBatchStore(a.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "cmd-fixture-runner",
		ToolCallID: callID, InputHash: "cmd-fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	return res.Snapshot.Root.RootID
}
