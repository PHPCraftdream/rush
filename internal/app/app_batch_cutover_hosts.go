// Host-identity and session-notice plumbing
// (package R12-F chunk C): the App's host lifecycle lives in
// session.HostStore, and the job-independent session-notice machinery in
// session.SessionNoticeStore, so registration, teardown and the notice
// surfaces never query the async_jobs table.

package app

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// batchHostStores memoizes each App's HostStore. The App struct
// (app.go) does not hold the field, so the store lives in this
// package-level map keyed by the App pointer; the entry is created once at
// the install and lives as long as the App.
var batchHostStores sync.Map // *batchOwner -> *session.HostStore

// batchNoticeStores memoizes each App's SessionNoticeStore.
var batchNoticeStores sync.Map // *batchOwner -> *session.SessionNoticeStore

// batchHostStore returns this App's HostStore, building it on first
// use over the writer pool (same pool choice as BatchCLIStore). Nil without
// a database or data dir (the configless-App rule): callers treat nil as
// "no host identity", never panic.
func batchHostStore(p batchCLIParams) *session.HostStore {
	if p.env.DataDir == nil || p.env.DataDir() == "" {
		return nil
	}
	if s, ok := batchHostStores.Load(p.owner()); ok {
		return s.(*session.HostStore)
	}
	writer := batchCutoverWriter(p)
	if writer == nil {
		return nil
	}
	s := session.NewHostStore(writer, p.env.DataDir(), os.Getpid(), "app")
	actual, loaded := batchHostStores.LoadOrStore(p.owner(), s)
	if loaded {
		// A concurrent caller won: one store per App.
		return actual.(*session.HostStore)
	}
	return s
}

// batchHostEnsure registers this App's host identity exactly once
// (lazy, memoized inside the HostStore). Fail closed: the caller aborts the
// install on error.
func batchHostEnsure(p batchCLIParams, ctx context.Context) (string, error) {
	s := batchHostStore(p)
	if s == nil {
		return "", errors.New("batch host store unavailable: no db writer or data dir")
	}
	return s.EnsureHost(ctx)
}

// batchHostRelease is the graceful host release (HostStore.Close:
// OS lock release plus row cleanup via DeleteAsyncHostIfNoNodes).
func batchHostRelease(p batchCLIParams, ctx context.Context) error {
	s := batchHostStore(p)
	if s == nil {
		return nil
	}
	if err := s.Close(ctx); err != nil {
		return err
	}
	// The graceful release is the last legitimate consumer moment: forget the
	// memoized stores so the package maps never retain a closed App (P2-6).
	batchCutoverStoresForget(p)
	return nil
}

// batchCutoverStoresForget drops this App's memoized stores from the
// package-level maps (batchHostStores, batchNoticeStores), which are keyed by
// *App with no other deletion: without this, closed Apps and their
// coordinators leak for the life of the process. Nil-safe.
func batchCutoverStoresForget(p batchCLIParams) {
	if p.owner == nil {
		return
	}
	batchHostStores.Delete(p.owner())
	batchNoticeStores.Delete(p.owner())
}

// batchHostReleaseKeepLock is the forced counterpart
// (HostStore.CloseKeepLock: forget the handle, pin the OS lock).
func batchHostReleaseKeepLock(p batchCLIParams) {
	if s := batchHostStore(p); s != nil {
		s.CloseKeepLock()
	}
}

// batchNoticeStore returns this App's SessionNoticeStore, built
// with the batch implementations and wired to the HostStore-backed
// host identity and liveness. Nil without a database (configless App).
func batchNoticeStore(p batchCLIParams) *session.SessionNoticeStore {
	if p.owner == nil {
		return nil
	}
	if s, ok := batchNoticeStores.Load(p.owner()); ok {
		return s.(*session.SessionNoticeStore)
	}
	writer := batchCutoverWriter(p)
	if writer == nil {
		return nil
	}
	s := session.NewSessionNoticeStore(writer)
	s.SetReadConn(db.New(batchCutoverReader(p, writer)))
	if hs := batchHostStore(p); hs != nil {
		s.SetHostIDFunc(hs.HostID)
		s.SetHostNotDeadFunc(func(own, host string) bool {
			if host == "" {
				return false
			}
			if host == own {
				return true
			}
			// Fail-open on an unreadable probe (alive-or-unknown keeps the
			// notice), exactly like the legacy store's HostNotDead.
			st, err := session.ProbeHostShared(p.env.DataDir(), host)
			return err != nil || st != session.HostStatusDead
		})
	}
	actual, loaded := batchNoticeStores.LoadOrStore(p.owner(), s)
	if loaded {
		return actual.(*session.SessionNoticeStore)
	}
	return s
}

// batchOwner is the identity key of the package-level batch registries
// (batchHostStores, batchNoticeStores, batchMaintenanceLoops). Its pointer
// identity replaces the *App key; it carries no state because the registry
// code reads dataDir and the DB pools from the App at the same lazy moments
// as before. The blank byte is load-bearing: a zero-size struct makes every
// &batchOwner{} the same address (runtime.zerobase), so all Apps would share
// one registry key and a later App would adopt an earlier App's HostStore.
type batchOwner struct{ _ byte }

// batchOwnerHandle returns this App's registry key, creating it on first
// use. Safe for concurrent callers and for bare App literals; a nil App
// yields a nil key, as the nil *App key did before.
func (app *App) batchOwnerHandle() *batchOwner {
	if app == nil {
		return nil
	}
	if o := app.batchOwner.Load(); o != nil {
		return o
	}
	app.batchOwner.CompareAndSwap(nil, &batchOwner{})
	return app.batchOwner.Load()
}
