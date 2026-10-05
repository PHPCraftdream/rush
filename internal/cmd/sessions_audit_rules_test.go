package cmd

// The suspicion rules of `rush sessions audit` (phase 2): one fixture per
// rule, driven exactly at the threshold and one occurrence short of it, plus
// the integration, --since and schema-degradation cases that tie them
// together. Everything here goes through the real command and the real
// read-only open, so a rule that starts writing, or that crashes on a row it
// cannot read, fails here rather than on a live database.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sessions/messages half of the Phase-1 fixture schema, reused verbatim so
// every variant below differs from it in exactly one optional capability —
// which is what the rules branch on.
const auditSchemaCore = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	title TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	message_count INTEGER
);
CREATE TABLE messages (
	id TEXT PRIMARY KEY,
	session_id TEXT,
	role TEXT,
	parts TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	notice_kind TEXT DEFAULT ''
);
`

// auditSchemaFixtureNoJobs is a database with no async_jobs table at all: one
// that never ran the async migrations, or one too old for them.
const auditSchemaFixtureNoJobs = auditSchemaCore

// auditSchemaFixtureJobsNoKind drops async_jobs.kind, the one column the
// unfinished-jobs rule is allowed to work without.
const auditSchemaFixtureJobsNoKind = auditSchemaCore + `
CREATE TABLE async_jobs (
	owner_session_id TEXT,
	tool_call_id TEXT,
	state TEXT,
	child_session_id TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	PRIMARY KEY (owner_session_id, tool_call_id)
);
`

// auditMessageFixture is one messages row a rules test seeds.
type auditMessageFixture struct {
	SessionID string
	Parts     string
	// NoticeKind is written whenever the schema has the column, '' otherwise.
	NoticeKind string
	// CreatedAt overrides "now" so a --since test can put a row outside the
	// window.
	CreatedAt int64
}

// auditJobFixture is one async_jobs row a rules test seeds.
type auditJobFixture struct {
	OwnerSessionID string
	ToolCallID     string
	Kind           string
	State          string
}

// auditRulesFixture is the whole database one rules test asserts against: the
// schema variant to create, the messages to insert and the jobs to insert. A
// field left empty is the test asserting a rule is silent about it.
type auditRulesFixture struct {
	Schema   string
	Messages []auditMessageFixture
	Jobs     []auditJobFixture
}

// seedAuditRulesDB writes a fixture database into dataDir and returns its
// path. Unlike seedAuditFixtureDB it seeds no rows of its own: a rule test has
// to know the only evidence in the database is the evidence it inserted.
//
// What it inserts follows what the schema actually has, discovered through the
// same PRAGMA table_info the audit itself uses — a fixture that pretended to
// write a column its schema lacks would fail to seed rather than test the rule.
func seedAuditRulesDB(t *testing.T, dataDir string, fixture auditRulesFixture) string {
	t.Helper()

	dbPath := filepath.Join(dataDir, "rush.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})

	schema := fixture.Schema
	if schema == "" {
		schema = auditSchemaFixture
	}
	_, err = db.ExecContext(context.Background(), schema)
	require.NoError(t, err)

	ctx := context.Background()
	messageColumns, err := auditTableColumns(ctx, db, "messages")
	require.NoError(t, err)
	hasNoticeKind := messageColumns["notice_kind"]
	jobColumns, err := auditTableColumns(ctx, db, "async_jobs")
	require.NoError(t, err)

	// A session row per distinct session id in the messages: auditFindings is
	// handed only the ids the selection produced, so a row belongs to a
	// session only if that session exists.
	now := timeNowUnix()
	for _, id := range auditDistinctSessions(fixture.Messages) {
		_, err = db.ExecContext(context.Background(), `INSERT INTO sessions (id, title, created_at, updated_at, message_count) VALUES
			(?, ?, ?, ?, 0)`, id, id+" title", now, now)
		require.NoError(t, err)
	}

	for i, message := range fixture.Messages {
		createdAt := message.CreatedAt
		if createdAt == 0 {
			createdAt = now
		}
		// The id only has to be unique and stable, which is what lets a
		// --since test seed the same logical message at two timestamps.
		id := fmt.Sprintf("m%d-%s", i, message.SessionID)
		if hasNoticeKind {
			_, err = db.ExecContext(context.Background(), `INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, notice_kind)
				VALUES (?, ?, 'assistant', ?, ?, ?, ?)`,
				id, message.SessionID, message.Parts, createdAt, createdAt, message.NoticeKind)
		} else {
			_, err = db.ExecContext(context.Background(), `INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
				VALUES (?, ?, 'assistant', ?, ?, ?)`,
				id, message.SessionID, message.Parts, createdAt, createdAt)
		}
		require.NoError(t, err)
	}

	for i, job := range fixture.Jobs {
		callID := fmt.Sprintf("call-%d", i)
		if jobColumns["kind"] {
			_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs (owner_session_id, tool_call_id, kind, state, child_session_id, created_at, updated_at)
				VALUES (?, ?, ?, ?, NULL, ?, ?)`,
				job.OwnerSessionID, callID, job.Kind, job.State, now, now)
		} else {
			_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs (owner_session_id, tool_call_id, state, child_session_id, created_at, updated_at)
				VALUES (?, ?, ?, NULL, ?, ?)`,
				job.OwnerSessionID, callID, job.State, now, now)
		}
		require.NoError(t, err)
	}

	// A clean close leaves no journal behind, so the audit runs against the
	// same sidecar-free file the read-only guarantee is about.
	require.NoError(t, db.Close())
	closed = true
	return dbPath
}

func auditDistinctSessions(messages []auditMessageFixture) []string {
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

// auditToolCall renders the parts column of a message holding one tool call,
// the way internal/message/parts_codec.go writes it.
func auditToolCall(t *testing.T, name string, fields map[string]any) string {
	t.Helper()
	input, err := json.Marshal(fields)
	require.NoError(t, err)
	return auditPartsJSON(t, map[string]any{
		"type": "tool_call",
		"data": map[string]any{"name": name, "input": string(input)},
	})
}

// auditToolResult renders the parts column of a message holding one tool
// result, including the failed ones the tool_errors rule counts.
func auditToolResult(t *testing.T, content string, isError bool) string {
	t.Helper()
	return auditPartsJSON(t, map[string]any{
		"type": "tool_result",
		"data": map[string]any{"name": "bash", "content": content, "is_error": isError},
	})
}

func auditPartsJSON(t *testing.T, parts ...map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(parts)
	require.NoError(t, err)
	return string(encoded)
}

// auditFindingLine is the exact text line the report prints for a finding.
func auditFindingLine(rule, sessionID, detail string, count int) string {
	return fmt.Sprintf("%s: session %s — %s (x%d)", rule, short(sessionID), detail, count)
}

// auditNoticeMsg is a message that is only a notice, which is a column rather
// than a part.
func auditNoticeMsg(sessionID, kind string) auditMessageFixture {
	return auditMessageFixture{SessionID: sessionID, NoticeKind: kind}
}

// auditRunRulesDB runs the real command over an already seeded data directory,
// once for the text an operator reads and once for the JSON a script reads, and
// returns both.
func auditRunRulesDB(t *testing.T, dataDir string, args ...string) (string, []auditDBReport) {
	t.Helper()

	textArgs := append([]string{"--data-dir", dataDir}, args...)
	stdout, stderr, err := runAuditCmd(t, textArgs...)
	require.NoError(t, err, "stderr: %s", stderr)

	jsonArgs := append([]string{"--data-dir", dataDir}, args...)
	jsonArgs = append(jsonArgs, "--json")
	jsonStdout, _, err := runAuditCmd(t, jsonArgs...)
	require.NoError(t, err)

	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.NotEmpty(t, reports)
	return stdout, reports
}

func auditRunRulesAll(t *testing.T, fixture auditRulesFixture, args ...string) (string, []auditDBReport) {
	t.Helper()
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditRulesDB(t, dataDir, fixture)
	all := append([]string{"--all"}, args...)
	return auditRunRulesDB(t, dataDir, all...)
}

// auditRunRulesSession audits one session of the fixture, so a test can prove
// evidence belonging to another session is never scanned.
func auditRunRulesSession(t *testing.T, fixture auditRulesFixture, session string, args ...string) (string, []auditDBReport) {
	t.Helper()
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditRulesDB(t, dataDir, fixture)
	selection := append([]string{session}, args...)
	return auditRunRulesDB(t, dataDir, selection...)
}

// TestAuditRules_RepeatedView_Boundary: the same file viewed three times is a
// suspicion; twice is not. The count is per file AND per session, so a file
// viewed twice by one session and once by another stays silent.
func TestAuditRules_RepeatedView_Boundary(t *testing.T) {
	const session = "audit-view-1"
	const path = "internal/cmd/sessions_audit.go"

	view := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
	}}
	stdout, reports := auditRunRulesAll(t, view)
	require.Contains(t, stdout, auditFindingLine("repeated-view", session, path, 3),
		"three views of one file must trip repeated-view")
	require.Len(t, auditFindingsForRule(reports, "repeated-view"), 1)

	onceLess := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
	}}
	stdout, _ = auditRunRulesAll(t, onceLess)
	require.NotContains(t, stdout, "repeated-view", "two views of one file is not a suspicion yet")

	// A different session viewing the same file must not add to this one's
	// count.
	split := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		{SessionID: "audit-view-2", Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
	}}
	stdout, _ = auditRunRulesAll(t, split)
	require.NotContains(t, stdout, "repeated-view", "the count is per session and per file")

	// Two different files, three views each: two findings, not one.
	twoFiles := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "a.go"})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "a.go"})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "a.go"})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "b.go"})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "b.go"})},
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "b.go"})},
	}}
	_, reports = auditRunRulesAll(t, twoFiles)
	require.Len(t, auditFindingsForRule(reports, "repeated-view"), 2, "each file repeats on its own")
}

// TestAuditRules_RepeatedCommand_Boundary: three normalized bash commands is a
// suspicion. Normalization folds every run of digits to "N", so commands that
// differ only in their numbers are one repeated command.
func TestAuditRules_RepeatedCommand_Boundary(t *testing.T) {
	const session = "audit-cmd-1"

	bash := func(command string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": command}),
		}
	}

	atThreshold := auditRulesFixture{Messages: []auditMessageFixture{
		bash("go test ./internal/... 2"), bash("go test ./internal/... 17"), bash("go test ./internal/... 404"),
	}}
	stdout, _ := auditRunRulesAll(t, atThreshold)
	require.Contains(t, stdout, auditFindingLine("repeated_command", session, "go test ./internal/... N", 3),
		"three commands that differ only in their numbers are one repeated command")

	onceLess := auditRulesFixture{Messages: []auditMessageFixture{
		bash("go test ./internal/... 2"), bash("go test ./internal/... 17"),
	}}
	stdout, _ = auditRunRulesAll(t, onceLess)
	require.NotContains(t, stdout, "repeated_command", "two is below the threshold")

	// Different commands stay different commands, however similar they look.
	distinct := auditRulesFixture{Messages: []auditMessageFixture{
		bash("go test ./internal/... 2"), bash("go vet ./internal/... 2"), bash("go build ./internal/... 2"),
	}}
	stdout, _ = auditRunRulesAll(t, distinct)
	require.NotContains(t, stdout, "repeated_command", "a number is not the only difference")

	// A command that waits and then works is ordinary work for this rule, and
	// wait_only's evidence is not counted twice.
	works := auditRulesFixture{Messages: []auditMessageFixture{
		bash("sleep 5 && go test ./..."), bash("sleep 5 && go test ./..."), bash("sleep 5 && go test ./..."),
	}}
	stdout, _ = auditRunRulesAll(t, works)
	require.Contains(t, stdout, auditFindingLine("repeated_command", session, "sleep N && go test ./...", 3))
	require.NotContains(t, stdout, "wait_only", "a command that also does work is not wait-only")

	// A wait-only command is wait_only's evidence alone.
	waits := auditRulesFixture{Messages: []auditMessageFixture{
		bash("sleep 5"), bash("sleep 5"), bash("sleep 5"),
	}}
	stdout, _ = auditRunRulesAll(t, waits)
	require.Contains(t, stdout, auditFindingLine("wait_only", session, "sleep N", 3))
	require.NotContains(t, stdout, "repeated_command",
		"wait-only evidence is not double-counted as a repeated command")

	// Input that is not JSON must not take the audit down: the broken row is
	// skipped silently and the healthy ones beside it are still counted.
	broken := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditPartsJSON(t, map[string]any{
			"type": "tool_call",
			"data": map[string]any{"name": "bash", "input": "not json at all"},
		})},
		bash("go test ./..."), bash("go test ./..."), bash("go test ./..."),
	}}
	stdout, _ = auditRunRulesAll(t, broken)
	require.Contains(t, stdout, auditFindingLine("repeated_command", session, "go test ./...", 3),
		"a broken row costs nothing but itself")
}

// TestAuditRules_WaitOnly_Boundary: three turns of nothing but waiting is a
// session polling in a loop. Every shape of wait counts towards the same
// total, because the rule is about how often the session waited, not with what.
func TestAuditRules_WaitOnly_Boundary(t *testing.T) {
	const session = "audit-wait-1"

	wait := func(command string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": command}),
		}
	}

	atThreshold := auditRulesFixture{Messages: []auditMessageFixture{
		wait("sleep 5"), wait("sleep 30"), wait("sleep 120"),
	}}
	stdout, _ := auditRunRulesAll(t, atThreshold)
	require.Contains(t, stdout, auditFindingLine("wait_only", session, "sleep N", 3),
		"three sleeps is a polling loop")

	onceLess := auditRulesFixture{Messages: []auditMessageFixture{
		wait("sleep 5"), wait("sleep 30"),
	}}
	stdout, _ = auditRunRulesAll(t, onceLess)
	require.NotContains(t, stdout, "wait_only", "two waits is not a loop")

	// Every wait shape the patterns accept: one finding with the total, not
	// one per shape.
	shapes := auditRulesFixture{Messages: []auditMessageFixture{
		wait("wait"), wait("wait 1234"), wait("sleep 0.5"),
		wait("timeout 60"), wait("job_output 12 --wait"), wait("poll.sh --interval=5"),
	}}
	stdout, reports := auditRunRulesAll(t, shapes)
	require.Len(t, auditFindingsForRule(reports, "wait_only"), 1)
	require.Contains(t, stdout, "(x6)", "all six waits are one loop")

	// Not wait-only: work that happens to mention waiting.
	notWait := auditRulesFixture{Messages: []auditMessageFixture{
		wait("echo waiting && ls"), wait("grep sleep main.go"), wait("timeout 60 make build && git status"),
	}}
	stdout, _ = auditRunRulesAll(t, notWait)
	require.NotContains(t, stdout, "wait_only", "a command that does work is not wait-only")
}

// TestAuditRules_ToolErrors_Boundary: three failed tool results of one class is
// a suspicion. The class folds numbers to "N" and keeps the first 60 bytes, so
// the same textual failure raised with different arguments is one class.
func TestAuditRules_ToolErrors_Boundary(t *testing.T) {
	const session = "audit-err-1"

	failing := func(number int) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts: auditToolResult(t,
				fmt.Sprintf("cannot unmarshal number %d into Go value of type X", number), true),
		}
	}
	succeeded := auditMessageFixture{
		SessionID: session,
		Parts:     auditToolResult(t, "all good", false),
	}

	// Short enough that the whole normalized sentence is the class.
	const class = "cannot unmarshal number N into Go value of type X"
	atThreshold := auditRulesFixture{Messages: []auditMessageFixture{
		failing(12), failing(7), failing(404), succeeded,
	}}
	stdout, reports := auditRunRulesAll(t, atThreshold)
	require.Contains(t, stdout, auditFindingLine("tool_errors", session, class, 3),
		"three failures of one class is a suspicion")
	require.Len(t, auditFindingsForRule(reports, "tool_errors"), 1, "one class, one finding")

	onceLess := auditRulesFixture{Messages: []auditMessageFixture{failing(12), failing(7)}}
	stdout, _ = auditRunRulesAll(t, onceLess)
	require.NotContains(t, stdout, "tool_errors", "two failures is below the threshold")

	// A successful result is never an error class, however often it repeats.
	successes := auditRulesFixture{Messages: []auditMessageFixture{succeeded, succeeded, succeeded}}
	stdout, _ = auditRunRulesAll(t, successes)
	require.NotContains(t, stdout, "tool_errors", "a successful result is not an error")

	// Two classes at once, each firing on its own, and the class capped at 60
	// bytes.
	other := func(number int) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts: auditToolResult(t,
				fmt.Sprintf("failed to read %s: permission refused (errno %d)", "/etc/hosts", number), true),
		}
	}
	classes := auditRulesFixture{Messages: []auditMessageFixture{
		failing(1), failing(22), failing(333), other(1), other(22), other(33),
	}}
	_, reports = auditRunRulesAll(t, classes)
	require.Len(t, auditFindingsForRule(reports, "tool_errors"), 2, "each class is its own finding")

	tailed := func(number int) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts: auditToolResult(t,
				fmt.Sprintf("cannot unmarshal number %d into Go value of type X: step %d of the batch", number, number), true),
		}
	}
	longClass := auditRulesFixture{Messages: []auditMessageFixture{
		tailed(1), tailed(2), tailed(3),
	}}
	_, reports = auditRunRulesAll(t, longClass)
	capped := auditFindingsForRule(reports, "tool_errors")[0].Detail
	require.Len(t, capped, 60, "the class is the first 60 bytes of the normalized error")
	require.Contains(t, capped, "number N", "the class folds numbers to N")
	require.NotContains(t, capped, "batch", "the class stops at the 60-byte cap")
}

// TestAuditRules_Notices_Boundary: one supervision notice is already a
// suspicion — a session that needs supervising at all has left the rails. The
// two counted kinds are counted separately, and a kind the rule does not know
// is ignored.
func TestAuditRules_Notices_Boundary(t *testing.T) {
	const session = "audit-notice-1"

	atThreshold := auditRulesFixture{Messages: []auditMessageFixture{auditNoticeMsg(session, "supervision")}}
	stdout, reports := auditRunRulesAll(t, atThreshold)
	require.Contains(t, stdout, auditFindingLine("notices", session, "supervision", 1),
		"the threshold is one notice")
	require.Len(t, auditFindingsForRule(reports, "notices"), 1)

	onceLess := auditRulesFixture{Messages: []auditMessageFixture{auditNoticeMsg(session, "")}}
	stdout, _ = auditRunRulesAll(t, onceLess)
	require.NotContains(t, stdout, "notices", "an ordinary message is not a notice")

	twoKinds := auditRulesFixture{Messages: []auditMessageFixture{
		auditNoticeMsg(session, "supervision"), auditNoticeMsg(session, "supervision"),
		auditNoticeMsg(session, "wake_failed"), auditNoticeMsg(session, "timeout_wake_only"),
	}}
	stdout, reports = auditRunRulesAll(t, twoKinds)
	require.Len(t, auditFindingsForRule(reports, "notices"), 2, "each counted kind is its own finding")
	require.Contains(t, stdout, auditFindingLine("notices", session, "supervision", 2))
	require.Contains(t, stdout, auditFindingLine("notices", session, "wake_failed", 1))
	require.NotContains(t, stdout, "timeout_wake_only", "a kind the rule does not count is silent")
}

// TestAuditRules_UnfinishedJobs_Boundary: one still-running delegation is a
// suspicion (a session that left a job hanging), and only running/interrupted
// states count — a finished job is evidence the delegation came back.
func TestAuditRules_UnfinishedJobs_Boundary(t *testing.T) {
	const session = "audit-job-1"
	ordinary := auditMessageFixture{
		SessionID: session,
		Parts:     auditToolCall(t, "view", map[string]any{"file_path": "a.go"}),
	}

	atThreshold := auditRulesFixture{
		Messages: []auditMessageFixture{ordinary},
		Jobs:     []auditJobFixture{{OwnerSessionID: session, ToolCallID: "call-1", Kind: "command", State: "running"}},
	}
	stdout, reports := auditRunRulesAll(t, atThreshold)
	require.Contains(t, stdout, auditFindingLine("unfinished_jobs", session, "call-0:command", 1),
		"one running job is already a suspicion")
	require.Len(t, auditFindingsForRule(reports, "unfinished_jobs"), 1)

	// Terminal states are not unfinished, however many there are.
	finished := auditRulesFixture{
		Messages: []auditMessageFixture{ordinary},
		Jobs: []auditJobFixture{
			{OwnerSessionID: session, ToolCallID: "call-1", Kind: "command", State: "completed"},
			{OwnerSessionID: session, ToolCallID: "call-2", Kind: "agent", State: "failed"},
			{OwnerSessionID: session, ToolCallID: "call-3", Kind: "fetch", State: "timed_out"},
		},
	}
	stdout, _ = auditRunRulesAll(t, finished)
	require.NotContains(t, stdout, "unfinished_jobs", "a settled job is not unfinished")

	// interrupted counts too, and both jobs are named in the detail.
	interrupted := auditRulesFixture{
		Messages: []auditMessageFixture{ordinary},
		Jobs: []auditJobFixture{
			{OwnerSessionID: session, ToolCallID: "call-1", Kind: "command", State: "running"},
			{OwnerSessionID: session, ToolCallID: "call-2", Kind: "agent", State: "interrupted"},
		},
	}
	stdout, reports = auditRunRulesAll(t, interrupted)
	require.Contains(t, stdout, auditFindingLine("unfinished_jobs", session, "call-0:command, call-1:agent", 2),
		"an interrupted delegation is just as unfinished")
	require.Len(t, auditFindingsForRule(reports, "unfinished_jobs"), 1)

	// A job owned by a session that was not selected is never scanned.
	elsewhere := auditRulesFixture{
		Messages: []auditMessageFixture{ordinary},
		Jobs:     []auditJobFixture{{OwnerSessionID: "audit-job-other", ToolCallID: "call-1", Kind: "command", State: "running"}},
	}
	stdout, _ = auditRunRulesSession(t, elsewhere, session)
	require.Contains(t, stdout, session, "the selected session was audited")
	require.NotContains(t, stdout, "unfinished_jobs", "another session's job is not this session's")
}

// TestAuditRules_SchemaDegradation_IsSilent: an older database is audited with
// the reduced set, never failed on. Without async_jobs the rule is simply
// absent; without the kind column the detail drops it and keeps working.
func TestAuditRules_SchemaDegradation_IsSilent(t *testing.T) {
	const session = "audit-schema-1"

	// No async_jobs table at all: the messages are still audited, the
	// unfinished-jobs rule is silently absent.
	noJobs := auditRulesFixture{
		Schema: auditSchemaFixtureNoJobs,
		Messages: []auditMessageFixture{
			auditNoticeMsg(session, "supervision"),
			{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "a.go"})},
			{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "a.go"})},
		},
	}
	stdout, reports := auditRunRulesAll(t, noJobs)
	require.NotContains(t, stdout, "unfinished_jobs", "no table, no rule")
	require.Contains(t, stdout, auditFindingLine("notices", session, "supervision", 1),
		"the rest of the audit still runs")
	require.Empty(t, reports[0].SchemaIssues, "a missing optional table is not a schema issue")

	// async_jobs without the kind column: the rule still fires, naming the
	// tool call alone.
	noKind := auditRulesFixture{
		Schema: auditSchemaFixtureJobsNoKind,
		Messages: []auditMessageFixture{{
			SessionID: session,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": "a.go"}),
		}},
		Jobs: []auditJobFixture{{OwnerSessionID: session, ToolCallID: "call-1", State: "running"}},
	}
	stdout, reports = auditRunRulesAll(t, noKind)
	require.Contains(t, stdout, auditFindingLine("unfinished_jobs", session, "call-0", 1),
		"without the kind column the detail is the tool call alone")
	require.Len(t, auditFindingsForRule(reports, "unfinished_jobs"), 1)
	require.Empty(t, reports[0].SchemaIssues)
}

// TestAuditRules_Integration: every rule at once, in one fixture, in text and
// in JSON. This is the case an operator would actually run into — a session
// that re-read the same files, re-ran the same command, polled in a sleep loop,
// hit the same provider error three times, needed supervising and left a
// delegation running — plus a quiet session that must produce nothing.
func TestAuditRules_Integration(t *testing.T) {
	const busy = "audit-busy-1"
	const quiet = "audit-idle-1"

	view := func(path string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: busy,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": path}),
		}
	}
	bash := func(command string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: busy,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": command}),
		}
	}
	failing := func(number int) auditMessageFixture {
		return auditMessageFixture{
			SessionID: busy,
			Parts: auditToolResult(t,
				fmt.Sprintf("cannot unmarshal number %d into Go value of type X", number), true),
		}
	}

	fixture := auditRulesFixture{
		Messages: []auditMessageFixture{
			// Two files, each read three times: two repeated-view findings.
			view("internal/cmd/sessions_audit.go"), view("internal/cmd/sessions_audit.go"),
			view("internal/cmd/sessions_audit.go"),
			view("internal/cmd/sessions_audit_report.go"), view("internal/cmd/sessions_audit_report.go"),
			view("internal/cmd/sessions_audit_report.go"),
			// One normalized command, three times.
			bash("go build ./internal/... 1"), bash("go build ./internal/... 2"), bash("go build ./internal/... 3"),
			// Three waits, of three different shapes.
			bash("sleep 5"), bash("sleep 30"), bash("wait 4711"),
			// Three failures of one class, the numbers differing.
			failing(1), failing(22), failing(333),
			// Two supervision notices.
			auditNoticeMsg(busy, "supervision"), auditNoticeMsg(busy, "supervision"),
			// A quiet session's ordinary traffic: no finding.
			{SessionID: quiet, Parts: auditToolCall(t, "view", map[string]any{"file_path": "README.md"})},
			auditNoticeMsg(quiet, ""),
		},
		Jobs: []auditJobFixture{
			{OwnerSessionID: busy, ToolCallID: "call-1", Kind: "command", State: "running"},
		},
	}

	stdout, reports := auditRunRulesAll(t, fixture)

	for _, line := range []string{
		auditFindingLine("repeated-view", busy, "internal/cmd/sessions_audit.go", 3),
		auditFindingLine("repeated-view", busy, "internal/cmd/sessions_audit_report.go", 3),
		auditFindingLine("repeated_command", busy, "go build ./internal/... N", 3),
		auditFindingLine("wait_only", busy, "sleep N", 3),
		auditFindingLine("tool_errors", busy, "cannot unmarshal number N into Go value of type X", 3),
		auditFindingLine("notices", busy, "supervision", 2),
		auditFindingLine("unfinished_jobs", busy, "call-0:command", 1),
	} {
		require.Contains(t, stdout, line, "the report must show this finding")
	}

	require.Len(t, reports[0].Findings, 7, "seven findings across the six rules")
	for _, finding := range reports[0].Findings {
		require.Equal(t, busy, finding.SessionID, "the quiet session produced no findings")
	}

	// The JSON carries the same numbers a script would read.
	views := auditFindingsForRule(reports, "repeated-view")
	require.Len(t, views, 2)
	require.Equal(t, 3, views[0].Count)
	require.Equal(t, "internal/cmd/sessions_audit.go", views[0].Detail)
	require.Equal(t, 3, views[1].Count)
	require.Equal(t, "internal/cmd/sessions_audit_report.go", views[1].Detail)
	require.Equal(t, 1, auditFindingsForRule(reports, "unfinished_jobs")[0].Count)
	require.Equal(t, "call-0:command", auditFindingsForRule(reports, "unfinished_jobs")[0].Detail)
}

// TestAuditRules_SinceWindow: --since is a window over the message's own
// timestamp. An old repeat is outside it and does not count, so a session that
// behaved itself recently is not accused of what it did last week.
func TestAuditRules_SinceWindow(t *testing.T) {
	const session = "audit-since-1"
	const path = "internal/cmd/sessions_audit.go"

	old := time.Now().Add(-48 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()
	view := func(createdAt int64) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": path}),
			CreatedAt: createdAt,
		}
	}

	// All three views are older than the window: the rule must not fire.
	allOld := auditRulesFixture{Messages: []auditMessageFixture{view(old), view(old + 1), view(old + 2)}}
	stdout, reports := auditRunRulesAll(t, allOld, "--since", "24h")
	require.NotContains(t, stdout, "repeated-view", "a message outside the window is not counted")
	require.Empty(t, reports[0].Findings, "nothing happened inside the window")

	// The same three views inside the window: it fires.
	allRecent := auditRulesFixture{Messages: []auditMessageFixture{view(recent), view(recent + 1), view(recent + 2)}}
	stdout, _ = auditRunRulesAll(t, allRecent, "--since", "24h")
	require.Contains(t, stdout, auditFindingLine("repeated-view", session, path, 3))

	// Two old, one new — only the new one counts, which is below the threshold.
	mixed := auditRulesFixture{Messages: []auditMessageFixture{view(old), view(old + 1), view(recent)}}
	stdout, _ = auditRunRulesAll(t, mixed, "--since", "24h")
	require.NotContains(t, stdout, "repeated-view",
		"only messages inside the window contribute to the count")
}

// TestAuditRules_BrokenRowsDoNotStopTheAudit: a message whose parts are not
// JSON at all, one whose parts are a JSON object rather than an array, and one
// whose part data is not an object, are all skipped without taking the audit
// with them.
func TestAuditRules_BrokenRowsDoNotStopTheAudit(t *testing.T) {
	const session = "audit-broken-1"
	const path = "a.go"

	fixture := auditRulesFixture{
		Messages: []auditMessageFixture{
			{SessionID: session, Parts: "this is not json"},
			{SessionID: session, Parts: `{"type":"tool_call"}`},
			{SessionID: session, Parts: `[{"type":"tool_call","data":"not an object"}]`},
			{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
			{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
			{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": path})},
		},
	}

	stdout, reports := auditRunRulesAll(t, fixture)
	require.Contains(t, stdout, auditFindingLine("repeated-view", session, path, 3),
		"the healthy rows beside the broken ones are still counted")
	require.Empty(t, reports[0].Notices, "a broken row is not a notice")
}

// auditFindingsForRule filters a parsed report's findings down to one rule, so
// a test can assert on that rule's evidence and counts alone.
func auditFindingsForRule(reports []auditDBReport, rule string) []auditFinding {
	var findings []auditFinding
	for _, report := range reports {
		for _, finding := range report.Findings {
			if finding.Rule == rule {
				findings = append(findings, finding)
			}
		}
	}
	return findings
}
