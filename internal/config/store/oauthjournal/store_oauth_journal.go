package oauthjournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

var (
	ErrOAuthJournalRead    = errors.New("OAuth journal cannot be read")
	ErrOAuthJournalDisk    = errors.New("OAuth config cannot be verified")
	ErrOAuthJournalInvalid = docread.ErrOAuthJournalInvalid()
	// Serial test seams must be restored only after their users have joined.
	OAuthJournalReadWait     = defaultWaitCodexRefresh
	OAuthJournalReadBackoffs = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	OAuthJournalReadFile     = os.ReadFile
	OAuthJournalAtomicWrite  = cfgcommit.AtomicWriteFile
	OAuthJournalRetryWait    = defaultWaitCodexRefresh
	// Serial tests shrink the future synchronous write budget before workers start.
	OAuthJournalWriteBudget = 5 * time.Second
)

// defaultWaitCodexRefresh mirrors the root store's waitCodexRefresh: a plain
// interruptible sleep. Free functions cannot reach the port, so the wait
// seams default to this; the root wiring assigns its own implementation.
func defaultWaitCodexRefresh(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

type Decision uint8

const (
	OAuthJournalError Decision = iota
	OAuthJournalEqual
	OAuthJournalPredecessor
	OAuthJournalDiskLaterOrTie
	OAuthJournalLater
	OAuthJournalLogout
)

// Record is the on-disk OAuth journal document.
type Record = docread.OAuthJournalRecord

func RefreshTokenSHA256(token *oauth.Token) string {
	if token == nil {
		return ""
	}
	digest := sha256.Sum256([]byte(token.RefreshToken))
	return hex.EncodeToString(digest[:])
}

func RotationDigest(previous, token *oauth.Token) string {
	if previous == nil || token == nil || previous.RefreshToken == "" || token.RefreshToken == "" || previous.RefreshToken == token.RefreshToken {
		return ""
	}
	return RefreshTokenSHA256(previous)
}

// Unchanged credentials establish lineage without claiming a burned token.
func LivePredecessor(record *docread.OAuthJournalRecord, live *oauth.Token) bool {
	if record == nil || live == nil {
		return false
	}
	if record.PredecessorRefreshTokenSHA256 != "" {
		// A digest equal to the record token's own refresh digest could only come from an
		// untagged build between 15ef615e and 9e62125f and is cleared by any later recovery,
		// so it is not normalized on read.
		return RefreshTokenSHA256(live) == record.PredecessorRefreshTokenSHA256
	}
	// Same-refresh generations are ordered only by the arbiter's expiry rule.
	return record.Token != nil && live.RefreshToken != "" && live.RefreshToken == record.Token.RefreshToken && Arbitrate(record, nil, live, nil) == OAuthJournalLater
}

// The legacy primary digest and optional additional digests form one set.
func WithBurned(record *docread.OAuthJournalRecord, digests ...string) docread.OAuthJournalRecord {
	copyRecord := *record
	successor := RefreshTokenSHA256(record.Token)
	if copyRecord.PredecessorRefreshTokenSHA256 == successor {
		copyRecord.PredecessorRefreshTokenSHA256 = ""
	}
	copyRecord.BurnedRefreshTokenSHA256 = nil
	for _, digest := range append(append([]string(nil), record.BurnedRefreshTokenSHA256...), digests...) {
		if digest != "" && digest != successor && digest != copyRecord.PredecessorRefreshTokenSHA256 && !slices.Contains(copyRecord.BurnedRefreshTokenSHA256, digest) {
			copyRecord.BurnedRefreshTokenSHA256 = append(copyRecord.BurnedRefreshTokenSHA256, digest)
		}
	}
	return copyRecord
}

// Anchor selection never reads config contents, including malformed JSON.
func JournalPath(configPath, providerID string) string {
	return docread.NormalizeReloadPath(configPath) + ".oauth-pending-" + docread.OAuthJournalDigest(providerID) + ".json"
}

// Missing journals are normal; unreadable or corrupt journals are kept.
func ReadJournal(path string) (*docread.OAuthJournalRecord, error) {
	data, err := OAuthJournalReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		LogOAuthJournalFailure(path, "read", "Failed to read OAuth journal")
		return nil, ErrOAuthJournalRead
	}
	return docread.ParseOAuthJournal(path, data)
}

