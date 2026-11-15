package migrater

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func copyLegacyRows(ctx context.Context, old, fresh *sql.DB) error {
	if err := old.PingContext(ctx); err != nil {
		return fmt.Errorf("legacy import snapshot connection: %w", err)
	}
	for _, table := range []string{"async_jobs", "async_jobs_new", "async_jobs_old_schema", "batch_nodes", "batch_control_requests", "batch_runner_checkpoints", "batch_timeout_checkins", "async_hosts", "session_drivers"} {
		if _, exists, err := tableColumnInfo(ctx, old, table); err != nil {
			return err
		} else if exists {
			policy, ok := legacyImportPolicy(table)
			if !ok || policy.Carry {
				return fmt.Errorf("legacy import: non-carried table %q has no explicit drop policy", table)
			}
		}
	}
	if err := checkUnknownLegacyTables(ctx, old); err != nil {
		return err
	}
	plans := make([]tableCopyPlan, 0, len(LegacyImportPolicy))
	for _, policy := range LegacyImportPolicy {
		if !policy.Carry {
			continue
		}
		source, sourceExists, err := tableColumnInfo(ctx, old, policy.Name)
		if err != nil {
			return err
		}
		if !sourceExists {
			continue
		}
		target, targetExists, err := tableColumnInfo(ctx, fresh, policy.Name)
		if err != nil {
			return err
		}
		if !targetExists {
			return fmt.Errorf("legacy import schema missing required carried table %q", policy.Name)
		}
		columns := intersectColumns(source, target)
		if err := ensureRequiredColumns(policy.Name, target, columns); err != nil {
			return err
		}
		if unknown := unknownSourceColumns(source, target); len(unknown) > 0 {
			return fmt.Errorf("%w: %s drops %s", ErrLegacyImportUnknownColumns, policy.Name, strings.Join(unknown, ", "))
		}
		plans = append(plans, tableCopyPlan{policy: policy, columns: columns})
	}
	for _, p := range LegacyImportPolicy {
		if !p.Carry {
			continue
		}
		if _, sourceExists, err := tableColumnInfo(ctx, old, p.Name); err != nil {
			return err
		} else if sourceExists && !hasCopyPlan(plans, p.Name) {
			return fmt.Errorf("legacy import did not plan carried table %q", p.Name)
		}
	}
	triggerSQL, err := readTriggers(ctx, fresh)
	if err != nil {
		return err
	}
	tx, err := fresh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(cause error) error { return errors.Join(cause, tx.Rollback()) }
	// Destination triggers update timestamps/counts and must be absent while
	// restoring stored values. Their original definitions return before commit.
	if err := dropTriggers(ctx, tx, triggerSQL); err != nil {
		return rollback(err)
	}
	for _, plan := range plans {
		if err := copyTableRows(ctx, old, tx, plan.policy.Name, plan.columns); err != nil {
			return rollback(err)
		}
	}
	if err := recreateTriggers(ctx, tx, triggerSQL); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("legacy import transaction commit: %w", err)
	}
	return nil
}

type tableCopyPlan struct {
	policy  LegacyTablePolicy
	columns []columnInfo
}

func hasCopyPlan(plans []tableCopyPlan, name string) bool {
	for _, plan := range plans {
		if plan.policy.Name == name {
			return true
		}
	}
	return false
}

func intersectColumns(source, target []columnInfo) []columnInfo {
	targetSet := make(map[string]bool, len(target))
	for _, col := range target {
		targetSet[col.Name] = true
	}
	out := make([]columnInfo, 0, len(source))
	for _, col := range source {
		if targetSet[col.Name] {
			out = append(out, col)
		}
	}
	return out
}

// unknownSourceColumns returns the names of source columns that have no
// target column and would be silently dropped by intersectColumns.
func unknownSourceColumns(source, target []columnInfo) []string {
	targetSet := make(map[string]bool, len(target))
	for _, col := range target {
		targetSet[col.Name] = true
	}
	var out []string
	for _, col := range source {
		if !targetSet[col.Name] {
			out = append(out, col.Name)
		}
	}
	return out
}

type columnInfo struct {
	Name       string
	NotNull    bool
	PrimaryKey int
	HasDefault bool
}

