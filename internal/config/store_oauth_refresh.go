package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

// These package-private budgets are test seams. Override only in serial tests
// and restore after all refresh workers have joined.
var (
	codexRefreshWaiterHook    func()
	codexRefreshBeforePersist func(context.Context, int) error
	codexRefreshPollWait      = waitCodexRefresh
	codexRefreshLockWait      = 30 * time.Second
	codexRefreshPollBudget    = 12 * time.Second
	codexRefreshPollInterval  = 300 * time.Millisecond
)

type codexRefreshKey struct {
	store    *ConfigStore
	provider string
}

var codexRefreshActive = struct {
	sync.Mutex
	calls map[codexRefreshKey]chan struct{}
}{calls: make(map[codexRefreshKey]chan struct{})}

func waitCodexRefresh(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func validCodexToken(token *oauth.Token) bool {
	return token != nil && token.AccessToken != "" && !token.IsExpired()
}

// A different valid live token releases callers only when ownership permits it.
func (s *ConfigStore) codexRefreshAlreadyCurrent(ctx context.Context, scope Scope, provider string, used *oauth.Token) bool {
	sn := s.loadSnapshot()
	if sn.config == nil || sn.config.Providers == nil {
		return false
	}
	pc, ok := sn.config.Providers.Get(provider)
	if !ok || !validCodexToken(pc.OAuthToken) || reflect.DeepEqual(pc.OAuthToken, used) {
		return false
	}
	path, err := s.configPath(scope)
	if err != nil {
		return true
	}
	record, predecessor := s.codexOAuthAuthorityRecord(path, provider)
	if record == nil {
		// Equal-live callers leave all WAL retries to locked recovery.
		record, _ = readOAuthJournalWithRetry(ctx, oauthJournalPath(path, provider))
	}
	if ctx.Err() != nil {
		return false
	}
	decision := arbitrateOAuthJournal(record, predecessor, pc.OAuthToken, nil, pc.OAuthToken)
	return decision == oauthJournalEqual || decision == oauthJournalDiskLaterOrTie
}

// Snapshot ownership without consuming the recovery reader's retry attempts.
func (s *ConfigStore) codexOAuthAuthorityRecord(path, provider string) (*oauthJournalRecord, *oauth.Token) {
	s.publishMu.Lock()
	entry := s.oauthOutbox[oauthOutboxKey{normalizeReloadPath(path), provider}]
	pending, hasPending := s.pendingOAuth[provider]
	var record *oauthJournalRecord
	if entry != nil {
		copyRecord := entry.record
		record = &copyRecord
	} else if hasPending {
		copyRecord := pending.record
		copyRecord.Token = &pending.token
		record = &copyRecord
	}
	s.publishMu.Unlock()
	var predecessor *oauth.Token
	if hasPending && record != nil && reflect.DeepEqual(record.Token, &pending.token) {
		predecessor = &pending.predecessor
	}
	return record, predecessor
}

// Readable disk authority precedes memory-only release and journal recovery.
func (s *ConfigStore) preflightCodexDiskAuthority(ctx context.Context, scope Scope, provider string, used *oauth.Token) (bool, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return false, nil
	}
	live, ok := s.loadSnapshot().config.Providers.Get(provider)
	if !ok || live.OAuthToken == nil {
		return false, nil
	}
	disk, err := readOAuthJournalDiskToken(path, provider)
	if err != nil {
		// Uncertain disk must not block an available different live token.
		return false, nil
	}
	record, predecessor := s.codexOAuthAuthorityRecord(path, provider)
	decision := arbitrateOAuthJournal(record, predecessor, disk, nil, live.OAuthToken)
	if decision == oauthJournalLogout {
		return true, s.applyCodexDiskLogout(provider, live.OAuthToken)
	}
	if !validCodexToken(disk) || decision != oauthJournalDiskLaterOrTie {
		return false, nil
	}
	if record == nil {
		// Consult WAL only for a disk publication, preserving bounded read retries.
		journal, journalErr := readOAuthJournalWithRetry(ctx, oauthJournalPath(path, provider))
		if err := ctx.Err(); err != nil {
			return true, err
		}
		// Invalid bytes prove no valid owner; unreadable bytes remain uncertain.
		if journalErr != nil && !errors.Is(journalErr, errOAuthJournalInvalid) {
			return false, nil
		}
		if journal != nil && arbitrateOAuthJournal(journal, nil, disk, nil, live.OAuthToken) != oauthJournalDiskLaterOrTie {
			return false, nil
		}
	}
	if err := s.applyOAuthToken(provider, live.OAuthToken, disk); err != nil {
		return true, err
	}
	return !reflect.DeepEqual(disk, used), nil
}

