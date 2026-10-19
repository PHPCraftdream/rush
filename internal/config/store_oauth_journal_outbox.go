package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type oauthOutboxKey struct {
	path     string
	provider string
}

type oauthOutboxEntry struct {
	scope    Scope
	record   oauthJournalRecord
	obsolete bool // Guarded by publishMu; reload must not resurrect this entry.
}

// Entries are immutable; pointer identity fences completion of older rotations.
func (s *ConfigStore) installOAuthOutbox(path string, scope Scope, provider string, record *oauthJournalRecord) *oauthOutboxEntry {
	token := *record.Token
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	var digests []string
	if pending, ok := s.pendingOAuth[provider]; ok {
		digests = append(digests, pending.record.PredecessorRefreshTokenSHA256)
		digests = append(digests, pending.record.BurnedRefreshTokenSHA256...)
	}
	if previous := s.oauthOutbox[oauthOutboxKey{normalizeReloadPath(path), provider}]; previous != nil {
		digests = append(digests, previous.record.PredecessorRefreshTokenSHA256)
		digests = append(digests, previous.record.BurnedRefreshTokenSHA256...)
	}
	copyRecord := oauthJournalWithBurned(record, digests...)
	copyRecord.Token = &token
	entry := &oauthOutboxEntry{scope: scope, record: copyRecord}
	if s.oauthOutbox == nil {
		s.oauthOutbox = make(map[oauthOutboxKey]*oauthOutboxEntry)
	}
	s.oauthOutbox[oauthOutboxKey{normalizeReloadPath(path), provider}] = entry
	return entry
}

func (s *ConfigStore) oauthOutboxCurrent(path, provider string) *oauthOutboxEntry {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.oauthOutbox[oauthOutboxKey{normalizeReloadPath(path), provider}]
}

func (s *ConfigStore) clearOAuthOutbox(path, provider string, entry *oauthOutboxEntry) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	key := oauthOutboxKey{normalizeReloadPath(path), provider}
	if s.oauthOutbox[key] == entry {
		delete(s.oauthOutbox, key)
	}
}

// Refresh sidecar fences identity; publication never nests inside disk locks.
func (s *ConfigStore) settleOAuthOutbox(ctx context.Context, path, provider string, entry *oauthOutboxEntry) (bool, error) {
	initial, readErr := readOAuthJournalDiskToken(path, provider)
	decision := arbitrateOAuthJournal(&entry.record, nil, initial, readErr)
	if decision != oauthJournalEqual && decision != oauthJournalDiskLaterOrTie && decision != oauthJournalLogout {
		return false, nil
	}
	var disk *oauth.Token
	settled := false
	err := s.withOAuthJournalRecoveryWriteLock(ctx, path, func() error {
		var readErr error
		disk, readErr = readOAuthJournalDiskToken(path, provider)
		decision := arbitrateOAuthJournal(&entry.record, nil, disk, readErr)
		if decision != oauthJournalEqual && decision != oauthJournalDiskLaterOrTie && decision != oauthJournalLogout {
			return nil
		}
		if err := verifyOAuthJournalConfigDurable(path, provider, disk); err != nil {
			return err
		}
		settled = true
		return nil
	})
	if err != nil || !settled {
		return false, err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	key := oauthOutboxKey{normalizeReloadPath(path), provider}
	if s.oauthOutbox[key] != entry {
		return false, errOAuthCredentialCAS
	}
	cur := s.loadSnapshot()
	pc, ok := cur.config.Providers.Get(provider)
	if ok && (reflect.DeepEqual(pc.OAuthToken, entry.record.Token) || reflect.DeepEqual(pc.OAuthToken, disk) || oauthJournalLivePredecessor(&entry.record, pc.OAuthToken)) {
		next := cur.clone()
		cfg := *cur.config
		providers := cur.config.Providers.Copy()
		if disk == nil {
			pc.OAuthToken, pc.APIKey = nil, ""
		} else {
			pc = oauthTokenProviderConfig(provider, pc, disk)
		}
		providers[provider] = pc
		cfg.Providers = csync.NewMapFrom(providers)
		next.config = &cfg
		s.publishLocked(next)
	}
	delete(s.oauthOutbox, key)
	if pending, ok := s.pendingOAuth[provider]; ok && reflect.DeepEqual(&pending.token, entry.record.Token) {
		delete(s.pendingOAuth, provider)
	}
	return true, nil
}

// An external WAL must be synced and unchanged before replacing owned rotation.
func verifyOAuthOutboxJournal(path string, record *oauthJournalRecord) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return errOAuthJournalDisk
	}
	defer file.Close()
	before, err := os.ReadFile(path)
	var actual oauthJournalRecord
	if err != nil || json.Unmarshal(before, &actual) != nil || !reflect.DeepEqual(&actual, record) {
		return errOAuthJournalDisk
	}
	info, err := file.Stat()
	if err != nil || file.Sync() != nil || file.Close() != nil || syncConfigParent(filepath.Dir(path)) != nil {
		return errOAuthJournalDisk
	}
	after, err := os.ReadFile(path)
	current, statErr := os.Stat(path)
	if err != nil || statErr != nil || !os.SameFile(info, current) || !bytes.Equal(before, after) {
		return errOAuthJournalDisk
	}
	return nil
}

