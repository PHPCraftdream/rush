package cmd

// The flag surface of `rush sessions audit`: what each flag adds to the scan.
// --worktrees is the only one that widens the scan beyond a single database,
// and it does so through the registered project list — so the registry has
// to be isolated rather than trusted. --since narrows which messages the
// rules count without narrowing what the report describes, and --json is the
// shape a script reads, so it has to carry the findings themselves.
//
// The --since window itself (old/recent/mixed evidence against the
// repeated-view rule) is already proved by TestAuditRules_SinceWindow in
// sessions_audit_rules_test.go; what is left over is that the window bounds
// the analysis only, and that it is not specific to one rule.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/projects"
	"github.com/stretchr/testify/require"
)

// TestSessionsAudit_HelpNoLongerDefersTheRules: the help must not claim the
// rules themselves are still to come — they are implemented, in
// sessions_audit_findings.go with the readers in sessions_audit_rules.go,
// and the thresholds are the const block the help describes. A stale "later
// phase" sentence here is exactly the kind of text that sends a reader
// looking for work that is already done.
//
// Revert-check: reintroducing the phase-1 wording into Long fails this.
func TestSessionsAudit_HelpNoLongerDefersTheRules(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsAuditCmd.Long)
	require.Contains(t, long, "Every rule is implemented and every threshold is declared once as a constant")
	for _, stale := range []string{"Phase 1", "phase 1", "later phase", "lands in a later", "are yet to come"} {
		require.NotContains(t, long, stale,
			"the help must not claim the rules still arrive in a later phase")
	}
}

// TestSessionsAudit_WorktreesScansEveryRegisteredDatabase: --worktrees audits
// the linked worktrees' databases alongside the current one — one report
// object per database — and still writes to neither of them.
//
// The registry is isolated the same way the rest of this package isolates
// config: projectsFilePath() is the projects.json that sits next to
// config.GlobalConfigData(), which honours RUSH_GLOBAL_DATA/XDG_DATA_HOME —
// exactly the two variables isolateConfigEnvForTests points at a throwaway
// directory. So this test registers its own second checkout with
// projects.Register and never sees (or disturbs) the operator's real list.
//
// Revert-check: making auditWorktreeDataDirs return nil, or dropping the
// --worktrees flag wiring in sessionsAuditCmdRun, turns the second database
// missing and this fails on the report count.
func TestSessionsAudit_WorktreesScansEveryRegisteredDatabase(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	linkedDir := t.TempDir()
	currentDB := seedAuditFixtureDB(t, dataDir)
	linkedDB := seedAuditFixtureDB(t, linkedDir)

	// The way `rush` registers a checkout on startup: its working directory
	// and the data directory it keeps its own .rush/rush.db in.
	require.NoError(t, projects.Register(filepath.Join(linkedDir, "checkout"), linkedDir))
	require.Equal(t, []string{linkedDir}, auditWorktreeDataDirs(dataDir),
		"the linked worktree's data dir is the only extra database to scan")

	beforeCurrent := auditFileSHA256(t, currentDB)
	beforeLinked := auditFileSHA256(t, linkedDB)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all")
	require.NoError(t, err, "stderr: %s", stderr)
	require.Contains(t, stdout, dataDir, "the text report must name the current database")
	require.Contains(t, stdout, linkedDir, "the text report must name the linked worktree's database")
	require.Equal(t, 2, strings.Count(stdout, "db health:"),
		"one health block per scanned database")

	jsonStdout, _, err := runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all", "--json")
	require.NoError(t, err)
	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.Len(t, reports, 2, "one JSON object per scanned database, not per rule or per session")
	require.Equal(t, dataDir, reports[0].DataDir, "the current database is scanned first")
	require.Equal(t, linkedDir, reports[1].DataDir)
	for _, report := range reports {
		require.Equal(t, filepath.Join(report.DataDir, "rush.db"), report.DBPath)
		require.NotNil(t, report.Health, "every scanned database reports its health")
		require.NotEmpty(t, report.Sessions, "every scanned database reports its sessions")
	}

	require.Equal(t, beforeCurrent, auditFileSHA256(t, currentDB),
		"--worktrees must not modify the current database")
	require.Equal(t, beforeLinked, auditFileSHA256(t, linkedDB),
		"--worktrees must not modify the linked worktree's database")
	require.Equal(t, []string{"rush.db"}, auditDirNames(t, linkedDir),
		"scanning a linked worktree must not create a sidecar in its data dir")

	// A registered checkout whose rush.db is gone is skipped rather than
	// reported as an error, so the scan still covers exactly two databases.
	vanishedDir := t.TempDir()
	require.NoError(t, projects.Register(filepath.Join(vanishedDir, "checkout"), vanishedDir))
	require.Equal(t, []string{linkedDir}, auditWorktreeDataDirs(dataDir),
		"a registered checkout without a rush.db is skipped")
	jsonStdout, _, err = runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.Len(t, reports, 2, "a skipped worktree is not a report")
}

