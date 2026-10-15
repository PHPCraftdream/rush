package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/filelock"
)

type LegacyImportStatus uint8

const (
	LegacyImportNoop LegacyImportStatus = iota
	LegacyImportImported
)

type LegacyImportResult struct {
	Status LegacyImportStatus
}

type LegacyImportOptions struct {
	// Schema is caller-supplied; production schema and test fixtures are not embedded.
	Schema fs.FS
}

// ImportLegacyStore imports durable legacy data once and never changes startup selection.
func ImportLegacyStore(ctx context.Context, dataDir string, opts LegacyImportOptions) (LegacyImportResult, error) {
	legacy := storepath.DataDirPath(dataDir, storepath.LegacyFilename)
	dest := storepath.StoreV1Path(dataDir)
	lockPath := filepath.Join(dataDir, storepath.StoreV1MigrateLockFilename)
	lock, err := filelock.AcquireFileLockContext(ctx, lockPath)
	if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import lock: %w", err)
	}
	defer func() { _ = lock.Release() }()
	if _, err := os.Stat(dest); err == nil {
		return LegacyImportResult{Status: LegacyImportNoop}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return LegacyImportResult{}, fmt.Errorf("legacy import destination: %w", err)
	}
	if _, err := os.Stat(legacy); errors.Is(err, os.ErrNotExist) {
		return LegacyImportResult{Status: LegacyImportNoop}, nil
	} else if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import source: %w", err)
	}
	if opts.Schema == nil {
		return LegacyImportResult{}, errors.New("legacy import: schema source is required")
	}
	if err := checkLegacyLiveHosts(ctx, legacy, dataDir); err != nil {
		return LegacyImportResult{}, &LegacyImportLiveHostsError{Cause: err}
	}

	snapshotDir, err := os.MkdirTemp(dataDir, ".rush-import-snapshot-")
	if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import snapshot directory: %w", err)
	}
	defer os.RemoveAll(snapshotDir)
	snapshot := filepath.Join(snapshotDir, "legacy.db")
	sourceState, err := snapshotLegacyFiles(legacy, snapshot)
	if err != nil {
		return LegacyImportResult{}, err
	}
	old, err := openReadDB(snapshot)
	if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import open snapshot: %w", err)
	}
	old.SetMaxOpenConns(1)
	defer old.Close()

	tmp, err := os.CreateTemp(dataDir, ".rush-import-building-*.db")
	if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import temporary database: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return LegacyImportResult{}, fmt.Errorf("legacy import temporary database close: %w", err)
	}
	defer removeImportArtifacts(tmpPath)
	fresh, err := openDB(tmpPath)
	if err != nil {
		return LegacyImportResult{}, err
	}
	fresh.SetMaxOpenConns(1)
	if err := migrateLocked(ctx, fresh, tmpPath, opts.Schema, MigrateOptions{MayMigrate: true}); err != nil {
		_ = fresh.Close()
		return LegacyImportResult{}, fmt.Errorf("legacy import schema: %w", err)
	}
	if err := copyLegacyRows(ctx, old, fresh); err != nil {
		_ = fresh.Close()
		return LegacyImportResult{}, err
	}
	if err := verifyImportedDatabase(ctx, old, fresh); err != nil {
		_ = fresh.Close()
		return LegacyImportResult{}, err
	}
	if err := checkpointImportDB(ctx, fresh); err != nil {
		_ = fresh.Close()
		return LegacyImportResult{}, err
	}
	if err := fresh.Close(); err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import close destination: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(tmpPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return LegacyImportResult{}, fmt.Errorf("legacy import remove destination sidecar %s: %w", suffix, err)
		}
	}
	if err := old.Close(); err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import close snapshot: %w", err)
	}
	// The host gate is advisory over enumerated hosts, not a registration fence.
	// Recheck source bytes before publication; operators must still stop old hosts
	// and automatic restarts for the entire import window.
	if err := verifyLegacyFilesUnchanged(legacy, sourceState); err != nil {
		return LegacyImportResult{}, err
	}
	if err := syncFile(tmpPath); err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import sync destination: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import atomic publish: %w", err)
	}
	// The store file is fsynced and verified before the rename, so after a
	// successful rename the published store is complete; a directory-entry
	// sync failure must not turn a published store into a reported failure
	// (and Windows cannot open directories for Sync).
	syncDirectoryBestEffort(dataDir)
	return LegacyImportResult{Status: LegacyImportImported}, nil
}

func syncDirectoryBestEffort(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	f, err := os.Open(dir)
	if err != nil {
		slog.Warn("Directory sync after store publish failed; the published store is already fsynced.", "dir", dir, "err", err)
		return
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		slog.Warn("Directory sync after store publish failed; the published store is already fsynced.", "dir", dir, "err", err)
	}
}

