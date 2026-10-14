// Flag-ON (RUSH_BATCH_RUNTIME) host-identity and session-notice plumbing
// (package R12-F chunk C): the App's host lifecycle moves from the
// async_jobs-backed AsyncJobStore to R12-D's session.HostStore, and the
// job-independent session-notice machinery from R12-D's
// session.SessionNoticeStore, so registration, teardown and the notice
// surfaces never query the async_jobs table (they survive the R12-H drop).
// Flag OFF: nothing here is built or called.

package app

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchHostStores memoizes each App's flag-ON HostStore. The App struct
// (app.go) is outside this chunk's ownership, so the store lives in this
// package-level map keyed by the App pointer; the entry is created once at
// the flagged install and lives as long as the App.
var batchHostStores sync.Map // *App -> *session.HostStore

// batchNoticeStores memoizes each App's flag-ON SessionNoticeStore.
var batchNoticeStores sync.Map // *App -> *session.SessionNoticeStore

// batchHostStore returns this App's flag-ON HostStore, building it on first
// use over the writer pool (same pool choice as BatchCLIStore). Nil without
// a database or data dir (the configless-App rule): callers treat nil as
// "no host identity", never panic.
func (app *App) batchHostStore() *session.HostStore {
	if app == nil || app.dataDir == "" {
		return nil
	}
	if s, ok := batchHostStores.Load(app); ok {
		return s.(*session.HostStore)
	}
	writer := app.batchCutoverWriter()
	if writer == nil {
		return nil
	}
	s := session.NewHostStore(writer, app.dataDir, os.Getpid(), "app")
	actual, loaded := batchHostStores.LoadOrStore(app, s)
	if loaded {
		// A concurrent caller won: one store per App.
		return actual.(*session.HostStore)
	}
	return s
}

// batchHostEnsure registers this App's flag-ON host identity exactly once
// (lazy, memoized inside the HostStore). Fail closed: the caller aborts the
// install on error.
func (app *App) batchHostEnsure(ctx context.Context) (string, error) {
	s := app.batchHostStore()
	if s == nil {
		return "", errors.New("batch host store unavailable: no db writer or data dir")
	}
	return s.EnsureHost(ctx)
}

// batchHostRelease is the flag-ON graceful host release (HostStore.Close:
// OS lock release plus row cleanup via DeleteAsyncHostIfNoNodes).
func (app *App) batchHostRelease(ctx context.Context) error {
	s := app.batchHostStore()
	if s == nil {
		return nil
	}
	return s.Close(ctx)
}

// batchHostReleaseKeepLock is the flag-ON forced counterpart
// (HostStore.CloseKeepLock: forget the handle, pin the OS lock).
func (app *App) batchHostReleaseKeepLock() {
	if s := app.batchHostStore(); s != nil {
		s.CloseKeepLock()
	}
}

// batchNoticeStore returns this App's flag-ON SessionNoticeStore, built
// with the R12-D batch implementations and wired to the HostStore-backed
// host identity and liveness. Nil without a database (configless App).
func (app *App) batchNoticeStore() *session.SessionNoticeStore {
	if app == nil {
		return nil
	}
	if s, ok := batchNoticeStores.Load(app); ok {
		return s.(*session.SessionNoticeStore)
	}
	writer := app.batchCutoverWriter()
	if writer == nil {
		return nil
	}
	s := session.NewSessionNoticeStore(writer)
	s.SetReadConn(db.New(app.batchCutoverReader(writer)))
	if hs := app.batchHostStore(); hs != nil {
		s.SetHostIDFunc(hs.HostID)
		s.SetHostNotDeadFunc(func(own, host string) bool {
			if host == "" {
				return false
			}
			if host == own {
				return true
			}
			// Fail-open on an unreadable probe (alive-or-unknown keeps the
			// notice), exactly like AsyncJobStore.HostNotDead.
			st, err := session.ProbeHostShared(app.dataDir, host)
			return err != nil || st != session.HostStatusDead
		})
	}
	actual, loaded := batchNoticeStores.LoadOrStore(app, s)
	if loaded {
		return actual.(*session.SessionNoticeStore)
	}
	return s
}
