package cmd

// Everything the audit does to one database: open it strictly read-only,
// verify its schema is a Rush schema, measure DB health, pick the sessions
// to look at and summarise them. The driver-specific open lives in
// sessions_audit_driver_*.go, whose build tags mirror internal/db's split so
// the "sqlite" driver is registered exactly once per build.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Columns an audited schema must have. Anything else is reported as an
// unrecognized schema and the database is left alone.
var (
	auditRequiredSessionsColumns = []string{"id", "title", "created_at", "updated_at", "message_count"}
	auditRequiredMessagesColumns = []string{"id", "session_id", "role", "parts", "created_at"}
)

// auditSchema records the optional capabilities the analysis may use. Their
// presence is discovered through PRAGMA table_info, never assumed: a
// database older than a migration is audited with the reduced set instead
// of failing.
type auditSchema struct {
	MessagesNoticeKind     bool
	MessagesBackgroundJobs bool
	HasAsyncJobs           bool
	AsyncJobColumns        []string
}

// auditDBHealth is the file-level picture of one database.
type auditDBHealth struct {
	Path          string  `json:"path"`
	SizeBytes     int64   `json:"size_bytes"`
	PageSize      int64   `json:"page_size"`
	PageCount     int64   `json:"page_count"`
	FreelistPages int64   `json:"freelist_pages"`
	FreePercent   float64 `json:"free_percent"`
	WALBytes      int64   `json:"wal_bytes,omitempty"`
	SHMBytes      int64   `json:"shm_bytes,omitempty"`
}

// auditSessionRow is the per-session summary a report line is built from.
type auditSessionRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	MessageCount int64  `json:"message_count"`
}

// auditDataDir audits one data directory. It never writes: a missing
// rush.db is a notice, a foreign schema is a notice, and both exit
// successfully.
func auditDataDir(ctx context.Context, dataDir string, args []string, all bool, since time.Duration) (auditDBReport, error) {
	report := auditDBReport{DataDir: dataDir, Sessions: []auditSessionRow{}, Findings: []auditFinding{}}
	dbPath := filepath.Join(dataDir, "rush.db")
	report.DBPath = dbPath

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			report.Notices = append(report.Notices,
				fmt.Sprintf("no rush.db found in %s; audit is read-only and does not create anything", dataDir))
			return report, nil
		}
		return report, fmt.Errorf("stat %s: %w", dbPath, err)
	}

	db, err := openAuditRO(dbPath)
	if err != nil {
		return report, fmt.Errorf("open %s read-only: %w", dbPath, err)
	}
	defer db.Close()

	schema, issues := auditInspectSchema(ctx, db)
	if len(issues) > 0 {
		// Not a Rush schema (someone else's database, or one too old to
		// reason about). Report and leave it untouched.
		report.SchemaIssues = issues
		return report, nil
	}

	health, err := auditCollectHealth(ctx, db, dbPath)
	if err != nil {
		return report, fmt.Errorf("read db health of %s: %w", dbPath, err)
	}
	report.Health = &health

	rows, notices, err := auditSelectSessions(ctx, db, args, all)
	if err != nil {
		return report, err
	}
	report.Notices = append(report.Notices, notices...)
	report.Sessions = rows
	if len(rows) == 0 {
		return report, nil
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	findings, err := auditFindings(ctx, db, schema, ids, since)
	if err != nil {
		return report, err
	}
	if len(findings) > 0 {
		report.Findings = findings
	}
	return report, nil
}

// auditInspectSchema reads the shape of the database and reports what an
// audit cannot assume. Missing required columns produce the messages the
// RunE prints; they are never an error, because a database audit cannot
// read is still a database audit must not touch.
func auditInspectSchema(ctx context.Context, db *sql.DB) (auditSchema, []string) {
	schema := auditSchema{}
	var issues []string

	for _, table := range []struct {
		name     string
		required []string
	}{
		{"sessions", auditRequiredSessionsColumns},
		{"messages", auditRequiredMessagesColumns},
	} {
		columns, err := auditTableColumns(ctx, db, table.name)
		if err != nil {
			issues = append(issues, fmt.Sprintf("unrecognized schema: cannot read table %s: %v", table.name, err))
			continue
		}
		if len(columns) == 0 {
			issues = append(issues, fmt.Sprintf("unrecognized schema: no %s table", table.name))
			continue
		}
		for _, want := range table.required {
			if !columns[want] {
				issues = append(issues, fmt.Sprintf("unrecognized schema: missing %s.%s", table.name, want))
			}
		}
	}

	messageColumns, err := auditTableColumns(ctx, db, "messages")
	if err == nil {
		schema.MessagesNoticeKind = messageColumns["notice_kind"]
		schema.MessagesBackgroundJobs = messageColumns["background_job_notice"]
	}

	jobColumns, err := auditTableColumns(ctx, db, "async_jobs")
	if err == nil && len(jobColumns) > 0 {
		schema.HasAsyncJobs = true
		schema.AsyncJobColumns = make([]string, 0, len(jobColumns))
		for column := range jobColumns {
			schema.AsyncJobColumns = append(schema.AsyncJobColumns, column)
		}
		sort.Strings(schema.AsyncJobColumns)
	}

	return schema, issues
}

