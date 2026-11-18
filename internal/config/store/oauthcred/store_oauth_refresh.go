package oauthcred

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

// These package-private budgets are test seams. Override only in serial tests
// and restore after all refresh workers have joined.
var (
	codexRefreshWaiterHook  func()
	codexRefreshPollBudget  = 12 * time.Second
	codexRefreshPollInterval = 300 * time.Millisecond
)

type codexRefreshKey struct {
	store    *Store
	provider string
}

var codexRefreshActive = struct {
	sync.Mutex
	calls map[codexRefreshKey]chan struct{}
}{calls: make(map[codexRefreshKey]chan struct{})}

func validCodexToken(token *oauth.Token) bool {
	return token != nil && token.AccessToken != "" && !token.IsExpired()
}

// A different valid live token releases callers only when ownership permits it.
func (s *Store) codexRefreshAlreadyCurrent(ctx context.Context, scope cfgmodel.Scope, provider string, used *oauth.Token) bool {
	sn := s.port.LoadSnapshot()
	if sn.Config() == nil || sn.Config().Providers == nil {
		return false
	}
	pc, ok := sn.Config().Providers.Get(provider)
	if !ok || !validCodexToken(pc.OAuthToken) || reflect.DeepEqual(pc.OAuthToken, used) {
		return false
	}
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return true
	}
	record, predecessor := s.codexOAuthAuthorityRecord(path, provider)
	if record == nil {
		// Equal-live callers leave all WAL retries to locked recovery.
		record, _ = oauthjournal.ReadJournalWithRetry(ctx, slots.SelectOAuthJournalPath(path, provider, false))
	}
	if ctx.Err() != nil {
		return false
	}
	decision := oauthjournal.Arbitrate(record, predecessor, pc.OAuthToken, nil, pc.OAuthToken)
	return decision == oauthjournal.OAuthJournalEqual || decision == oauthjournal.OAuthJournalDiskLaterOrTie
}

// Snapshot ownership without consuming the recovery reader's retry attempts.
func (s *Store) codexOAuthAuthorityRecord(path, provider string) (*docread.OAuthJournalRecord, *oauth.Token) {
	s.port.LockPublish()
	entry := s.port.OAuthJournal().OutboxLookupLocked(path, provider)
	pending, hasPending := s.pendingOAuth[provider]
	var record *docread.OAuthJournalRecord
	if entry != nil {
		copyRecord := entry.Record
		record = &copyRecord
	} else if hasPending {
		copyRecord := pending.Record
		copyRecord.Token = &pending.Token
		record = &copyRecord
	}
	s.port.UnlockPublish()
	var predecessor *oauth.Token
	if hasPending && record != nil && reflect.DeepEqual(record.Token, &pending.Token) {
		predecessor = &pending.Predecessor
	}
	return record, predecessor
}

// Readable disk authority precedes memory-only release and journal recovery.
func (s *Store) preflightCodexDiskAuthority(ctx context.Context, scope cfgmodel.Scope, provider string, used *oauth.Token) (bool, error) {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return false, nil
	}
	live, ok := s.port.LoadSnapshot().Config().Providers.Get(provider)
	if !ok || live.OAuthToken == nil {
		return false, nil
	}
	disk, err := oauthjournal.ReadJournalDiskToken(path, provider)
	return s.adoptCodexDiskAuthority(ctx, path, provider, used, live.OAuthToken, disk, err, true)
}

// Adopt sampled disk authority without rereading configuration.
func (s *Store) adoptCodexDiskAuthority(ctx context.Context, path, provider string, used, expected, disk *oauth.Token, readErr error, logoutAllowed bool) (bool, error) {
	if readErr != nil {
		// Uncertain disk must not block an available different live token.
		return false, nil
	}
	record, predecessor := s.codexOAuthAuthorityRecord(path, provider)
	decision := oauthjournal.Arbitrate(record, predecessor, disk, nil, expected)
	if decision == oauthjournal.OAuthJournalLogout {
		if !logoutAllowed {
			return false, nil
		}
		return true, s.applyCodexDiskLogout(provider, expected)
	}
	if !validCodexToken(disk) || decision != oauthjournal.OAuthJournalDiskLaterOrTie {
		return false, nil
	}
	if record == nil {
		// Consult WAL only for a disk publication, preserving bounded read retries.
		journal, journalErr := oauthjournal.ReadJournalWithRetry(ctx, slots.SelectOAuthJournalPath(path, provider, false))
		if err := ctx.Err(); err != nil {
			return true, err
		}
		// Invalid bytes prove no valid owner; unreadable bytes remain uncertain.
		if journalErr != nil && !errors.Is(journalErr, oauthjournal.ErrOAuthJournalInvalid) {
			return false, nil
		}
		if journal != nil && oauthjournal.Arbitrate(journal, nil, disk, nil, expected) != oauthjournal.OAuthJournalDiskLaterOrTie {
			return false, nil
		}
	}
	if err := s.applyOAuthToken(provider, expected, disk); err != nil {
		return true, err
	}
	return !reflect.DeepEqual(disk, used), nil
}

