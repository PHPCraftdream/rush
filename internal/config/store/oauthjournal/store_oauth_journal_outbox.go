package oauthjournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

type Key struct {
	Path     string
	Provider string
}

type Entry struct {
	Scope    cfgmodel.Scope
	Record   docread.OAuthJournalRecord
	Obsolete bool // Guarded by publishMu; reload must not resurrect this entry.
}

// Entries are immutable; pointer identity fences completion of older rotations.
func (s *Store) InstallOAuthOutbox(path string, scope cfgmodel.Scope, provider string, record *docread.OAuthJournalRecord) *Entry {
	token := *record.Token
	var entry *Entry
	_ = s.port.PublishMutation(func(pub MutationScope) error {
		var digests []string
		digests = append(digests, pub.PendingOAuthDigests(provider)...)
		if previous := s.oauthOutbox[Key{Path: docread.NormalizeReloadPath(path), Provider: provider}]; previous != nil {
			digests = append(digests, previous.Record.PredecessorRefreshTokenSHA256)
			digests = append(digests, previous.Record.BurnedRefreshTokenSHA256...)
		}
		copyRecord := WithBurned(record, digests...)
		copyRecord.Token = &token
		entry = &Entry{Scope: scope, Record: copyRecord}
		if s.oauthOutbox == nil {
			s.oauthOutbox = make(map[Key]*Entry)
		}
		s.oauthOutbox[Key{Path: docread.NormalizeReloadPath(path), Provider: provider}] = entry
		return nil
	})
	return entry
}

func (s *Store) OutboxCurrent(path, provider string) *Entry {
	var current *Entry
	s.port.WithPublishMu(func() {
		current = s.oauthOutbox[Key{Path: docread.NormalizeReloadPath(path), Provider: provider}]
	})
	return current
}

func (s *Store) ClearOutbox(path, provider string, entry *Entry) {
	s.port.WithPublishMu(func() {
		key := Key{Path: docread.NormalizeReloadPath(path), Provider: provider}
		if s.oauthOutbox[key] == entry {
			delete(s.oauthOutbox, key)
		}
	})
}

// Refresh sidecar fences identity; publication never nests inside disk locks.
func (s *Store) SettleOutbox(ctx context.Context, path, provider string, entry *Entry) (bool, error) {
	initial, readErr := ReadJournalDiskToken(path, provider)
	decision := Arbitrate(&entry.Record, nil, initial, readErr)
	if decision != OAuthJournalEqual && decision != OAuthJournalDiskLaterOrTie && decision != OAuthJournalLogout {
		return false, nil
	}
	var disk *oauth.Token
	settled := false
	err := s.WithRecoveryWriteLock(ctx, path, func() error {
		var readErr error
		disk, readErr = ReadJournalDiskToken(path, provider)
		decision := Arbitrate(&entry.Record, nil, disk, readErr)
		if decision != OAuthJournalEqual && decision != OAuthJournalDiskLaterOrTie && decision != OAuthJournalLogout {
			return nil
		}
		if err := VerifyConfigDurable(path, provider, disk); err != nil {
			return err
		}
		settled = true
		return nil
	})
	if err != nil || !settled {
		return false, err
	}
	err = s.port.PublishMutation(func(pub MutationScope) error {
		key := Key{Path: docread.NormalizeReloadPath(path), Provider: provider}
		if s.oauthOutbox[key] != entry {
			return s.port.ErrCredentialCAS()
		}
		cur := pub.Current()
		pc, ok := cur.Config().Providers.Get(provider)
		if ok && (reflect.DeepEqual(pc.OAuthToken, entry.Record.Token) || reflect.DeepEqual(pc.OAuthToken, disk) || LivePredecessor(&entry.Record, pc.OAuthToken)) {
			cfg := *cur.Config()
			providers := cur.Config().Providers.Copy()
			if disk == nil {
				pc.OAuthToken, pc.APIKey = nil, ""
			} else {
				pc = s.port.TokenProviderConfig(provider, pc, disk)
			}
			providers[provider] = pc
			cfg.Providers = csync.NewMapFrom(providers)
			pub.Publish(&cfg)
		}
		delete(s.oauthOutbox, key)
		pub.DeletePendingOAuthIfToken(provider, entry.Record.Token)
		return nil
	})
	return true, err
}

// An external WAL must be synced and unchanged before replacing owned rotation.
func VerifyOutboxJournal(path string, record *docread.OAuthJournalRecord) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return ErrOAuthJournalDisk
	}
	defer file.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		return ErrOAuthJournalDisk
	}
	jsonBytes, decodeErr := docread.DecodeConfigDocument(path, before)
	var actual docread.OAuthJournalRecord
	if decodeErr != nil || json.Unmarshal(jsonBytes, &actual) != nil || !reflect.DeepEqual(&actual, record) {
		return ErrOAuthJournalDisk
	}
	info, err := file.Stat()
	if err != nil || file.Sync() != nil || file.Close() != nil || cfgcommit.SyncConfigParent(filepath.Dir(path)) != nil {
		return ErrOAuthJournalDisk
	}
	after, err := os.ReadFile(path)
	current, statErr := os.Stat(path)
	if err != nil || statErr != nil || !os.SameFile(info, current) || !bytes.Equal(before, after) {
		return ErrOAuthJournalDisk
	}
	return nil
}