// Bounded leader retries hand remaining durability work to the store outbox.
func WriteJournalUntilDurable(parent context.Context, path string, record *docread.OAuthJournalRecord) error {
	ctx, cancel := context.WithTimeout(parent, OAuthJournalWriteBudget)
	defer cancel()
	delay := 50 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := WriteJournalOnce(path, record); err == nil {
			return nil
		} else if errors.Is(err, ErrOAuthJournalInvalid) {
			return err
		}
		if err := OAuthJournalRetryWait(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// A worker attempts one write without waiting under its refresh sidecar.
func WriteJournalOnce(path string, record *docread.OAuthJournalRecord) error {
	if !docread.ValidOAuthJournal(record) {
		return ErrOAuthJournalInvalid
	}
	data, err := json.Marshal(record)
	if err != nil {
		return ErrOAuthJournalInvalid
	}
	data, encErr := docread.EncodeSettingsDocument(data, docread.SettingsDocumentFormatOf(path))
	if encErr != nil {
		return ErrOAuthJournalInvalid
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = OAuthJournalAtomicWrite(path, data, 0o600)
	}
	if err != nil {
		return ErrOAuthJournalDisk
	}
	return nil
}

// A successful read of a valid document is required to establish logout.
func ReadJournalDiskToken(path, providerID string) (*oauth.Token, error) {
	doc, err := docread.ReadSettingsDocument(path)
	if err != nil {
		return nil, ErrOAuthJournalDisk
	}
	return DecodeOAuthJournalDiskToken(doc.JSONBytes, providerID)
}

// Order: uncertainty, proven logout, equal, burned predecessor, then expiry.
// Without ownership, live is the reference and no predecessor digest is used.
func Arbitrate(record *docread.OAuthJournalRecord, predecessor, disk *oauth.Token, diskErr error, live ...*oauth.Token) Decision {
	if diskErr != nil {
		return OAuthJournalError
	}
	if disk == nil {
		return OAuthJournalLogout
	}
	var reference *oauth.Token
	if record != nil {
		reference = record.Token
	} else if len(live) > 0 {
		reference = live[0]
	}
	if reference == nil {
		return OAuthJournalError
	}
	if reflect.DeepEqual(disk, reference) {
		return OAuthJournalEqual
	}
	if predecessor != nil && RotationDigest(predecessor, reference) != "" && reflect.DeepEqual(disk, predecessor) ||
		record != nil && (record.PredecessorRefreshTokenSHA256 != "" && RefreshTokenSHA256(disk) == record.PredecessorRefreshTokenSHA256 || slices.Contains(record.BurnedRefreshTokenSHA256, RefreshTokenSHA256(disk))) {
		return OAuthJournalPredecessor
	}
	if disk.ExpiresAt >= reference.ExpiresAt {
		return OAuthJournalDiskLaterOrTie
	}
	return OAuthJournalLater
}

// Caller holds refresh/config locks; equal bytes alone do not prove durability.
func VerifyConfigDurable(path, providerID string, token *oauth.Token) error {
	physical := docread.ReadSlotOf(path)
	file, err := os.OpenFile(physical, os.O_RDWR, 0)
	if err != nil {
		return ErrOAuthJournalDisk
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return ErrOAuthJournalDisk
	}
	jsonBytes, decodeErr := docread.DecodeConfigDocument(physical, data)
	if decodeErr != nil {
		return ErrOAuthJournalDisk
	}
	disk, err := DecodeOAuthJournalDiskToken(jsonBytes, providerID)
	if err != nil || !reflect.DeepEqual(disk, token) {
		return ErrOAuthJournalDisk
	}
	info, err := file.Stat()
	if err != nil {
		return ErrOAuthJournalDisk
	}
	if file.Sync() != nil {
		return ErrOAuthJournalDisk
	}
	if err := file.Close(); err != nil {
		return ErrOAuthJournalDisk
	}
	if OAuthJournalConfigSyncParent(filepath.Dir(path)) != nil {
		return ErrOAuthJournalDisk
	}
	// Detect replacement of the synced inode before authorizing journal removal.
	current, err := os.Stat(physical)
	if err != nil || !os.SameFile(info, current) {
		return ErrOAuthJournalDisk
	}
	verified, err := os.ReadFile(physical)
	if err != nil || !bytes.Equal(data, verified) {
		return ErrOAuthJournalDisk
	}
	return nil
}

// Only proven completion or explicit stale/logout arbitration permits removal.
func RemoveJournal(path, configPath, providerID string, record *docread.OAuthJournalRecord, decision Decision) error {
	latest, err := ReadJournal(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(latest, record) {
		return ErrOAuthJournalDisk
	}
	disk, diskErr := ReadJournalDiskToken(configPath, providerID)
	if Arbitrate(record, nil, disk, diskErr) != decision {
		return ErrOAuthJournalDisk
	}
	switch decision {
	case OAuthJournalEqual, OAuthJournalDiskLaterOrTie, OAuthJournalLogout:
		if record == nil || VerifyConfigDurable(configPath, providerID, disk) != nil {
			return ErrOAuthJournalDisk
		}
	default:
		return ErrOAuthJournalDisk
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return ErrOAuthJournalDisk
	}
	if cfgcommit.SyncConfigParent(filepath.Dir(path)) != nil {
		return ErrOAuthJournalDisk
	}
	return nil
}