func (s *ConfigStore) refreshCodexOAuthToken(ctx context.Context, scope Scope, pc ProviderConfig, client *http.Client) error {
	key := codexRefreshKey{s, pc.ID}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if done, err := s.preflightCodexDiskAuthority(ctx, scope, pc.ID, pc.OAuthToken); done || err != nil {
			return err
		}
		if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
			return nil
		}
		codexRefreshActive.Lock()
		if done, busy := codexRefreshActive.calls[key]; busy {
			codexRefreshActive.Unlock()
			if codexRefreshWaiterHook != nil {
				codexRefreshWaiterHook()
			}
			// Recheck published memory without waiting for persistence.
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-done:
				timer.Stop()
			case <-timer.C:
			}
			continue // Re-evaluate; do not inherit another caller's error.
		}
		done := make(chan struct{})
		codexRefreshActive.calls[key] = done
		codexRefreshActive.Unlock()
		defer func() {
			codexRefreshActive.Lock()
			delete(codexRefreshActive.calls, key)
			close(done)
			codexRefreshActive.Unlock()
		}()
		return s.refreshCodexOAuthLeader(ctx, scope, pc, client)
	}
}

func (s *ConfigStore) refreshCodexOAuthLeader(ctx context.Context, scope Scope, pc ProviderConfig, client *http.Client) error {
	used := pc.OAuthToken
	if done, err := s.preflightCodexDiskAuthority(ctx, scope, pc.ID, used); done || err != nil {
		return err
	}
	if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
		return nil
	}
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	// Refresh sidecar protects recovery/journal writes; recovery nests disk/write locks.
	// Background config edits release the refresh sidecar before taking write locks.
	lockPath := oauthRefreshLockPath(path, pc.ID)
	lockCtx, cancel := context.WithTimeout(ctx, codexRefreshLockWait)
	defer cancel()
	if err := lockCtx.Err(); err != nil {
		return err
	}
	lock, err := acquireConfigFileLock(lockCtx, lockPath)
	if err != nil {
		return fmt.Errorf("lock Codex OAuth refresh: %w", err)
	}
	defer func() {
		if err := releaseConfigFileLock(lockPath, lock); err != nil {
			slog.Error("Failed to release Codex OAuth refresh lock", "failure", "storage")
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	adopted, pending, recoveryErr := s.recoverCodexOAuthJournal(ctx, scope, pc.ID)
	if pending != nil {
		s.startOAuthJournalWorker(scope, pc.ID)
	}
	if recoveryErr != nil && !errors.Is(recoveryErr, errOAuthJournalDisk) {
		return recoveryErr
	}
	if adopted != nil {
		if validCodexToken(adopted) && !reflect.DeepEqual(adopted, used) {
			return nil
		}
		pc.OAuthToken = adopted
	}
	if live, ok := s.loadSnapshot().config.Providers.Get(pc.ID); !ok || live.OAuthToken == nil {
		return errOAuthCredentialCAS
	}
	if recoveryErr != nil && adopted == nil {
		return recoveryErr
	}
	if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, used) && adopted == nil {
		return nil
	}
	if adopted == nil {
		disk, diskErr := readOAuthJournalDiskToken(path, pc.ID)
		live, ok := s.loadSnapshot().config.Providers.Get(pc.ID)
		if !ok || live.OAuthToken == nil {
			return errOAuthCredentialCAS
		}
		pc.OAuthToken = live.OAuthToken
		record, predecessor := s.codexOAuthAuthorityRecord(path, pc.ID)
		decision := arbitrateOAuthJournal(record, predecessor, disk, diskErr, live.OAuthToken)
		if decision == oauthJournalLogout {
			return s.applyCodexDiskLogout(pc.ID, live.OAuthToken)
		}
		// Expired disk rotations still supersede older live credentials.
		if disk != nil && disk.AccessToken != "" && disk.RefreshToken != "" && disk.ExpiresAt > 0 &&
			(decision == oauthJournalDiskLaterOrTie || decision == oauthJournalEqual && record == nil) {
			if err := s.applyOAuthToken(pc.ID, live.OAuthToken, disk); err != nil {
				return err
			}
			pc.OAuthToken = disk
			if validCodexToken(disk) && !reflect.DeepEqual(disk, used) {
				return nil
			}
		}
	}
	refreshed, err := codexRefreshTokenFn(ctx, client, pc.OAuthToken.RefreshToken)
	if err != nil {
		var rejected *oauth.TokenExchangeError
		if errors.As(err, &rejected) {
			return s.pollCodexRefreshDisk(ctx, scope, pc, err)
		}
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", pc.ID, err)
	}
	if !validCodexToken(refreshed) {
		return fmt.Errorf("OAuth refresh for provider %s returned no valid token", pc.ID)
	}
	copyToken := *refreshed
	if copyToken.AccountID == "" {
		copyToken.AccountID = pc.OAuthToken.AccountID
	}
	refreshed = &copyToken
	// Install ownership before publication or I/O; failed CAS cannot lose rotation.
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(pc.OAuthToken), Token: refreshed}
	known, _ := s.codexOAuthAuthorityRecord(path, pc.ID)
	if known == nil {
		known = pending
	}
	if known != nil {
		copyRecord := oauthJournalWithBurned(record, append([]string{known.PredecessorRefreshTokenSHA256}, known.BurnedRefreshTokenSHA256...)...)
		record = &copyRecord
	}
	entry := s.installOAuthOutbox(path, scope, pc.ID, record)
	applyErr := s.applyOAuthTokenWithPending(pc.ID, pc.OAuthToken, entry.record.Token, true)
	if err := writeOAuthJournalUntilDurable(ctx, oauthJournalPath(path, pc.ID), &entry.record); err == nil {
		s.clearOAuthOutbox(path, pc.ID, entry)
	} else {
		logOAuthJournalFailure(oauthJournalPath(path, pc.ID), "handoff", "OAuth journal write deferred to background persistence")
	}
	s.startOAuthJournalWorker(scope, pc.ID)
	if applyErr != nil && !s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
		return applyErr
	}
	return nil
}

