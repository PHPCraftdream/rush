package migrater

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
)

// legacyMigrations is the importer-owned copy of the pre-store-v1
// migration chain. ImportLegacyStore upgrades a private snapshot of an
// old rush.db to this chain's head before copying rows, so data written
// by older binaries (model-slot renames, cost backfills, tables that do
// not exist yet) arrives in the new store instead of being silently
// dropped.
//
// H4 will replace the active embedded set (internal/db/migrations,
// embedded as FS in connect.go) with the fresh store_v1 schema. This
// copy keeps the importer able to upgrade legacy files after that swap:
// it is frozen at the last legacy head and must never gain a store_v1
// migration. Until H4 lands, TestLegacyImportMigrationsMatchEmbedded
// pins this directory byte-identical to internal/db/migrations; H4
// deletes that test together with the migrations directory and
// repoints sqlc.yaml.
//
//go:embed legacymigrations/*.sql
var legacyMigrations embed.FS

// LegacyFS returns the legacy chain rooted at its own
// directory, so callers see the bare migration filenames. It mirrors
// migrationFS for the active set.
func LegacyFS() (fs.FS, error) {
	return fs.Sub(legacyMigrations, "legacymigrations")
}

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
// The private snapshot is first migrated to the head of the importer-owned legacy
// chain (LegacyFS), so stores written by older binaries are upgraded
// before their rows are copied; a snapshot recorded by an unknown lineage is refused.
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
	// Artifacts of a previous crashed import are only ever created under
	// this lock, so no live importer can own them now.
	sweepImportDebris(dataDir)

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
	// The snapshot is opened read-write: it is migrated to the legacy head
	// below, and the original legacy file is never opened for writing.
	old, err := OpenDB(snapshot)
	if err != nil {
		return LegacyImportResult{}, fmt.Errorf("legacy import open snapshot: %w", err)
	}
	old.SetMaxOpenConns(1)
	defer old.Close()
	if err := migrateLegacySnapshot(ctx, old); err != nil {
		return LegacyImportResult{}, err
	}
	// The probe reads async_hosts from the migrated snapshot: stores older
	// than 20260928000002 only gain that table through the migration above.
	if err := CheckLiveHosts(ctx, old, dataDir, nil); err != nil {
		return LegacyImportResult{}, &LegacyImportLiveHostsError{Cause: err}
	}

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
	fresh, err := OpenDB(tmpPath)
	if err != nil {
		return LegacyImportResult{}, err
	}
	fresh.SetMaxOpenConns(1)
	if err := ApplyLocked(ctx, fresh, tmpPath, opts.Schema, MigrateOptions{MayMigrate: true}); err != nil {
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

// migrateLegacySnapshot brings the private snapshot to the head of the
// importer-owned legacy migration chain and refuses snapshots recorded by
// a lineage this binary does not know. Only the throwaway copy changes.
func migrateLegacySnapshot(ctx context.Context, snapshot *sql.DB) error {
	fsys, err := LegacyFS()
	if err != nil {
		return err
	}
	provider, err := newMigrationProvider(snapshot, fsys, providerOptions()...)
	if err != nil {
		return fmt.Errorf("legacy import legacy schema provider: %w", err)
	}
	applied, err := appliedVersions(ctx, snapshot, provider)
	if err != nil {
		return err
	}
	embedded, err := embeddedVersions(fsys)
	if err != nil {
		return err
	}
	// migrate() would only warn about unknown versions and import anyway,
	// silently dropping their columns; a future lineage must be refused.
	if unknown := subtractVersions(applied, embedded); len(unknown) > 0 {
		return fmt.Errorf("%w: %s", ErrLegacyImportFutureVersion, formatVersions(unknown))
	}
	if err := ApplyLocked(ctx, snapshot, "", fsys, MigrateOptions{MayMigrate: true}); err != nil {
		return fmt.Errorf("legacy import snapshot migration: %w", err)
	}
	return nil
}

// sweepImportDebris removes artifacts of a previous crashed import. Every
// importer holds the store-v1 migrate lock while running, so nothing live
// owns the names; removal is best-effort because a crashed process's file
// handles can outlive it briefly on Windows.
func sweepImportDebris(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".rush-import-snapshot-") &&
			!strings.HasPrefix(name, ".rush-host-probe-") &&
			!strings.HasPrefix(name, ".rush-import-building-") {
			continue
		}
		if entry.IsDir() {
			_ = os.RemoveAll(filepath.Join(dataDir, name))
			continue
		}
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}

type legacyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