// auditTableColumns runs PRAGMA table_info and returns the column names.
// An absent table is not an error — it is an empty set.
func auditTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

// auditCollectHealth measures the file, not the contents: page size and
// count, freelist pages, and how much of the file is free. WAL sidecars are
// stat'ed because they hold the part of a live database the main file has
// not absorbed yet.
func auditCollectHealth(ctx context.Context, db *sql.DB, dbPath string) (auditDBHealth, error) {
	health := auditDBHealth{Path: dbPath}

	for _, pragma := range []struct {
		name string
		dest *int64
	}{
		{"page_size", &health.PageSize},
		{"page_count", &health.PageCount},
		{"freelist_count", &health.FreelistPages},
	} {
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(pragma.dest); err != nil {
			return health, fmt.Errorf("PRAGMA %s: %w", pragma.name, err)
		}
	}

	if health.PageCount > 0 {
		health.FreePercent = float64(health.FreelistPages) / float64(health.PageCount) * 100
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		return health, fmt.Errorf("stat %s: %w", dbPath, err)
	}
	health.SizeBytes = info.Size()

	for suffix, dest := range map[string]*int64{
		"-wal": &health.WALBytes,
		"-shm": &health.SHMBytes,
	} {
		sidecar, err := os.Stat(dbPath + suffix)
		if err != nil {
			// A sidecar is optional: no WAL, or a checkpointed one.
			continue
		}
		*dest = sidecar.Size()
	}

	return health, nil
}

// auditSelectSessions resolves the selection (all, an exact id, or a prefix)
// into session rows. Ambiguity is a notice, never a guess.
func auditSelectSessions(ctx context.Context, db *sql.DB, args []string, all bool) ([]auditSessionRow, []string, error) {
	const query = `SELECT id, title, created_at, updated_at, message_count FROM sessions`

	var notices []string
	var selection []auditSessionRow
	var err error
	if all {
		selection, err = auditQuerySessions(ctx, db, query+" ORDER BY updated_at DESC", nil)
	} else {
		id := args[0]
		// An exact id or a prefix: a single match is unambiguous, more than
		// one is not — the operator has to say which session they meant.
		selection, err = auditQuerySessions(ctx, db,
			query+" WHERE id = ? OR id LIKE ? || '%' ORDER BY updated_at DESC", []any{id, id})
	}
	if err != nil {
		return nil, nil, err
	}
	if all {
		return selection, notices, nil
	}

	id := args[0]
	// The exact id wins outright, even when the prefix also matches others.
	for _, row := range selection {
		if row.ID == id {
			return []auditSessionRow{row}, notices, nil
		}
	}
	if len(selection) > 1 {
		notices = append(notices, fmt.Sprintf("prefix %q matches %d sessions — re-run with the full id:", id, len(selection)))
		for _, row := range selection {
			notices = append(notices, "  "+auditFormatUnix(row.UpdatedAt)+"  "+row.ID)
		}
		return nil, notices, nil
	}
	if len(selection) == 0 {
		notices = append(notices, fmt.Sprintf("no session matches %q", id))
	}
	return selection, notices, nil
}

// auditQuerySessions runs one session-selecting query and scans the rows.
func auditQuerySessions(ctx context.Context, db *sql.DB, query string, params []any) ([]auditSessionRow, error) {
	result, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer result.Close()

	var rows []auditSessionRow
	for result.Next() {
		var row auditSessionRow
		if err := result.Scan(&row.ID, &row.Title, &row.CreatedAt, &row.UpdatedAt, &row.MessageCount); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return rows, nil
}

// auditFormatUnix renders a millisecond timestamp for the text output.
func auditFormatUnix(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

// auditFormatBytes renders a byte count the way an operator reads it.
func auditFormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, suffix := range suffixes {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
