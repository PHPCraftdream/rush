package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type oauthJournalWorker struct {
	wake    chan struct{}
	scopes  map[Scope]uint64
	attempt int
}

var (
	oauthJournalBeforeWorker func(context.Context)
	oauthJournalWorkerWait   = waitCodexRefresh
	oauthJournalWorkerDone   func(*ConfigStore, string)
	oauthJournalWorkerIdle   func(*ConfigStore, string)
	oauthJournalRecoveryHook func()
)

func oauthRefreshLockPath(path, provider string) string {
	digest := sha256.Sum256([]byte(provider))
	return fmt.Sprintf("%s.oauth-refresh-%x.lock", normalizeReloadPath(path), digest[:8])
}

// Cleanup also cancels and joins the OAuth disk sync worker.
func (s *ConfigStore) stopOAuthJournalWorkers() {
	s.oauthWorkerMu.Lock()
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	s.oauthWorkerCancel()
	s.oauthWorkerMu.Unlock()
	s.oauthWorkerWG.Wait()
}

func (s *ConfigStore) startOAuthJournalWorker(scope Scope, provider string) {
	s.oauthWorkerMu.Lock()
	defer s.oauthWorkerMu.Unlock()
	if s.oauthWorkerCtx == nil {
		s.oauthWorkerCtx, s.oauthWorkerCancel = context.WithCancel(context.Background())
	}
	if s.oauthWorkerCtx.Err() != nil {
		return
	}
	if worker := s.oauthWorkers[provider]; worker != nil {
		worker.scopes[scope]++
		select {
		case worker.wake <- struct{}{}:
		default:
		}
		return
	}
	if s.oauthWorkers == nil {
		s.oauthWorkers = make(map[string]*oauthJournalWorker)
	}
	worker := &oauthJournalWorker{wake: make(chan struct{}, 1), scopes: map[Scope]uint64{scope: 1}}
	s.oauthWorkers[provider] = worker
	s.oauthWorkerWG.Add(1)
	go s.runOAuthJournalWorker(s.oauthWorkerCtx, provider, worker)
}