// Owned rotations are anchored before config durability is checked.
func (s *ConfigStore) flushOAuthOutbox(ctx context.Context, path, provider string) error {
	entry := s.oauthOutboxCurrent(path, provider)
	if entry == nil {
		return nil
	}
	s.publishMu.Lock()
	obsolete := entry.obsolete
	s.publishMu.Unlock()
	if obsolete {
		// Superseded ownership may settle, but must never rewrite its old WAL.
		if settled, err := s.settleOAuthOutbox(ctx, path, provider, entry); err != nil || settled {
			return err
		}
		return errOAuthJournalDisk
	}
	journalPath := oauthJournalPath(path, provider)
	latest, err := readOAuthJournal(journalPath)
	if errors.Is(err, errOAuthJournalInvalid) {
		// Owned rotation remains writable even if preservation fails.
		quarantineOwnedOAuthJournal(journalPath)
	} else if err != nil {
		return err
	}
	if latest != nil && !reflect.DeepEqual(latest, &entry.record) && arbitrateOAuthJournal(&entry.record, nil, latest.Token, nil) == oauthJournalDiskLaterOrTie {
		if err := verifyOAuthOutboxJournal(oauthJournalPath(path, provider), latest); err != nil {
			return err
		}
		// Preserve the external WAL while honoring verified config authority.
		if settled, err := s.settleOAuthOutbox(ctx, path, provider, entry); err != nil || settled {
			return err
		}
		live, ok := s.loadSnapshot().config.Providers.Get(provider)
		if !ok || live.OAuthToken == nil {
			return errOAuthCredentialCAS
		}
		// Recovery lineage gates adoption; owned live was arbitrated above.
		if !reflect.DeepEqual(live.OAuthToken, latest.Token) && !reflect.DeepEqual(live.OAuthToken, entry.record.Token) && !oauthJournalLivePredecessor(latest, live.OAuthToken) {
			return errOAuthCredentialCAS
		}
		if err := s.applyOAuthTokenWithPendingPredecessor(provider, live.OAuthToken, latest.Token, live.OAuthToken, true, latest); err != nil {
			return err
		}
		s.clearOAuthOutbox(path, provider, entry)
		return nil
	}
	if s.oauthOutboxCurrent(path, provider) != entry {
		return errOAuthCredentialCAS
	}
	if err := writeOAuthJournalOnce(journalPath, &entry.record); err != nil {
		return err
	}
	// Keep ownership through config CAS and any failed durability verification.
	_, err = s.settleOAuthOutbox(ctx, path, provider, entry)
	return err
}

var oauthJournalLogs = struct {
	sync.Mutex
	last      map[oauthJournalLogKey]time.Time
	aggregate time.Time
}{last: make(map[oauthJournalLogKey]time.Time)}

const (
	oauthJournalLogCapacity = 1024
	oauthJournalLogWindow   = 30 * time.Second
)

type oauthJournalLogKey struct {
	path string
	kind string
}

func logOAuthJournalFailure(path, kind, message string) {
	logOAuthJournalFailureAt(path, kind, message, time.Now())
}

func logOAuthJournalFailureAt(path, kind, message string, now time.Time) {
	key := oauthJournalLogKey{normalizeReloadPath(path), kind}
	oauthJournalLogs.Lock()
	last := oauthJournalLogs.last[key]
	if !last.IsZero() && now.Sub(last) < oauthJournalLogWindow {
		oauthJournalLogs.Unlock()
		return
	}
	if len(oauthJournalLogs.last) >= oauthJournalLogCapacity {
		for old, stamp := range oauthJournalLogs.last {
			if now.Sub(stamp) >= oauthJournalLogWindow {
				delete(oauthJournalLogs.last, old)
			}
		}
	}
	if _, exists := oauthJournalLogs.last[key]; !exists && len(oauthJournalLogs.last) >= oauthJournalLogCapacity {
		logAggregate := oauthJournalLogs.aggregate.IsZero() || now.Sub(oauthJournalLogs.aggregate) >= oauthJournalLogWindow
		if logAggregate {
			oauthJournalLogs.aggregate = now
		}
		oauthJournalLogs.Unlock()
		if logAggregate {
			slog.Error("Additional OAuth journal failures suppressed while log limiter is saturated", "failure", kind)
		}
		return
	}
	oauthJournalLogs.last[key] = now
	oauthJournalLogs.Unlock()
	slog.Error(message, "file", filepath.Base(path), "failure", kind)
}
