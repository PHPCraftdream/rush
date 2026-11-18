//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

package migrater

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A second process opening a not-yet-WAL database while another connection holds the write
// lock must wait for it, not fail with SQLITE_BUSY: journal_mode(WAL) can return BUSY at once
// (busy_timeout does not cover it, and before the pragma order was fixed it could even run
// before busy_timeout was set), so OpenWriter retries.
func TestOpenDBWaitsOnBusyTimeoutWhileAnotherConnectionWrites(t *testing.T) {
	for i := 0; i < 24; i++ {
		path := filepath.Join(t.TempDir(), "rush.db")
		holder, err := sql.Open("sqlite", "file:"+path)
		require.NoError(t, err)
		holder.SetMaxOpenConns(1)
		_, err = holder.Exec("CREATE TABLE t (id INTEGER)")
		require.NoError(t, err)
		tx, err := holder.Begin()
		require.NoError(t, err)
		_, err = tx.Exec("INSERT INTO t VALUES (1)")
		require.NoError(t, err)

		release := time.AfterFunc(300*time.Millisecond, func() { _ = tx.Commit() })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		db, openErr := OpenWriter(ctx, path)
		cancel()
		release.Stop()
		_ = tx.Rollback()
		_ = holder.Close()
		if db != nil {
			_ = db.Close()
		}
		require.NoError(t, openErr, "iteration %d: open must wait for the lock, not fail with SQLITE_BUSY", i)
	}
}

// Several openers (OpenWriter, as Connect uses) racing on a brand-new database file (two `rush run` processes starting
// together) must all get a usable connection: none may fail with SQLITE_BUSY.
func TestConcurrentOpenDBOnFreshFileNeverBusy(t *testing.T) {
	for round := 0; round < 8; round++ {
		path := filepath.Join(t.TempDir(), "rush.db")
		const openers = 8
		errs := make(chan error, openers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < openers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				db, err := OpenWriter(ctx, path)
				if err != nil {
					errs <- err
					return
				}
				defer db.Close()
				errs <- db.PingContext(ctx)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
	}
}

func TestPragmaListStartsWithBusyTimeout(t *testing.T) {
	require.Equal(t, "busy_timeout", pragmaList[0].name, "busy_timeout must be applied before any pragma that can need a lock")
}

func TestIsBusyRecognisesBothDrivers(t *testing.T) {
	require.True(t, IsBusy(errors.New("database is locked (5) (SQLITE_BUSY)")))
	require.True(t, IsBusy(errors.New("sqlite3: database is locked")))
	require.False(t, IsBusy(errors.New("no such table: t")))
	require.False(t, IsBusy(nil))
}
