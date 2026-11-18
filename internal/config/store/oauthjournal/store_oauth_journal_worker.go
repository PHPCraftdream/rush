package oauthjournal

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

type Worker struct {
	Wake    chan struct{}
	Scopes  map[cfgmodel.Scope]uint64
	Attempt int
}

var (
	OAuthJournalBeforeWorker func(context.Context)
	OAuthJournalWorkerWait   = defaultWaitCodexRefresh
	OAuthJournalRecoveryHook func()
)

func RefreshLockPath(path, provider string) string {
	return docread.OAuthRefreshLockPath(path, provider)
}

// Cleanup also cancels and joins the OAuth disk sync worker.
func (s *Store) StopWorkers() {
	s.oauthWorkerMu.Lock()
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	s.oauthWorkerCancel()
	s.oauthWorkerMu.Unlock()
	s.oauthWorkerWG.Wait()
}

func (s *Store) StartWorker(scope cfgmodel.Scope, provider string) {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	if s.oauthWorkerCtx.Err() != nil {
		return
	}
	if worker := s.oauthWorkers[provider]; worker != nil {
		worker.Scopes[scope]++
		select {
		case worker.Wake <- struct{}{}:
		default:
		}
		return
	}
	if s.oauthWorkers == nil {
		s.oauthWorkers = make(map[string]*Worker)
	}
	worker := &Worker{Wake: make(chan struct{}, 1), Scopes: map[cfgmodel.Scope]uint64{scope: 1}}
	s.oauthWorkers[provider] = worker
	s.oauthWorkerWG.Add(1)
	go s.RunWorker(s.oauthWorkerCtx, provider, worker)
}