// Caller holds the refresh sidecar, never publishMu or diskWriteMu.
func (s *ConfigStore) recoverCodexOAuthJournal(ctx context.Context, scope Scope, provider string) (*oauth.Token, *oauthJournalRecord, error) {
	if oauthJournalRecoveryHook != nil {
		oauthJournalRecoveryHook()
	}
	path, err := s.configPath(scope)
	if err != nil {
		return nil, nil, err
	}
	journalPath := oauthJournalPath(path, provider)
	if entry := s.oauthOutboxCurrent(path, provider); entry != nil {
		settled, settleErr := s.settleOAuthOutbox(ctx, path, provider, entry)
		if settled {
			pc, _ := s.loadSnapshot().config.Providers.Get(provider)
			return pc.OAuthToken, nil, nil
		}
		if !settled {
			s.publishMu.Lock()
			obsolete := entry.obsolete
			s.publishMu.Unlock()
			if obsolete {
				return nil, &entry.record, errOAuthJournalDisk
			}
			if ctx.Err() != nil {
				return nil, &entry.record, ctx.Err()
			}
			if errors.Is(settleErr, errOAuthCredentialCAS) {
				return nil, &entry.record, settleErr
			}
			if settleErr != nil {
				disk, diskErr := readOAuthJournalDiskToken(path, provider)
				if arbitrateOAuthJournal(&entry.record, nil, disk, diskErr) == oauthJournalLogout {
					return nil, &entry.record, errOAuthJournalDisk
				}
			}
			live, ok := s.loadSnapshot().config.Providers.Get(provider)
			if !ok || live.OAuthToken == nil {
				return nil, &entry.record, errOAuthCredentialCAS
			}
			if !reflect.DeepEqual(live.OAuthToken, entry.record.Token) {
				if !oauthJournalLivePredecessor(&entry.record, live.OAuthToken) {
					return nil, &entry.record, errOAuthCredentialCAS
				}
				if err := s.applyOAuthTokenWithPendingPredecessor(provider, live.OAuthToken, entry.record.Token, live.OAuthToken, true, &entry.record); err != nil {
					return nil, &entry.record, err
				}
			}
			if settleErr != nil {
				// Config uncertainty does not invalidate the owned live lineage.
				return entry.record.Token, &entry.record, errOAuthJournalDisk
			}
			return entry.record.Token, &entry.record, nil
		}
	}
	record, err := readOAuthJournalWithRetry(ctx, journalPath)
	if errors.Is(err, errOAuthJournalInvalid) {
		quarantineOAuthJournal(journalPath)
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, nil
	}
	var disk *oauth.Token
	decision := oauthJournalError
	err = s.withOAuthJournalRecoveryWriteLock(ctx, path, func() error {
		var diskErr error
		disk, diskErr = readOAuthJournalDiskToken(path, provider)
		decision = arbitrateOAuthJournal(record, nil, disk, diskErr)
		switch decision {
		case oauthJournalEqual, oauthJournalDiskLaterOrTie, oauthJournalLogout:
			return removeOAuthJournal(journalPath, path, provider, record, decision)
		case oauthJournalError:
			return errOAuthJournalDisk
		}
		return nil
	})
	if err != nil && (errors.Is(err, errOAuthJournalRead) || errors.Is(err, errOAuthJournalInvalid)) {
		return nil, record, err
	}
	if ctx.Err() != nil {
		return nil, record, ctx.Err()
	}
	current := s.loadSnapshot()
	var live *oauth.Token
	if current.config != nil && current.config.Providers != nil {
		pc, _ := current.config.Providers.Get(provider)
		live = pc.OAuthToken
	}
	if decision != oauthJournalLogout && live != nil && !reflect.DeepEqual(live, record.Token) && !reflect.DeepEqual(live, disk) && !oauthJournalLivePredecessor(record, live) {
		return nil, record, errOAuthCredentialCAS
	}
	if decision == oauthJournalError || err != nil {
		if decision == oauthJournalLogout {
			return nil, record, errOAuthJournalDisk
		}
		// Config uncertainty preserves B only in its matching live lineage.
		if live != nil && (reflect.DeepEqual(live, record.Token) || oauthJournalLivePredecessor(record, live)) {
			var predecessor *oauth.Token
			if oauthJournalLivePredecessor(record, live) {
				predecessor = live
			}
			if applyErr := s.applyOAuthTokenWithPendingPredecessor(provider, live, record.Token, predecessor, true, record); applyErr != nil {
				return nil, record, applyErr
			}
			return record.Token, record, errOAuthJournalDisk
		}
		return nil, record, errOAuthJournalDisk
	}
	switch decision {
	case oauthJournalEqual, oauthJournalDiskLaterOrTie, oauthJournalLogout:
		if decision == oauthJournalLogout && live != nil {
			s.publishMu.Lock()
			cur := s.loadSnapshot()
			pc, ok := cur.config.Providers.Get(provider)
			if ok && reflect.DeepEqual(pc.OAuthToken, live) {
				next := cur.clone()
				cfg := *cur.config
				providers := cur.config.Providers.Copy()
				pc.OAuthToken, pc.APIKey = nil, ""
				providers[provider] = pc
				cfg.Providers = csync.NewMapFrom(providers)
				next.config = &cfg
				s.publishLocked(next)
			}
			s.publishMu.Unlock()
		}
		if disk != nil && live != nil {
			_ = s.applyOAuthToken(provider, live, disk)
		}
		s.publishMu.Lock()
		if entry, ok := s.pendingOAuth[provider]; ok && reflect.DeepEqual(&entry.token, record.Token) {
			delete(s.pendingOAuth, provider)
		}
		s.publishMu.Unlock()
		return disk, nil, nil
	default:
		// WAL-only recovery adds the disk predecessor's digest to the in-memory pending
		// record only; the journal file is not rewritten, so burned digests a former pending
		// record carried are not recovered here. Harm needs a distinct later-expiring token
		// that reuses the overwritten token's refresh token, which single-use rotation rules out.
		if live != nil {
			if err := s.applyOAuthTokenWithPendingPredecessor(provider, live, record.Token, disk, true, record); err != nil {
				return nil, record, err
			}
		}
		return record.Token, record, nil
	}
}

// Refresh sidecar -> diskWriteMu -> write sidecar; publishMu is never nested.
func (s *ConfigStore) withOAuthJournalRecoveryWriteLock(parent context.Context, path string, fn func() error) error {
	ctx, cancel := context.WithTimeout(parent, configWriteLockTimeout)
	defer cancel()
	for !s.diskWriteMu.TryLock() {
		if err := waitCodexRefresh(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	defer s.diskWriteMu.Unlock()
	canonical := normalizeReloadPath(path)
	lock, err := acquireConfigFileLock(ctx, canonical+".lock")
	if err != nil {
		return err
	}
	defer releaseConfigFileLock(canonical+".lock", lock)
	if normalizeReloadPath(path) != canonical {
		return errOAuthJournalDisk
	}
	return fn()
}

func (s *ConfigStore) recoverOAuthJournalsAtLoad() {
	for _, descriptor := range oauthProviderDescriptors() {
		if !descriptor.rotating {
			continue
		}
		for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
			path, err := s.configPath(scope)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), codexRefreshLockWait)
			record, readErr := readOAuthJournalWithRetry(ctx, oauthJournalPath(path, descriptor.id))
			if readErr == nil && record == nil {
				cancel()
				continue
			}
			lockPath := oauthRefreshLockPath(path, descriptor.id)
			lock, err := acquireConfigFileLock(ctx, lockPath)
			if err == nil {
				_, record, recoveryErr := s.recoverCodexOAuthJournal(ctx, scope, descriptor.id)
				_ = releaseConfigFileLock(lockPath, lock)
				if record != nil || recoveryErr != nil {
					s.startOAuthJournalWorker(scope, descriptor.id)
				}
			} else {
				s.startOAuthJournalWorker(scope, descriptor.id)
			}
			cancel()
		}
	}
}