func (s *Store) refreshCodexOAuthToken(ctx context.Context, scope cfgmodel.Scope, pc cfgmodel.ProviderConfig, client *http.Client) error {
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

func (s *Store) refreshCodexOAuthLeader(ctx context.Context, scope cfgmodel.Scope, pc cfgmodel.ProviderConfig, client *http.Client) error {
	used := pc.OAuthToken
	if done, err := s.preflightCodexDiskAuthority(ctx, scope, pc.ID, used); done || err != nil {
		return err
	}
	if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
		return nil
	}
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	// Refresh sidecar protects recovery/journal writes; recovery nests disk/write locks.
	// Background config edits release the refresh sidecar before taking write locks.
	lockPath := oauthjournal.RefreshLockPath(path, pc.ID)
	lockCtx, cancel := context.WithTimeout(ctx, s.port.CodexRefreshLockWait())
	defer cancel()
	if err := lockCtx.Err(); err != nil {
		return err
	}
	lock, err := cfgcommit.AcquireConfigFileLock(lockCtx, lockPath)
	if err != nil {
		return fmt.Errorf("lock Codex OAuth refresh: %w", err)
	}
	defer func() {
		if err := cfgcommit.ReleaseConfigFileLock(lockPath, lock); err != nil {
			slog.Error("Failed to release Codex OAuth refresh lock", "failure", "storage")
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	adopted, pending, recoveryErr := s.port.OAuthJournal().RecoverCodexJournal(ctx, scope, pc.ID)
	if pending != nil {
		s.port.OAuthJournal().StartWorker(scope, pc.ID)
	}
	if recoveryErr != nil && !errors.Is(recoveryErr, oauthjournal.ErrOAuthJournalDisk) {
		return recoveryErr
	}
	if adopted != nil {
		if validCodexToken(adopted) && !reflect.DeepEqual(adopted, used) {
			return nil
		}
		pc.OAuthToken = adopted
	}
	if live, ok := s.port.LoadSnapshot().Config().Providers.Get(pc.ID); !ok || live.OAuthToken == nil {
		return errOAuthCredentialCAS
	}
	if recoveryErr != nil && adopted == nil {
		return recoveryErr
	}
	if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, used) && adopted == nil {
		return nil
	}
	if adopted == nil {
		disk, diskErr := oauthjournal.ReadJournalDiskToken(path, pc.ID)
		live, ok := s.port.LoadSnapshot().Config().Providers.Get(pc.ID)
		if !ok || live.OAuthToken == nil {
			return errOAuthCredentialCAS
		}
		pc.OAuthToken = live.OAuthToken
		record, predecessor := s.codexOAuthAuthorityRecord(path, pc.ID)
		decision := oauthjournal.Arbitrate(record, predecessor, disk, diskErr, live.OAuthToken)
		if decision == oauthjournal.OAuthJournalLogout {
			return s.applyCodexDiskLogout(pc.ID, live.OAuthToken)
		}
		// A disk rotation that supersedes this caller's credential belongs to a
		// newer generation, so the caller's network client is stale. Require a
		// fresh snapshot/client pair instead of exchanging.
		if disk != nil && disk.AccessToken != "" && disk.RefreshToken != "" && disk.ExpiresAt > 0 &&
			!reflect.DeepEqual(disk, used) && reflect.DeepEqual(live.OAuthToken, disk) {
			return errOAuthCredentialCAS
		}
		// Expired disk rotations still supersede older live credentials.
		if disk != nil && disk.AccessToken != "" && disk.RefreshToken != "" && disk.ExpiresAt > 0 &&
			(decision == oauthjournal.OAuthJournalDiskLaterOrTie || decision == oauthjournal.OAuthJournalEqual && record == nil) {
			if err := s.applyOAuthToken(pc.ID, live.OAuthToken, disk); err != nil {
				return err
			}
			pc.OAuthToken = disk
			if validCodexToken(disk) && !reflect.DeepEqual(disk, used) {
				return nil
			}
		}
	}
	descriptor, ok := s.oauthProvider(pc.ID)
	if !ok || !descriptor.rotating {
		return fmt.Errorf("rotating OAuth refresh not supported for provider %s", pc.ID)
	}
	refreshed, err := descriptor.exchange(ctx, client, pc.OAuthToken.RefreshToken)
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
	refreshed = normalizeOAuthRefresh(descriptor, pc.OAuthToken, refreshed)
	if done, err := s.preflightCodexDiskAuthority(ctx, scope, pc.ID, used); done || err != nil {
		return err
	}
	// Install ownership before publication or I/O; failed CAS cannot lose rotation.
	record := &docread.OAuthJournalRecord{PredecessorRefreshTokenSHA256: oauthjournal.RotationDigest(pc.OAuthToken, refreshed), Token: refreshed}
	known, _ := s.codexOAuthAuthorityRecord(path, pc.ID)
	if known == nil {
		known = pending
	}
	if known != nil {
		copyRecord := oauthjournal.WithBurned(record, append([]string{known.PredecessorRefreshTokenSHA256}, known.BurnedRefreshTokenSHA256...)...)
		record = &copyRecord
	}
	entry := s.port.OAuthJournal().InstallOAuthOutbox(path, scope, pc.ID, record)
	applyErr := s.applyOAuthTokenWithPending(pc.ID, pc.OAuthToken, entry.Record.Token, true)
	journalPath := slots.SelectOAuthJournalPath(path, pc.ID, true)
	if err := oauthjournal.WriteJournalUntilDurable(ctx, journalPath, &entry.Record); err == nil {
		s.port.OAuthJournal().ClearOutbox(path, pc.ID, entry)
	} else {
		oauthjournal.LogOAuthJournalFailure(journalPath, "handoff", "OAuth journal write deferred to background persistence")
	}
	s.port.OAuthJournal().StartWorker(scope, pc.ID)
	if applyErr != nil && !s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
		return applyErr
	}
	return nil
}

