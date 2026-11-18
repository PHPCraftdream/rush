package oauthcred

import (
	"context"
	"math/rand/v2"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
)

var (
	oauthDiskSyncInterval = 5 * time.Second
	oauthDiskSyncStat     = os.Stat
	oauthDiskSyncWait     func(context.Context, time.Duration) error
)

type DiskSyncState struct {
	fi      os.FileInfo
	gen     uint64
	settled bool
	tick    uint64
}

// oauthDiskSyncMu guards oauthDiskSyncStarted; the worker mutex, context and
// WaitGroup live in the oauthjournal component.
var oauthDiskSyncMu sync.Mutex

// StartOAuthDiskSync starts one store-lifetime, read-only credential poller.
func (s *Store) StartOAuthDiskSync() {
	oauthDiskSyncMu.Lock()
	defer oauthDiskSyncMu.Unlock()
	if s.port.GlobalDataPath() == "" || s.oauthDiskSyncStarted {
		return
	}
	s.port.OAuthJournal().StartDiskSync(func(ctx context.Context) {
		s.oauthDiskSyncStarted = true
		go s.runOAuthDiskSync(ctx)
	})
}

func oauthDiskSyncDelay() time.Duration {
	spread := oauthDiskSyncInterval / 5
	return oauthDiskSyncInterval - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func (s *Store) runOAuthDiskSync(ctx context.Context) {
	var st DiskSyncState
	delay := time.Duration(rand.Int64N(int64(oauthDiskSyncInterval)))
	wait := oauthDiskSyncWait
	if wait == nil {
		wait = s.port.WaitCodexRefresh
	}
	for wait(ctx, delay) == nil {
		if ctx.Err() != nil {
			return
		}
		s.syncOAuthFromDisk(ctx, &st)
		delay = oauthDiskSyncDelay()
	}
}

func (s *Store) syncOAuthFromDisk(ctx context.Context, st *DiskSyncState) {
	if ctx.Err() != nil || s.port.GlobalDataPath() == "" {
		return
	}
	sn := s.port.LoadSnapshot()
	if sn.Config() == nil || sn.Config().Providers == nil {
		return
	}
	providers := sn.Config().Providers.Copy()
	eligible := false
	for id, pc := range providers {
		if _, known := s.oauthProvider(id); known && pc.OAuthToken != nil {
			eligible = true
			break
		}
	}
	if !eligible {
		return
	}
	st.tick++
	selected := docread.ReadSlotOf(s.port.GlobalDataPath())
	fi, err := oauthDiskSyncStat(selected)
	if err != nil {
		st.settled = false
		return
	}
	if st.tick%12 != 0 && st.settled && st.gen == sn.Generation() && st.fi != nil &&
		os.SameFile(st.fi, fi) && st.fi.Size() == fi.Size() && st.fi.ModTime().Equal(fi.ModTime()) {
		return
	}
	st.fi, st.gen, st.settled = fi, sn.Generation(), false
	doc, err := docread.ReadSettingsDocument(s.port.GlobalDataPath())
	if err != nil {
		st.settled = false
		return
	}
	data := doc.JSONBytes
	settled := true
	for id, pc := range providers {
		descriptor, known := s.oauthProvider(id)
		if !known || pc.OAuthToken == nil {
			continue
		}
		disk, decodeErr := oauthjournal.DecodeOAuthJournalDiskToken(data, id)
		if decodeErr != nil {
			settled = false
			continue
		}
		if disk == nil || reflect.DeepEqual(disk, pc.OAuthToken) {
			continue
		}
		if descriptor.rotating {
			// Foreign WAL authority is deferred until locked recovery settles it.
			_, _ = s.adoptCodexDiskAuthority(ctx, s.port.GlobalDataPath(), id, pc.OAuthToken, pc.OAuthToken, disk, nil, false)
		} else if legacyDiskTokenAdoptable(id, pc.OAuthToken, disk) {
			_ = s.applyOAuthToken(id, pc.OAuthToken, disk)
		}
		current := s.port.LoadSnapshot()
		if current.Config() == nil || current.Config().Providers == nil {
			settled = false
			continue
		}
		live, ok := current.Config().Providers.Get(id)
		if !ok || !reflect.DeepEqual(disk, live.OAuthToken) {
			settled = false
		}
	}
	// Concurrent publications force another check even after successful adoption.
	st.settled = settled && s.port.Generation() == sn.Generation()
}

// oauthOutboxDrainBudget bounds the synchronous outbox flush StopOAuthDiskSync
// performs after joining the workers, so shutdown cannot hang on an
// uncooperative lock.
const oauthOutboxDrainBudget = 5 * time.Second

// StopOAuthDiskSync cancels the poller and joins it together with any OAuth
// journal workers on the shared worker goroutines. It is idempotent, and a
// later StartOAuthDiskSync is refused.
func (s *Store) StopOAuthDiskSync() {
	s.port.OAuthJournal().StopWorkers()
	s.drainOAuthOutboxOnStop()
}

// drainOAuthOutboxOnStop flushes any rotation left in the in-memory outbox
// after the workers have joined. A journal worker cancelled before its first
// loop iteration never attempts its outbox entry, and a later start is
// refused on the cancelled context, so an unflushed rotation would lose its
// journal WAL at shutdown (review P2-4) while the provider has already
// burned the old refresh token. Each flush runs after the join, so it cannot
// race the workers, and is bounded by oauthOutboxDrainBudget.
func (s *Store) drainOAuthOutboxOnStop() {
	keys := s.port.OAuthJournal().OutboxKeysSorted()
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), oauthOutboxDrainBudget)
	defer cancel()
	for _, key := range keys {
		if ctx.Err() != nil {
			oauthjournal.LogOAuthJournalFailure(key.Path, "drain", "OAuth outbox drain exceeded its shutdown budget")
			return
		}
		lockPath := oauthjournal.RefreshLockPath(key.Path, key.Provider)
		lock, err := cfgcommit.AcquireConfigFileLock(ctx, lockPath)
		if err != nil {
			oauthjournal.LogOAuthJournalFailure(key.Path, "drain", "OAuth outbox drain could not take the refresh sidecar")
			continue
		}
		if err := s.port.OAuthJournal().FlushOutbox(ctx, key.Path, key.Provider); err != nil {
			oauthjournal.LogOAuthJournalFailure(key.Path, "drain", "OAuth outbox drain flush failed")
		}
		cfgcommit.ReleaseConfigFileLock(lockPath, lock)
	}
}

// OAuthDiskSyncRunning reports whether the OAuth disk-sync poller goroutine
// is running: started, not stopped, and not refused after a stop.
func (s *Store) OAuthDiskSyncRunning() bool {
	oauthDiskSyncMu.Lock()
	defer oauthDiskSyncMu.Unlock()
	return s.oauthDiskSyncStarted && s.port.OAuthJournal().DiskSyncRunning()
}