// Refresh sidecars cover recovery/removal only, not config edits or waits.
func (s *ConfigStore) runOAuthJournalWorker(ctx context.Context, provider string, worker *oauthJournalWorker) {
	defer s.oauthWorkerWG.Done()
	defer func() {
		if oauthJournalWorkerDone != nil {
			oauthJournalWorkerDone(s, provider)
		}
	}()
	defer func() {
		s.oauthWorkerMu.Lock()
		delete(s.oauthWorkers, provider)
		s.oauthWorkerMu.Unlock()
	}()
	if oauthJournalBeforeWorker != nil {
		oauthJournalBeforeWorker(ctx)
	}
	delay := 50 * time.Millisecond
	var lastLog time.Time
	for ctx.Err() == nil {
		s.oauthWorkerMu.Lock()
		scopes := make(map[Scope]uint64, len(worker.scopes))
		for scope, generation := range worker.scopes {
			scopes[scope] = generation
		}
		s.oauthWorkerMu.Unlock()
		var err error
		for scope, generation := range scopes {
			done, attemptErr := s.oauthJournalWorkerAttempt(ctx, scope, provider, worker.attempt)
			worker.attempt++
			if attemptErr != nil {
				err = attemptErr
			}
			if done {
				s.oauthWorkerMu.Lock()
				if worker.scopes[scope] == generation {
					delete(worker.scopes, scope)
				}
				s.oauthWorkerMu.Unlock()
			}
		}
		s.oauthWorkerMu.Lock()
		idle := len(worker.scopes) == 0
		s.oauthWorkerMu.Unlock()
		if idle {
			if oauthJournalWorkerIdle != nil {
				oauthJournalWorkerIdle(s, provider)
			}
			// Idle ownership avoids a lost enqueue during worker termination.
			select {
			case <-ctx.Done():
				return
			case <-worker.wake:
				continue
			}
		}
		if err != nil && (lastLog.IsZero() || time.Since(lastLog) >= 30*time.Second) {
			slog.Error("Failed to persist OAuth config; retrying", "failure", "storage")
			lastLog = time.Now()
		}
		if oauthJournalWorkerWait(ctx, delay/2+time.Duration(rand.Int64N(int64(delay/2)+1))) != nil {
			return
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func (s *ConfigStore) oauthJournalWorkerAttempt(ctx context.Context, scope Scope, provider string, attempt int) (bool, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return false, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, codexRefreshLockWait)
	lockPath := oauthRefreshLockPath(path, provider)
	lock, err := acquireConfigFileLock(lockCtx, lockPath)
	cancel()
	if err != nil {
		return false, err
	}
	// One journal attempt precedes all general config locks and persistence.
	if err := s.flushOAuthOutbox(ctx, path, provider); err != nil {
		_ = releaseConfigFileLock(lockPath, lock)
		return false, err
	}
	_, record, recoveryErr := s.recoverCodexOAuthJournal(ctx, scope, provider)
	_ = releaseConfigFileLock(lockPath, lock)
	if record == nil {
		return recoveryErr == nil, recoveryErr
	}
	if recoveryErr != nil {
		return false, recoveryErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if codexRefreshBeforePersist != nil {
		if err := codexRefreshBeforePersist(ctx, attempt); err != nil {
			return false, err
		}
	}
	// Config CAS rereads under the write lock; no refresh lock is held here.
	pinned, err := s.resolveConfigWriteTarget(path)
	if err != nil {
		return false, err
	}
	var committed bool
	err = s.withCodexConfigWriteLock(ctx, path, pinned, func(target configWriteTarget) error {
		latest, journalErr := readOAuthJournalWithRetry(ctx, oauthJournalPath(path, provider))
		if journalErr != nil {
			return journalErr
		}
		if !reflect.DeepEqual(latest, record) {
			return errOAuthCredentialCAS
		}
		disk, readErr := readOAuthJournalDiskToken(target.selectedPath, provider)
		decision := arbitrateOAuthJournal(record, nil, disk, readErr)
		if decision != oauthJournalPredecessor && decision != oauthJournalLater {
			return errOAuthCredentialCAS
		}
		// The expected predecessor is selected only under this write lock.
		live := s.loadSnapshot()
		if live.config == nil || live.config.Providers == nil {
			return errOAuthCredentialCAS
		}
		pc, ok := live.config.Providers.Get(provider)
		if !ok || !reflect.DeepEqual(pc.OAuthToken, record.Token) {
			return errOAuthCredentialCAS
		}
		var writeErr error
		committed, writeErr = s.setOAuthTokenIfCurrentWithLock(scope, provider, disk, record.Token,
			func(_ string, fn func(configWriteTarget) error) error { return fn(target) })
		if writeErr != nil {
			return writeErr
		}
		if !committed {
			return errOAuthCredentialCAS
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
