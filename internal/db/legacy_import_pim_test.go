package db

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/stretchr/testify/require"
)

const pimCrashHelperEnv = "RUSH_PIM_IMPORT_CRASH_HELPER"

func pimExistingDestination(t *testing.T) {
	t.Helper()
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "valid"}[valid], func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			_, source := createLegacyFixture(t, dataDir)
			defer source.Close()
			beforeSource := legacyImportHashes(t, storepath.DataDirPath(dataDir, storepath.LegacyFilename))
			path := storepath.StoreV1Path(dataDir)
			if valid {
				dest := openTestDB(t, path)
				require.NoError(t, migrateLocked(t.Context(), dest, path, storeV1FixtureMigrations(), MigrateOptions{MayMigrate: true}))
				require.NoError(t, dest.Close())
			} else {
				require.NoError(t, os.WriteFile(path, []byte("invalid destination"), 0o600))
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
			require.NoError(t, err)
			require.Equal(t, LegacyImportNoop, result.Status)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeSource, legacyImportHashes(t, storepath.DataDirPath(dataDir, storepath.LegacyFilename)))
		})
	}
}

func pimHostRefusal(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"alive", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			legacy, conn := createLegacyFixture(t, dataDir)
			defer conn.Close()
			_, err := conn.ExecContext(t.Context(), `DELETE FROM async_jobs`)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO async_hosts (id,pid,label,started_at) VALUES ('idle-host',1,'cli',1)`)
			require.NoError(t, err)
			if mode == "alive" {
				lock, lockErr := filelock.TryAcquireFileLock(filepath.Join(dataDir, "hosts", "idle-host.lock"))
				require.NoError(t, lockErr)
				defer lock.Release()
				status, probeErr := filelock.ProbeShared(filepath.Join(dataDir, "hosts", "idle-host.lock"))
				require.Equal(t, filelock.LockStatusAlive, status)
				require.NoError(t, probeErr)
			} else {
				require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "hosts", "idle-host.lock"), 0o700))
				status, probeErr := filelock.ProbeShared(filepath.Join(dataDir, "hosts", "idle-host.lock"))
				require.Equal(t, filelock.LockStatusUnknown, status)
				require.Error(t, probeErr)
			}
			before := legacyImportHashes(t, legacy)
			_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
			require.ErrorIs(t, err, ErrLegacyImportLiveHosts)
			require.ErrorContains(t, err, "idle-host")
			require.Equal(t, before, legacyImportHashes(t, legacy))
			_, err = os.Stat(storepath.StoreV1Path(dataDir))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func pimJournalModes(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"DELETE", "WAL"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			legacy, conn := createLegacyFixture(t, dataDir)
			defer conn.Close()
			actualMode := ""
			err := conn.QueryRowContext(t.Context(), "PRAGMA journal_mode="+mode).Scan(&actualMode)
			require.NoError(t, err)
			require.Equal(t, strings.ToLower(mode), actualMode)
			before := legacyImportHashes(t, legacy)
			result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
			require.NoError(t, err)
			require.Equal(t, LegacyImportImported, result.Status)
			require.Equal(t, before, legacyImportHashes(t, legacy))
			main, err := os.ReadFile(storepath.StoreV1Path(dataDir))
			require.NoError(t, err)
			require.NotEmpty(t, main)
			for _, suffix := range []string{"-wal", "-shm"} {
				_, err := os.Stat(storepath.StoreV1Path(dataDir) + suffix)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			dest := openTestDB(t, storepath.StoreV1Path(dataDir))
			defer dest.Close()
			var count int
			require.NoError(t, dest.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='async_jobs'`).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, dest.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM batch_timeout_checkins`).Scan(&count))
			require.Zero(t, count)
			var integrity string
			require.NoError(t, dest.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity))
			require.Equal(t, "ok", integrity)
			rows, err := dest.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
			require.NoError(t, err)
			defer rows.Close()
			require.False(t, rows.Next())
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			var sourceCheckins int
			require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM batch_timeout_checkins`).Scan(&sourceCheckins))
			require.Equal(t, 1, sourceCheckins)
			var messages, messageCount int
			require.NoError(t, dest.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE session_id='s'`).Scan(&messages))
			require.NoError(t, dest.QueryRowContext(t.Context(), `SELECT message_count FROM sessions WHERE id='s'`).Scan(&messageCount))
			require.Equal(t, 2, messages)
			require.Equal(t, 2, messageCount)
			var created, updated int64
			require.NoError(t, dest.QueryRowContext(t.Context(), `SELECT created_at,updated_at FROM messages WHERE id='m'`).Scan(&created, &updated))
			require.Equal(t, int64(1700000000), created)
			require.Equal(t, int64(1700000000), updated)
		})
	}
}

func pimFailureRetry(t *testing.T) {
	t.Helper()
	dataDir := t.TempDir()
	legacy, conn := createLegacyFixture(t, dataDir)
	defer conn.Close()
	_, err := conn.ExecContext(t.Context(), `INSERT INTO read_files (session_id,path,read_at) VALUES ('s','boom',3)`)
	require.NoError(t, err)
	before := legacyImportHashes(t, legacy)
	badSQL, err := os.ReadFile(filepath.Join("testdata", "store_v1", "20261015000001_store_v1_initial.sql"))
	require.NoError(t, err)
	broken := string(badSQL)
	broken = strings.Replace(broken, `path TEXT NOT NULL CHECK (path != '')`, `path TEXT NOT NULL CHECK (path != '' AND path <> 'boom')`, 1)
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: fstest.MapFS{"20261015000001_store_v1_initial.sql": &fstest.MapFile{Data: []byte(broken)}}})
	require.ErrorContains(t, err, "read_files")
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	legacyImportNoArtifacts(t, dataDir)
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.NoError(t, err)
	require.Equal(t, before, legacyImportHashes(t, legacy))
}

type pimCrashSchemaFS struct{}

func (pimCrashSchemaFS) Open(name string) (fs.File, error) {
	if name == "20261015000001_store_v1_initial.sql" {
		entries, err := os.ReadDir(os.Getenv("RUSH_PIM_DATA_DIR"))
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".rush-import-building-") {
					os.Exit(73)
				}
			}
		}
	}
	return storeV1FixtureMigrations().Open(name)
}

func (pimCrashSchemaFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(storeV1FixtureMigrations(), name)
}

func pimCrashChild() {
	if os.Getenv(pimCrashHelperEnv) != "1" {
		return
	}
	dataDir := os.Getenv("RUSH_PIM_DATA_DIR")
	_, _ = ImportLegacyStore(context.Background(), dataDir, LegacyImportOptions{Schema: pimCrashSchemaFS{}})
	os.Exit(74)
}

func pimPrepublicationCrashRetry(t *testing.T) {
	t.Helper()
	if os.Getenv(pimCrashHelperEnv) == "1" {
		pimCrashChild()
	}
	dataDir := t.TempDir()
	legacy, conn := createLegacyFixture(t, dataDir)
	defer conn.Close()
	before := legacyImportHashes(t, legacy)
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestLegacyImportSweepsDebrisFromCrashedRun$/^pimPrepublicationCrashRetry$")
	cmd.Env = append(os.Environ(), pimCrashHelperEnv+"=1", "RUSH_PIM_DATA_DIR="+dataDir)
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "%s", output)
	require.Equal(t, 73, cmd.ProcessState.ExitCode())
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	var hasBuilding bool
	for _, entry := range entries {
		hasBuilding = hasBuilding || strings.HasPrefix(entry.Name(), ".rush-import-building-")
	}
	require.True(t, hasBuilding)
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	legacyImportNoArtifacts(t, dataDir)
	require.Equal(t, before, legacyImportHashes(t, legacy))
}