func tableColumnInfo(ctx context.Context, conn legacyQueryer, table string) ([]columnInfo, bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name, "notnull", pk, dflt_value FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, false, fmt.Errorf("legacy import inspect %s: %w", table, err)
	}
	defer rows.Close()
	var cols []columnInfo
	for rows.Next() {
		var col columnInfo
		var def sql.NullString
		if err := rows.Scan(&col.Name, &col.NotNull, &col.PrimaryKey, &def); err != nil {
			return nil, false, err
		}
		col.HasDefault = def.Valid
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	return cols, len(cols) != 0, nil
}

// checkUnknownLegacyTables refuses snapshots with tables the import policy
// does not classify: a snapshot migrated to the legacy head only contains
// classified tables, so an extra one means the file is not a store this
// importer understands.
func checkUnknownLegacyTables(ctx context.Context, conn legacyQueryer) error {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("legacy import enumerate tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, ok := legacyImportPolicy(name); !ok {
			return fmt.Errorf("%w: %q", ErrLegacyImportUnknownTable, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func ensureRequiredColumns(table string, target, copied []columnInfo) error {
	have := make(map[string]bool, len(copied))
	for _, col := range copied {
		have[col.Name] = true
	}
	for _, col := range target {
		if have[col.Name] || (!col.NotNull && col.PrimaryKey == 0) || col.HasDefault {
			continue
		}
		return fmt.Errorf("legacy import %s: required target column %q has no source or default", table, col.Name)
	}
	return nil
}

func copyTableRows(ctx context.Context, old *sql.DB, tx *sql.Tx, table string, cols []columnInfo) error {
	quoted := make([]string, len(cols))
	for i, col := range cols {
		quoted[i] = quoteIdentifier(col.Name)
	}
	query := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteIdentifier(table) + ` ORDER BY rowid`
	rows, err := old.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("legacy import read %s: %w", table, err)
	}
	defer rows.Close()
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	insert := `INSERT INTO ` + quoteIdentifier(table) + ` (` + strings.Join(quoted, ",") + `) VALUES (` + strings.Join(placeholders, ",") + `)`
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("legacy import scan %s: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx, insert, normalizeLegacyRow(table, cols, values)...); err != nil {
			return fmt.Errorf("legacy import insert %s: %w", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("legacy import rows %s: %w", table, err)
	}
	return rows.Close()
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// normalizeLegacyRow returns the expected destination row for a copied legacy
// row, clearing runtime ownership/execution markers so the new generation never
// inherits stale in-flight state.
func normalizeLegacyRow(table string, cols []columnInfo, values []any) []any {
	out := make([]any, len(values))
	copy(out, values)
	index := func(name string) int {
		for i, col := range cols {
			if col.Name == name {
				return i
			}
		}
		return -1
	}
	value := func(name string) any {
		if i := index(name); i >= 0 {
			return out[i]
		}
		return nil
	}
	set := func(name string, v any) {
		if i := index(name); i >= 0 {
			out[i] = v
		}
	}
	switch table {
	case "sessions":
		set("cancel_requested", int64(0))
	case "queue_tasks":
		// Mirrors queue.Service.ReclaimRunning (internal/queue/queue.go).
		if sqliteTextEqual(value("status"), "running") {
			set("status", "pending")
			set("cost", int64(0))
			set("tokens", int64(0))
			set("exit_reason", "")
			set("started_at", nil)
			set("finished_at", nil)
		}
	case "session_run_queue":
		if sqliteTextEqual(value("status"), "leased") {
			set("status", "pending")
			set("leased_by", nil)
			set("leased_at", nil)
			set("lease_expires_at", nil)
			// Parity with CleanupExpiredLeases (internal/db/sql/run_queue.sql):
			// an expired lease is a consumed attempt, so the poison-entry
			// budget survives the import. Timestamps stay as stored: the
			// import preserves history, unlike the runtime recovery path.
			set("last_error", "lease_expired")
			if attempts, ok := value("attempts").(int64); ok {
				set("attempts", attempts+1)
			}
		}
	case "orphan_call_outbox":
		// 'processing' is the retired pre-P0-4 claim state; the drain only
		// scans 'pending', so a carried claim would strand its call forever.
		if sqliteTextEqual(value("status"), "processing") {
			set("status", "pending")
		}
	case "session_notices":
		if value("job_tool_call_id") != nil {
			set("delivery", "void")
			set("wake", int64(0))
			set("reacted", int64(1))
			set("job_tool_call_id", nil)
		}
	case "wake_schedules":
		set("lease_owner", nil)
		set("lease_expires_at", nil)
	}
	return out
}

// sqliteTextEqual compares SQLite text values, which drivers may return as
// string or []byte.
func sqliteTextEqual(v any, text string) bool {
	switch x := v.(type) {
	case string:
		return x == text
	case []byte:
		return string(x) == text
	default:
		return false
	}
}

type triggerDefinition struct{ name, table, sql string }

func readTriggers(ctx context.Context, conn *sql.DB) ([]triggerDefinition, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_master WHERE type='trigger' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []triggerDefinition
	for rows.Next() {
		var trigger triggerDefinition
		if err := rows.Scan(&trigger.name, &trigger.table, &trigger.sql); err != nil {
			return nil, err
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return triggers, nil
}

func dropTriggers(ctx context.Context, tx *sql.Tx, triggers []triggerDefinition) error {
	for _, trigger := range triggers {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+quoteIdentifier(trigger.name)); err != nil {
			return err
		}
	}
	return nil
}

func recreateTriggers(ctx context.Context, tx *sql.Tx, triggers []triggerDefinition) error {
	for _, trigger := range triggers {
		if _, err := tx.ExecContext(ctx, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}

func verifyImportedDatabase(ctx context.Context, old, fresh *sql.DB) error {
	for _, p := range LegacyImportPolicy {
		if !p.Carry {
			continue
		}
		sourceCols, sourceExists, err := tableColumnInfo(ctx, old, p.Name)
		if err != nil {
			return err
		}
		targetCols, targetExists, err := tableColumnInfo(ctx, fresh, p.Name)
		if err != nil {
			return err
		}
		if !sourceExists {
			continue
		}
		if !targetExists {
			return fmt.Errorf("legacy import verify: carried table %q missing", p.Name)
		}
		columns := intersectColumns(sourceCols, targetCols)
		if err := compareRows(ctx, old, fresh, p.Name, columns); err != nil {
			return err
		}
	}
	if err := foreignKeyCheck(ctx, fresh); err != nil {
		return err
	}
	var integrity string
	if err := fresh.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("legacy import integrity check: %s", integrity)
	}
	return nil
}

func compareRows(ctx context.Context, old, fresh *sql.DB, table string, cols []columnInfo) error {
	quoted := make([]string, len(cols))
	for i, col := range cols {
		quoted[i] = quoteIdentifier(col.Name)
	}
	selectSQL := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteIdentifier(table) + ` ORDER BY rowid`
	oldRows, err := old.QueryContext(ctx, selectSQL)
	if err != nil {
		return err
	}
	defer oldRows.Close()
	newRows, err := fresh.QueryContext(ctx, selectSQL)
	if err != nil {
		return err
	}
	defer newRows.Close()
	for oldRows.Next() {
		if !newRows.Next() {
			return fmt.Errorf("legacy import verify %s: destination has fewer rows", table)
		}
		left, right := make([]any, len(cols)), make([]any, len(cols))
		leftPtrs, rightPtrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range cols {
			leftPtrs[i], rightPtrs[i] = &left[i], &right[i]
		}
		if err := oldRows.Scan(leftPtrs...); err != nil {
			return err
		}
		if err := newRows.Scan(rightPtrs...); err != nil {
			return err
		}
		normalized := normalizeLegacyRow(table, cols, left)
		for i := range left {
			if !equalSQLiteValue(normalized[i], right[i]) {
				return fmt.Errorf("legacy import verify %s row column %s differs", table, cols[i].Name)
			}
		}
	}
	if err := oldRows.Err(); err != nil {
		return err
	}
	if newRows.Next() {
		return fmt.Errorf("legacy import verify %s: destination has extra rows", table)
	}
	if err := newRows.Err(); err != nil {
		return err
	}
	if err := oldRows.Close(); err != nil {
		return err
	}
	return newRows.Close()
}

// equalSQLiteValue compares SQLite values, treating []byte and string as
// equivalent text.
func equalSQLiteValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := a.([]byte); ok {
		return sqliteTextEqual(b, string(x))
	}
	if x, ok := b.([]byte); ok {
		return sqliteTextEqual(a, string(x))
	}
	switch x := a.(type) {
	case string:
		return sqliteTextEqual(b, x)
	case []byte:
		return sqliteTextEqual(b, string(x))
	default:
		return fmt.Sprint(a) == fmt.Sprint(b)
	}
}

func foreignKeyCheck(ctx context.Context, conn *sql.DB) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("legacy import verification: foreign key check failed")
	}
	return rows.Err()
}
