package config

import (
	"context"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"
)

var (
	oauthDiskSyncInterval = 5 * time.Second
	oauthDiskSyncWait     = waitCodexRefresh
	oauthDiskSyncStat     = os.Stat
)

type oauthDiskSyncState struct {
	fi      os.FileInfo
	gen     uint64
	settled bool
	tick    uint64
}

// StartOAuthDiskSync starts one store-lifetime, read-only credential poller.
func (s *ConfigStore) StartOAuthDiskSync() {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	if s.globalDataPath == "" || s.oauthDiskSyncStarted {
		return
	}
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	if s.oauthWorkerCtx.Err() != nil {
		return
	}
	s.oauthDiskSyncStarted = true
	s.oauthWorkerWG.Add(1)
	go s.runOAuthDiskSync(s.oauthWorkerCtx)
}

func oauthDiskSyncDelay() time.Duration {
	spread := oauthDiskSyncInterval / 5
	return oauthDiskSyncInterval - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func (s *ConfigStore) runOAuthDiskSync(ctx context.Context) {
	defer s.oauthWorkerWG.Done()
	var st oauthDiskSyncState
	delay := time.Duration(rand.Int64N(int64(oauthDiskSyncInterval)))
	for oauthDiskSyncWait(ctx, delay) == nil {
		if ctx.Err() != nil {
			return
		}
		s.syncOAuthFromDisk(ctx, &st)
		delay = oauthDiskSyncDelay()
	}
}

func (s *ConfigStore) syncOAuthFromDisk(ctx context.Context, st *oauthDiskSyncState) {
	if ctx.Err() != nil || s.globalDataPath == "" {
		return
	}
	sn := s.loadSnapshot()
	if sn.config == nil || sn.config.Providers == nil {
		return
	}
	providers := sn.config.Providers.Copy()
	eligible := false
	for id, pc := range providers {
		if _, known := oauthProvider(id); known && pc.OAuthToken != nil {
			eligible = true
			break
		}
	}
	if !eligible {
		return
	}
	st.tick++
	fi, err := oauthDiskSyncStat(s.globalDataPath)
	if err != nil {
		st.settled = false
		return
	}
	if st.tick%12 != 0 && st.settled && st.gen == sn.generation && st.fi != nil &&
		os.SameFile(st.fi, fi) && st.fi.Size() == fi.Size() && st.fi.ModTime().Equal(fi.ModTime()) {
		return
	}
	st.fi, st.gen, st.settled = fi, sn.generation, false
	data, err := os.ReadFile(s.globalDataPath)
	if err != nil {
		return
	}
	settled := true
	for id, pc := range providers {
		descriptor, known := oauthProvider(id)
		if !known || pc.OAuthToken == nil {
			continue
		}
		disk, decodeErr := decodeOAuthJournalDiskToken(data, id)
		if decodeErr != nil {
			settled = false
			continue
		}
		if disk == nil || reflect.DeepEqual(disk, pc.OAuthToken) {
			continue
		}
		if descriptor.rotating {
			// Foreign WAL authority is deferred until locked recovery settles it.
			_, _ = s.adoptCodexDiskAuthority(ctx, s.globalDataPath, id, pc.OAuthToken, pc.OAuthToken, disk, nil, false)
		} else if legacyDiskTokenAdoptable(id, pc.OAuthToken, disk) {
			_ = s.applyOAuthToken(id, pc.OAuthToken, disk)
		}
		current := s.loadSnapshot()
		if current.config == nil || current.config.Providers == nil {
			settled = false
			continue
		}
		live, ok := current.config.Providers.Get(id)
		if !ok || !reflect.DeepEqual(disk, live.OAuthToken) {
			settled = false
		}
	}
	// Concurrent publications force another check even after successful adoption.
	st.settled = settled && s.Generation() == sn.generation
}

// oauthOutboxDrainBudget bounds the synchronous outbox flush StopOAuthDiskSync
// performs after joining the workers, so shutdown cannot hang on an
// uncooperative lock.
const oauthOutboxDrainBudget = 5 * time.Second

// StopOAuthDiskSync cancels the poller and joins it together with any OAuth
// journal workers on the shared worker goroutines. It is idempotent, and a
// later StartOAuthDiskSync is refused.
func (s *ConfigStore) StopOAuthDiskSync() {
	s.stopOAuthJournalWorkers()
	s.drainOAuthOutboxOnStop()
}

// drainOAuthOutboxOnStop flushes any rotation left in the in-memory outbox
// after the workers have joined. A journal worker cancelled before its first
// loop iteration never attempts its outbox entry, and a later start is
// refused on the cancelled context, so an unflushed rotation would lose its
// journal WAL at shutdown (review P2-4) while the provider has already
// burned the old refresh token. Each flush runs after the join, so it cannot
// race the workers, and is bounded by oauthOutboxDrainBudget.
func (s *ConfigStore) drainOAuthOutboxOnStop() {
	s.publishMu.Lock()
	keys := make([]oauthOutboxKey, 0, len(s.oauthOutbox))
	for key := range s.oauthOutbox {
		keys = append(keys, key)
	}
	s.publishMu.Unlock()
	if len(keys) == 0 {
		return
	}
	slices.SortFunc(keys, func(a, b oauthOutboxKey) int {
		if c := strings.Compare(a.path, b.path); c != 0 {
			return c
		}
		return strings.Compare(a.provider, b.provider)
	})
	ctx, cancel := context.WithTimeout(context.Background(), oauthOutboxDrainBudget)
	defer cancel()
	for _, key := range keys {
		if ctx.Err() != nil {
			logOAuthJournalFailure(key.path, "drain", "OAuth outbox drain exceeded its shutdown budget")
			return
		}
		lockPath := oauthRefreshLockPath(key.path, key.provider)
		lock, err := acquireConfigFileLock(ctx, lockPath)
		if err != nil {
			logOAuthJournalFailure(key.path, "drain", "OAuth outbox drain could not take the refresh sidecar")
			continue
		}
		if err := s.flushOAuthOutbox(ctx, key.path, key.provider); err != nil {
			logOAuthJournalFailure(key.path, "drain", "OAuth outbox drain flush failed")
		}
		releaseConfigFileLock(lockPath, lock)
	}
}

// OAuthDiskSyncRunning reports whether the OAuth disk-sync poller goroutine
// is running: started, not stopped, and not refused after a stop.
func (s *ConfigStore) OAuthDiskSyncRunning() bool {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	return s.oauthDiskSyncStarted && s.oauthWorkerCtx != nil && s.oauthWorkerCtx.Err() == nil
}