// Publish proven disk logout only if the sampled live credential still matches.
func (s *Store) applyCodexDiskLogout(provider string, expected *oauth.Token) error {
	s.port.LockPublish()
	defer s.port.UnlockPublish()
	cur := s.port.LoadSnapshot()
	if cur.Config() == nil || cur.Config().Providers == nil {
		return errOAuthCredentialCAS
	}
	pc, ok := cur.Config().Providers.Get(provider)
	if !ok || !reflect.DeepEqual(pc.OAuthToken, expected) {
		return errOAuthCredentialCAS
	}
	next := cur.Clone()
	cfg := *cur.Config()
	providers := cur.Config().Providers.Copy()
	pc.OAuthToken, pc.APIKey = nil, ""
	providers[provider] = pc
	cfg.Providers = csync.NewMapFrom(providers)
	next.SetConfig(&cfg)
	s.port.PublishSnapshot(next)
	return nil
}

func (s *Store) pollCodexRefreshDisk(ctx context.Context, scope cfgmodel.Scope, pc cfgmodel.ProviderConfig, exchangeErr error) error {
	pollCtx, cancel := context.WithTimeout(ctx, codexRefreshPollBudget)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.codexRefreshAlreadyCurrent(ctx, scope, pc.ID, pc.OAuthToken) {
			return nil
		}
		path, pathErr := s.port.ScopePath(scope)
		if pathErr == nil {
			disk, diskErr := oauthjournal.ReadJournalDiskToken(path, pc.ID)
			live, ok := s.port.LoadSnapshot().Config().Providers.Get(pc.ID)
			if ok && live.OAuthToken != nil {
				record, predecessor := s.codexOAuthAuthorityRecord(path, pc.ID)
				decision := oauthjournal.Arbitrate(record, predecessor, disk, diskErr, live.OAuthToken)
				if decision == oauthjournal.OAuthJournalLogout {
					return s.applyCodexDiskLogout(pc.ID, live.OAuthToken)
				}
				if validCodexToken(disk) && (decision == oauthjournal.OAuthJournalDiskLaterOrTie ||
					decision == oauthjournal.OAuthJournalEqual && !reflect.DeepEqual(disk, pc.OAuthToken)) {
					return s.applyOAuthToken(pc.ID, live.OAuthToken, disk)
				}
			}
		}
		if err := s.port.WaitCodexRefresh(pollCtx, codexRefreshPollInterval); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", pc.ID, exchangeErr)
		}
	}
}

// Unlike the legacy write wrapper, the in-process mutex wait is cancelable.
// Keep the existing stable-target, commit and generation machinery intact.
func (s *Store) withCodexConfigWriteLock(ctx context.Context, path string, pinned WriteTarget, fn func(WriteTarget) error) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.port.ConfigWriteLockTimeout())
	defer cancel()
	for {
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if s.port.TryLockDiskWrite() {
			break
		}
		if err := s.port.WaitCodexRefresh(writeCtx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	defer s.port.UnlockDiskWrite()
	target, err := s.port.ResolveConfigWriteTarget(path)
	if err != nil {
		return err
	}
	if target.Path != pinned.Path || target.SelectedPath != pinned.SelectedPath {
		return fileid.ErrConfigCommitVerification()
	}
	if err := s.port.VerifyWriteTarget(pinned); err != nil {
		return err
	}
	lock, err := cfgcommit.AcquireConfigFileLock(writeCtx, target.LockPath)
	if err != nil {
		return err
	}
	defer cfgcommit.ReleaseConfigFileLock(target.LockPath, lock)
	if err := writeCtx.Err(); err != nil {
		return err
	}
	if err := s.port.VerifyWriteTarget(pinned); err != nil {
		return err
	}
	if err := fn(target); err != nil {
		return err
	}
	s.port.CommitDiskWrite()
	return nil
}