// Owned rotations are anchored before config durability is checked.
func (s *Store) FlushOutbox(ctx context.Context, path, provider string) error {
	entry := s.OutboxCurrent(path, provider)
	if entry == nil {
		return nil
	}
	var obsolete bool
	s.port.WithPublishMu(func() { obsolete = entry.Obsolete })
	if obsolete {
		// Superseded ownership may settle, but must never rewrite its old WAL.
		if settled, err := s.SettleOutbox(ctx, path, provider, entry); err != nil || settled {
			return err
		}
		return ErrOAuthJournalDisk
	}
	journalPath := slots.SelectOAuthJournalPath(path, provider, true)
	latest, err := ReadJournal(journalPath)
	if errors.Is(err, ErrOAuthJournalInvalid) {
		// Owned rotation remains writable even if preservation fails.
		QuarantineOwned(journalPath)
	} else if err != nil {
		return err
	}
	if latest != nil && !reflect.DeepEqual(latest, &entry.Record) && Arbitrate(&entry.Record, nil, latest.Token, nil) == OAuthJournalDiskLaterOrTie {
		if err := VerifyOutboxJournal(journalPath, latest); err != nil {
			return err
		}
		// Preserve the external WAL while honoring verified config authority.
		if settled, err := s.SettleOutbox(ctx, path, provider, entry); err != nil || settled {
			return err
		}
		var liveCfg cfgmodel.ProviderConfig
		var ok bool
		_ = s.port.ViewSnap(func(v View) error {
			liveCfg, ok = v.Config().Providers.Get(provider)
			return nil
		})
		if !ok || liveCfg.OAuthToken == nil {
			return s.port.ErrCredentialCAS()
		}
		// Recovery lineage gates adoption; owned live was arbitrated above.
		if !reflect.DeepEqual(liveCfg.OAuthToken, latest.Token) && !reflect.DeepEqual(liveCfg.OAuthToken, entry.Record.Token) && !LivePredecessor(latest, liveCfg.OAuthToken) {
			return s.port.ErrCredentialCAS()
		}
		if err := s.port.ApplyOAuthTokenWithPendingPredecessor(provider, liveCfg.OAuthToken, latest.Token, liveCfg.OAuthToken, true, latest); err != nil {
			return err
		}
		s.ClearOutbox(path, provider, entry)
		return nil
	}
	if s.OutboxCurrent(path, provider) != entry {
		return s.port.ErrCredentialCAS()
	}
	if err := WriteJournalOnce(journalPath, &entry.Record); err != nil {
		return err
	}
	// Keep ownership through config CAS and any failed durability verification.
	_, err = s.SettleOutbox(ctx, path, provider, entry)
	return err
}

var OAuthJournalLogs = struct {
	sync.Mutex
	Last      map[LogKey]time.Time
	Aggregate time.Time
}{Last: make(map[LogKey]time.Time)}

const (
	oauthJournalLogCapacity = 1024
	oauthJournalLogWindow   = 30 * time.Second
)

type LogKey struct {
	Path string
	Kind string
}

func LogOAuthJournalFailure(path, kind, message string) {
	LogOAuthJournalFailureAt(path, kind, message, time.Now())
}

func LogOAuthJournalFailureAt(path, kind, message string, now time.Time) {
	key := LogKey{Path: docread.NormalizeReloadPath(path), Kind: kind}
	OAuthJournalLogs.Lock()
	prev := OAuthJournalLogs.Last[key]
	if !prev.IsZero() && now.Sub(prev) < oauthJournalLogWindow {
		OAuthJournalLogs.Unlock()
		return
	}
	if len(OAuthJournalLogs.Last) >= oauthJournalLogCapacity {
		for old, stamp := range OAuthJournalLogs.Last {
			if now.Sub(stamp) >= oauthJournalLogWindow {
				delete(OAuthJournalLogs.Last, old)
			}
		}
	}
	if _, exists := OAuthJournalLogs.Last[key]; !exists && len(OAuthJournalLogs.Last) >= oauthJournalLogCapacity {
		logAggregate := OAuthJournalLogs.Aggregate.IsZero() || now.Sub(OAuthJournalLogs.Aggregate) >= oauthJournalLogWindow
		if logAggregate {
			OAuthJournalLogs.Aggregate = now
		}
		OAuthJournalLogs.Unlock()
		if logAggregate {
			slog.Error("Additional OAuth journal failures suppressed while log limiter is saturated", "failure", kind)
		}
		return
	}
	OAuthJournalLogs.Last[key] = now
	OAuthJournalLogs.Unlock()
	slog.Error(message, "file", filepath.Base(path), "failure", kind)
}

// OutboxKeysSorted snapshots the outbox keys under the root publishMu and
// sorts them by path then provider — the drain's existing ordering.
func (s *Store) OutboxKeysSorted() []Key {
	var keys []Key
	s.port.WithPublishMu(func() {
		keys = make([]Key, 0, len(s.oauthOutbox))
		for key := range s.oauthOutbox {
			keys = append(keys, key)
		}
	})
	slices.SortFunc(keys, func(a, b Key) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Provider, b.Provider)
	})
	return keys
}

// OutboxEachLocked iterates the outbox; caller holds the root publishMu
// (plain guarded section only — no publishing inside fn).
func (s *Store) OutboxEachLocked(fn func(key Key, entry *Entry)) {
	for key, entry := range s.oauthOutbox {
		fn(key, entry)
	}
}

// OutboxLookupLocked returns the outbox entry for the normalized
// path/provider pair; caller holds the root publishMu.
func (s *Store) OutboxLookupLocked(path, provider string) *Entry {
	return s.oauthOutbox[Key{Path: docread.NormalizeReloadPath(path), Provider: provider}]
}