type sourceFileState struct {
	suffix  string
	present bool
	size    int64
	modTime int64
	hash    [32]byte
}

func snapshotLegacyFiles(legacy, snapshot string) ([]sourceFileState, error) {
	states := make([]sourceFileState, 0, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		state, err := copyFileState(legacy+suffix, snapshot+suffix, suffix == "")
		state.suffix = suffix
		if err != nil {
			return nil, fmt.Errorf("legacy import snapshot %q: %w", suffix, err)
		}
		states = append(states, state)
	}
	return states, nil
}

func copyFileState(src, dst string, required bool) (sourceFileState, error) {
	state := sourceFileState{}
	if strings.HasSuffix(src, "-wal") {
		state.suffix = "-wal"
	} else if strings.HasSuffix(src, "-shm") {
		state.suffix = "-shm"
	}
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) && !required {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return state, err
	}
	h := sha256.New()
	var out *os.File
	if dst != "" {
		out, err = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return state, err
		}
	}
	writer := io.Writer(h)
	if out != nil {
		writer = io.MultiWriter(h, out)
	}
	_, copyErr := io.Copy(writer, in)
	var closeErr error
	if out != nil {
		closeErr = out.Close()
	}
	if copyErr != nil || closeErr != nil {
		return state, errors.Join(copyErr, closeErr)
	}
	state.present = true
	state.size = info.Size()
	state.modTime = info.ModTime().UnixNano()
	copy(state.hash[:], h.Sum(nil))
	return state, nil
}

func verifyLegacyFilesUnchanged(legacy string, want []sourceFileState) error {
	for _, expected := range want {
		got, err := copyFileState(legacy+expected.suffix, "", expected.suffix == "")
		if err != nil {
			return fmt.Errorf("legacy import source changed: %w", err)
		}
		if got.present != expected.present || got.size != expected.size || got.modTime != expected.modTime || got.hash != expected.hash {
			return fmt.Errorf("legacy import source changed during copy (%s); no new store published", expected.suffix)
		}
	}
	return nil
}

func removeImportArtifacts(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func checkpointImportDB(ctx context.Context, conn *sql.DB) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return fmt.Errorf("legacy import checkpoint destination: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return errors.Join(errors.New("legacy import checkpoint returned no status"), rows.Err(), rows.Close())
	}
	var busy, logFrames, checkpointed int
	if err := rows.Scan(&busy, &logFrames, &checkpointed); err != nil {
		return errors.Join(err, rows.Close())
	}
	if err := rows.Err(); err != nil {
		return errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if busy != 0 || logFrames != checkpointed {
		return fmt.Errorf("legacy import checkpoint incomplete: busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func checkLegacyLiveHosts(ctx context.Context, legacy, dataDir string) error {
	snapshotDir, err := os.MkdirTemp(dataDir, ".rush-host-probe-")
	if err != nil {
		return fmt.Errorf("legacy import host probe snapshot: %w", err)
	}
	defer os.RemoveAll(snapshotDir)
	copyPath := filepath.Join(snapshotDir, "legacy.db")
	if _, err := snapshotLegacyFiles(legacy, copyPath); err != nil {
		return err
	}
	conn, err := openReadDB(copyPath)
	if err != nil {
		return fmt.Errorf("legacy import host probe open: %w", err)
	}
	defer conn.Close()
	return CheckLiveHosts(ctx, conn, dataDir, nil)
}

type legacyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func copyLegacyRows(ctx context.Context, old, fresh *sql.DB) error {
	if err := old.PingContext(ctx); err != nil {
		return fmt.Errorf("legacy import snapshot connection: %w", err)
	}
	for _, table := range []string{"async_jobs", "async_jobs_new", "async_jobs_old_schema", "batch_nodes", "batch_control_requests", "batch_runner_checkpoints", "orphan_call_outbox", "async_hosts", "session_drivers"} {
		if _, exists, err := tableColumnInfo(ctx, old, table); err != nil {
			return err
		} else if exists {
			policy, ok := legacyImportPolicy(table)
			if !ok || policy.Carry {
				return fmt.Errorf("legacy import: non-carried table %q has no explicit drop policy", table)
			}
		}
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
	fkRows, err := fresh.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	if fkRows.Next() {
		_ = fkRows.Close()
		return errors.New("legacy import verification: foreign key check failed")
	}
	if err := fkRows.Err(); err != nil {
		_ = fkRows.Close()
		return err
	}
	if err := fkRows.Close(); err != nil {
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