// TestSessionsAudit_SinceLimitsTheWindowNotTheReport: --since bounds which
// messages the rules count, not which databases and sessions the audit
// describes. A database whose every message is outside the window is still
// audited — it is reported, with its health and its session list, and simply
// has nothing suspicious to say inside the window.
func TestSessionsAudit_SinceLimitsTheWindowNotTheReport(t *testing.T) {
	const session = "audit-since-flags-1"

	old := time.Now().Add(-72 * time.Hour).Unix()
	recent := time.Now().Add(-time.Minute).Unix()

	wait := func(createdAt int64) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": "sleep 5"}),
			CreatedAt: createdAt,
		}
	}
	notice := func(kind string, createdAt int64) auditMessageFixture {
		return auditMessageFixture{SessionID: session, NoticeKind: kind, CreatedAt: createdAt}
	}

	// Three waits older than the window: nothing inside it to count.
	stale := auditRulesFixture{Messages: []auditMessageFixture{wait(old), wait(old + 1), wait(old + 2)}}
	_, reports := auditRunRulesAll(t, stale, "--since", "24h")
	require.Empty(t, reports[0].Findings, "a wait outside the window is not counted")
	require.NotEmpty(t, reports[0].Sessions,
		"--since narrows the analysis, not the sessions the audit describes")
	require.NotNil(t, reports[0].Health, "DB health is not part of the window")

	// The same three waits inside the window: the rule fires.
	fresh := auditRulesFixture{Messages: []auditMessageFixture{wait(recent), wait(recent + 1), wait(recent + 2)}}
	stdout, _ := auditRunRulesAll(t, fresh, "--since", "24h")
	require.Contains(t, stdout, auditFindingLine("wait_only", session, "sleep N", 3))

	// The window is not specific to one rule: a threshold-one rule obeys it
	// exactly the same way.
	noticeOld := auditRulesFixture{Messages: []auditMessageFixture{notice("supervision", old)}}
	stdout, _ = auditRunRulesAll(t, noticeOld, "--since", "24h")
	require.NotContains(t, stdout, "notices", "a notice outside the window is not counted")

	noticeRecent := auditRulesFixture{Messages: []auditMessageFixture{notice("supervision", recent)}}
	stdout, _ = auditRunRulesAll(t, noticeRecent, "--since", "24h")
	require.Contains(t, stdout, auditFindingLine("notices", session, "supervision", 1))
}

// TestSessionsAudit_JSONCarriesFindings: --json is what a script reads, so it
// carries the findings themselves — the rule, the session, the evidence and
// the count — and not only the summary the text output shows.
func TestSessionsAudit_JSONCarriesFindings(t *testing.T) {
	const session = "audit-json-flags-1"

	view := func(path string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": path}),
		}
	}
	fixture := auditRulesFixture{Messages: []auditMessageFixture{
		// Two files, three views each, and one repeated command.
		view("internal/cmd/sessions_audit.go"), view("internal/cmd/sessions_audit.go"),
		view("internal/cmd/sessions_audit.go"),
		view("internal/cmd/sessions_audit_report.go"), view("internal/cmd/sessions_audit_report.go"),
		view("internal/cmd/sessions_audit_report.go"),
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 2"}),
		},
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 77"}),
		},
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 909"}),
		},
	}}

	_, reports := auditRunRulesAll(t, fixture)
	require.Len(t, reports, 1)

	findings := reports[0].Findings
	require.NotEmpty(t, findings, "a fixture of repeats must produce findings in --json")
	for _, finding := range findings {
		require.NotEmpty(t, finding.Rule, "a finding without a rule cannot be acted on")
		require.NotEmpty(t, finding.SessionID, "a finding must name the session that tripped it")
		require.NotEmpty(t, finding.Detail, "a finding must carry its evidence")
		require.Greater(t, finding.Count, 0, "a finding is a count that reached its threshold")
		require.Equal(t, session, finding.SessionID)
	}

	views := auditFindingsForRule(reports, "repeated-view")
	require.Len(t, views, 2, "each repeated file is its own finding")
	require.Equal(t, "repeated-view", views[0].Rule)
	require.Equal(t, 3, views[0].Count)

	commands := auditFindingsForRule(reports, "repeated_command")
	require.Len(t, commands, 1)
	require.Equal(t, 3, commands[0].Count)
	require.Equal(t, "go test ./internal/cmd/ N", commands[0].Detail,
		"the JSON evidence is normalized exactly like the text output's")
}