// Caller holds the refresh sidecar, never publishMu or diskWriteMu.
func (s *Store) RecoverCodexJournal(ctx context.Context, scope cfgmodel.Scope, provider string) (*oauth.Token, *docread.OAuthJournalRecord, error) {
	if OAuthJournalRecoveryHook != nil {
		OAuthJournalRecoveryHook()
	}
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return nil, nil, err
	}
	journalPath := slots.SelectOAuthJournalPath(path, provider, false)
	if entry := s.OutboxCurrent(path, provider); entry != nil {
		settled, settleErr := s.SettleOutbox(ctx, path, provider, entry)
		if settled {
			var pc cfgmodel.ProviderConfig
			_ = s.port.ViewSnap(func(v View) error {
				pc, _ = v.Config().Providers.Get(provider)
				return nil
			})
			return pc.OAuthToken, nil, nil
		}
		if !settled {
			var obsolete bool
			s.port.WithPublishMu(func() { obsolete = entry.Obsolete })
			if obsolete {
				return nil, &entry.Record, ErrOAuthJournalDisk
			}
			if ctx.Err() != nil {
				return nil, &entry.Record, ctx.Err()
			}
			if errors.Is(settleErr, s.port.ErrCredentialCAS()) {
				return nil, &entry.Record, settleErr
			}
			if settleErr != nil {
				disk, diskErr := ReadJournalDiskToken(path, provider)
				if Arbitrate(&entry.Record, nil, disk, diskErr) == OAuthJournalLogout {
					return nil, &entry.Record, ErrOAuthJournalDisk
				}
			}
			var live cfgmodel.ProviderConfig
			var ok bool
			_ = s.port.ViewSnap(func(v View) error {
				live, ok = v.Config().Providers.Get(provider)
				return nil
			})
			if !ok || live.OAuthToken == nil {
				return nil, &entry.Record, s.port.ErrCredentialCAS()
			}
			if !reflect.DeepEqual(live.OAuthToken, entry.Record.Token) {
				if !LivePredecessor(&entry.Record, live.OAuthToken) {
					return nil, &entry.Record, s.port.ErrCredentialCAS()
				}
				if err := s.port.ApplyOAuthTokenWithPendingPredecessor(provider, live.OAuthToken, entry.Record.Token, live.OAuthToken, true, &entry.Record); err != nil {
					return nil, &entry.Record, err
				}
			}
			if settleErr != nil {
				// Config uncertainty does not invalidate the owned live lineage.
				return entry.Record.Token, &entry.Record, ErrOAuthJournalDisk
			}
			return entry.Record.Token, &entry.Record, nil
		}
	}
	record, err := ReadJournalWithRetry(ctx, journalPath)
	if errors.Is(err, ErrOAuthJournalInvalid) {
		Quarantine(journalPath)
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, nil
	}
	var disk *oauth.Token
	decision := OAuthJournalError
	err = s.WithRecoveryWriteLock(ctx, path, func() error {
		var diskErr error
		disk, diskErr = ReadJournalDiskToken(path, provider)
		decision = Arbitrate(record, nil, disk, diskErr)
		switch decision {
		case OAuthJournalEqual, OAuthJournalDiskLaterOrTie, OAuthJournalLogout:
			return RemoveJournal(journalPath, path, provider, record, decision)
		case OAuthJournalError:
			return ErrOAuthJournalDisk
		}
		return nil
	})
	if err != nil && (errors.Is(err, ErrOAuthJournalRead) || errors.Is(err, ErrOAuthJournalInvalid)) {
		return nil, record, err
	}
	if ctx.Err() != nil {
		return nil, record, ctx.Err()
	}
	var live cfgmodel.ProviderConfig
	var liveOK bool
	_ = s.port.ViewSnap(func(v View) error {
		if v.Config() != nil && v.Config().Providers != nil {
			live, liveOK = v.Config().Providers.Get(provider)
		}
		return nil
	})
	var liveToken *oauth.Token
	if liveOK {
		liveToken = live.OAuthToken
	}
	if decision != OAuthJournalLogout && liveToken != nil && !reflect.DeepEqual(liveToken, record.Token) && !reflect.DeepEqual(liveToken, disk) && !LivePredecessor(record, liveToken) {
		return nil, record, s.port.ErrCredentialCAS()
	}
	if decision == OAuthJournalError || err != nil {
		if decision == OAuthJournalLogout {
			return nil, record, ErrOAuthJournalDisk
		}
		// Config uncertainty preserves B only in its matching live lineage.
		if liveToken != nil && (reflect.DeepEqual(liveToken, record.Token) || LivePredecessor(record, liveToken)) {
			var predecessor *oauth.Token
			if LivePredecessor(record, liveToken) {
				predecessor = liveToken
			}
			if applyErr := s.port.ApplyOAuthTokenWithPendingPredecessor(provider, liveToken, record.Token, predecessor, true, record); applyErr != nil {
				return nil, record, applyErr
			}
			return record.Token, record, ErrOAuthJournalDisk
		}
		return nil, record, ErrOAuthJournalDisk
	}
	switch decision {
	case OAuthJournalEqual, OAuthJournalDiskLaterOrTie, OAuthJournalLogout:
		if decision == OAuthJournalLogout && liveToken != nil {
			_ = s.port.PublishMutation(func(pub MutationScope) error {
				cur := pub.Current()
				pc, ok := cur.Config().Providers.Get(provider)
				if ok && reflect.DeepEqual(pc.OAuthToken, liveToken) {
					cfg := *cur.Config()
					providers := cur.Config().Providers.Copy()
					pc.OAuthToken, pc.APIKey = nil, ""
					providers[provider] = pc
					cfg.Providers = csync.NewMapFrom(providers)
					pub.Publish(&cfg)
				}
				return nil
			})
		}
		if disk != nil && liveToken != nil {
			_ = s.port.ApplyOAuthToken(provider, liveToken, disk)
		}
		s.port.WithPublishMu(func() {
			s.port.DeletePendingOAuthIfToken(provider, record.Token)
		})
		return disk, nil, nil
	default:
		// WAL-only recovery adds the disk predecessor's digest to the in-memory pending
		// record only; the journal file is not rewritten, so burned digests a former pending
		// record carried are not recovered here. Harm needs a distinct later-expiring token
		// that reuses the overwritten token's refresh token, which single-use rotation rules out.
		if liveToken != nil {
			if err := s.port.ApplyOAuthTokenWithPendingPredecessor(provider, liveToken, record.Token, disk, true, record); err != nil {
				return nil, record, err
			}
		}
		return record.Token, record, nil
	}
}