// Publish proven disk logout only if the sampled live credential still matches.
func (s *ConfigStore) applyCodexDiskLogout(provider string, expected *oauth.Token) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	if cur.config == nil || cur.config.Providers == nil {
		return errOAuthCredentialCAS
	}
	pc, ok := cur.config.Providers.Get(provider)
	if !ok || !reflect.DeepEqual(pc.OAuthToken, expected) {
		return errOAuthCredentialCAS
	}
	next := cur.clone()
	cfg := *cur.config
	providers := cur.config.Providers.Copy()
	pc.OAuthToken, pc.APIKey = nil, ""
	providers[provider] = pc
	cfg.Providers = csync.NewMapFrom(providers)
	next.config = &cfg
	s.publishLocked(next)
	return nil
}

func (s *ConfigStore) pollCodexRefreshDisk(ctx context.Context, scope Scope, pc ProviderConfig, exchangeErr error) error {
	pollCtx, cancel := context.WithTimeout(ctx, codexRefreshPollBudget)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
			return nil
		}
		path, pathErr := s.configPath(scope)
		if pathErr == nil {
			disk, diskErr := readOAuthJournalDiskToken(path, pc.ID)
			live, ok := s.loadSnapshot().config.Providers.Get(pc.ID)
			if ok && live.OAuthToken != nil {
				record, predecessor := s.codexOAuthAuthorityRecord(path, pc.ID)
				decision := arbitrateOAuthJournal(record, predecessor, disk, diskErr, live.OAuthToken)
				if decision == oauthJournalLogout {
					return s.applyCodexDiskLogout(pc.ID, live.OAuthToken)
				}
				if validCodexToken(disk) && (decision == oauthJournalDiskLaterOrTie ||
					decision == oauthJournalEqual && !reflect.DeepEqual(disk, pc.OAuthToken)) {
					return s.applyOAuthToken(pc.ID, live.OAuthToken, disk)
				}
			}
		}
		if err := codexRefreshPollWait(pollCtx, codexRefreshPollInterval); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", pc.ID, exchangeErr)
		}
	}
}

// Unlike the legacy write wrapper, the in-process mutex wait is cancelable.
// Keep the existing stable-target, commit and generation machinery intact.
func (s *ConfigStore) withCodexConfigWriteLock(ctx context.Context, path string, pinned configWriteTarget, fn func(configWriteTarget) error) error {
	writeCtx, cancel := context.WithTimeout(ctx, configWriteLockTimeout)
	defer cancel()
	for {
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if s.diskWriteMu.TryLock() {
			break
		}
		if err := waitCodexRefresh(writeCtx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	defer s.diskWriteMu.Unlock()
	target, err := s.resolveConfigWriteTarget(path)
	if err != nil {
		return err
	}
	if target.path != pinned.path || target.selectedPath != pinned.selectedPath {
		return errConfigCommitVerification
	}
	if err := verifyConfigWriteTarget(pinned); err != nil {
		return err
	}
	lock, err := acquireConfigFileLock(writeCtx, target.lockPath)
	if err != nil {
		return err
	}
	defer releaseConfigFileLock(target.lockPath, lock)
	if err := writeCtx.Err(); err != nil {
		return err
	}
	if err := verifyConfigWriteTarget(pinned); err != nil {
		return err
	}
	if err := fn(target); err != nil {
		return err
	}
	s.diskWriteGeneration++
	return nil
}