// Refresh sidecar -> diskWriteMu -> write sidecar; publishMu is never nested.
func (s *Store) WithRecoveryWriteLock(parent context.Context, path string, fn func() error) error {
	ctx, cancel := context.WithTimeout(parent, s.port.ConfigWriteLockTimeout())
	defer cancel()
	for !s.port.TryLockDiskWrite() {
		if err := s.port.WaitCodexRefresh(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	defer s.port.UnlockDiskWrite()
	canonical := docread.NormalizeReloadPath(path)
	lockAnchor := docread.NormalizeReloadPath(docread.ConfigWriteLockAnchor(canonical))
	lock, err := cfgcommit.AcquireConfigFileLock(ctx, lockAnchor+".lock")
	if err != nil {
		return err
	}
	defer cfgcommit.ReleaseConfigFileLock(lockAnchor+".lock", lock)
	if docread.NormalizeReloadPath(path) != canonical {
		return ErrOAuthJournalDisk
	}
	return fn()
}

func (s *Store) RecoverAtLoad() {
	for _, descriptor := range s.port.OAuthRotatingProviders() {
		if !descriptor.Rotating {
			continue
		}
		for _, scope := range []cfgmodel.Scope{cfgmodel.ScopeGlobal, cfgmodel.ScopeWorkspace} {
			path, err := s.port.ScopePath(scope)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.port.CodexRefreshLockWait())
			record, readErr := ReadJournalWithRetry(ctx, slots.SelectOAuthJournalPath(path, descriptor.ID, false))
			if readErr == nil && record == nil {
				cancel()
				continue
			}
			lockPath := RefreshLockPath(path, descriptor.ID)
			lock, err := cfgcommit.AcquireConfigFileLock(ctx, lockPath)
			if err == nil {
				_, record, recoveryErr := s.RecoverCodexJournal(ctx, scope, descriptor.ID)
				_ = cfgcommit.ReleaseConfigFileLock(lockPath, lock)
				if record != nil || recoveryErr != nil {
					s.StartWorker(scope, descriptor.ID)
				}
			} else {
				s.StartWorker(scope, descriptor.ID)
			}
			cancel()
		}
	}
}

// Refresh sidecars cover recovery/removal only, not config edits or waits.
func (s *Store) RunWorker(ctx context.Context, provider string, worker *Worker) {
	defer s.oauthWorkerWG.Done()
	defer func() {
		s.port.OAuthJournalWorkerDone(provider)
	}()
	defer func() {
		s.oauthWorkerMu.Lock()
		delete(s.oauthWorkers, provider)
		s.oauthWorkerMu.Unlock()
	}()
	if OAuthJournalBeforeWorker != nil {
		OAuthJournalBeforeWorker(ctx)
	}
	delay := 50 * time.Millisecond
	var lastLog time.Time
	for ctx.Err() == nil {
		s.oauthWorkerMu.Lock()
		Scopes := make(map[cfgmodel.Scope]uint64, len(worker.Scopes))
		for scope, generation := range worker.Scopes {
			Scopes[scope] = generation
		}
		s.oauthWorkerMu.Unlock()
		var err error
		for scope, generation := range Scopes {
			done, attemptErr := s.WorkerAttempt(ctx, scope, provider, worker.Attempt)
			worker.Attempt++
			if attemptErr != nil {
				err = attemptErr
			}
			if done {
				s.oauthWorkerMu.Lock()
				if worker.Scopes[scope] == generation {
					delete(worker.Scopes, scope)
				}
				s.oauthWorkerMu.Unlock()
			}
		}
		s.oauthWorkerMu.Lock()
		idle := len(worker.Scopes) == 0
		s.oauthWorkerMu.Unlock()
		if idle {
			s.port.OAuthJournalWorkerIdle(provider)
			// Idle ownership avoids a lost enqueue during worker termination.
			select {
			case <-ctx.Done():
				return
			case <-worker.Wake:
				continue
			}
		}
		if err != nil && (lastLog.IsZero() || time.Since(lastLog) >= 30*time.Second) {
			slog.Error("Failed to persist OAuth config; retrying", "failure", "storage")
			lastLog = time.Now()
		}
		if OAuthJournalWorkerWait(ctx, delay/2+time.Duration(rand.Int64N(int64(delay/2)+1))) != nil {
			return
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func (s *Store) WorkerAttempt(ctx context.Context, scope cfgmodel.Scope, provider string, Attempt int) (bool, error) {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return false, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, s.port.CodexRefreshLockWait())
	lockPath := RefreshLockPath(path, provider)
	lock, err := cfgcommit.AcquireConfigFileLock(lockCtx, lockPath)
	cancel()
	if err != nil {
		return false, err
	}
	// One journal Attempt precedes all general config locks and persistence.
	if err := s.FlushOutbox(ctx, path, provider); err != nil {
		_ = cfgcommit.ReleaseConfigFileLock(lockPath, lock)
		return false, err
	}
	_, record, recoveryErr := s.RecoverCodexJournal(ctx, scope, provider)
	_ = cfgcommit.ReleaseConfigFileLock(lockPath, lock)
	if record == nil {
		return recoveryErr == nil, recoveryErr
	}
	if recoveryErr != nil {
		return false, recoveryErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if beforePersist := s.port.CodexRefreshBeforePersist(); beforePersist != nil {
		if err := beforePersist(ctx, Attempt); err != nil {
			return false, err
		}
	}
	// Config CAS rereads under the write lock; no refresh lock is held here.
	pinned, err := s.port.ResolveConfigWriteTarget(path)
	if err != nil {
		return false, err
	}
	var committed bool
	err = s.port.WithCodexConfigWriteLock(ctx, path, pinned, func(target WriteTarget) error {
		latest, journalErr := ReadJournalWithRetry(ctx, slots.SelectOAuthJournalPath(path, provider, false))
		if journalErr != nil {
			return journalErr
		}
		if !reflect.DeepEqual(latest, record) {
			return s.port.ErrCredentialCAS()
		}
		disk, readErr := ReadJournalDiskToken(target.SelectedPath, provider)
		decision := Arbitrate(record, nil, disk, readErr)
		if decision != OAuthJournalPredecessor && decision != OAuthJournalLater {
			return s.port.ErrCredentialCAS()
		}
		// The expected predecessor is selected only under this write lock.
		var liveView View
		_ = s.port.ViewSnap(func(v View) error { liveView = v; return nil })
		if liveView.Config() == nil || liveView.Config().Providers == nil {
			return s.port.ErrCredentialCAS()
		}
		pc, ok := liveView.Config().Providers.Get(provider)
		if !ok || !reflect.DeepEqual(pc.OAuthToken, record.Token) {
			return s.port.ErrCredentialCAS()
		}
		var writeErr error
		committed, writeErr = s.port.SetOAuthTokenIfCurrentWithLock(scope, provider, disk, record.Token,
			func(_ string, fn func(WriteTarget) error) error { return fn(target) })
		if writeErr != nil {
			return writeErr
		}
		if !committed {
			return s.port.ErrCredentialCAS()
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if !committed {
		return false, errors.New("OAuth journal config changed")
	}
	return false, nil // Next recovery verifies durability and removes this record.
}

// StartDiskSync replicates the original disk-sync worker-state dance under
// the component worker mutex: ensure the shared worker context exists, refuse
// after a stop, register on the shared WaitGroup, and launch start with the
// live context. start must launch its goroutine and must not block.
func (s *Store) StartDiskSync(start func(context.Context)) {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	if s.oauthWorkerCtx.Err() != nil {
		return
	}
	s.oauthWorkerWG.Add(1)
	go func() {
		defer s.oauthWorkerWG.Done()
		start(s.oauthWorkerCtx)
	}()
}

// DiskSyncRunning reports whether the shared worker context is live.
func (s *Store) DiskSyncRunning() bool {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	return s.oauthWorkerCtx != nil && s.oauthWorkerCtx.Err() == nil
}

// WorkersCount reports the number of live journal workers (locked).
func (s *Store) WorkersCount() int {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	return len(s.oauthWorkers)
}

// WorkerFor returns the live worker for provider, or nil (locked).
func (s *Store) WorkerFor(provider string) *Worker {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	return s.oauthWorkers[provider]
}

// WorkerContext returns the shared worker context, or nil before the first
// worker/disk-sync start.
func (s *Store) WorkerContext() context.Context {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	return s.oauthWorkerCtx
}
