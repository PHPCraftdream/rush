package config

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

	"github.com/PHPCraftdream/rush/internal/oauth"
)

var (
	errOAuthJournalInvalid = errors.New("invalid OAuth journal")
	errOAuthJournalRead    = errors.New("OAuth journal cannot be read")
	errOAuthJournalDisk    = errors.New("OAuth config cannot be verified")
	// Serial test seams must be restored only after their users have joined.
	oauthJournalReadWait     = waitCodexRefresh
	oauthJournalReadBackoffs = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	oauthJournalReadFile     = os.ReadFile
	oauthJournalAtomicWrite  = atomicWriteFile
	oauthJournalRetryWait    = waitCodexRefresh
	// Serial tests shrink the future synchronous write budget before workers start.
	oauthJournalWriteBudget = 5 * time.Second
)

// Config verification and journal writes may share a parent directory.
var oauthJournalConfigSyncParent = syncConfigParent

// Only the predecessor refresh-token digest is persisted, never its secret.
type oauthJournalRecord struct {
	PredecessorRefreshTokenSHA256 string       `json:"predecessor_refresh_token_sha256"`
	Token                         *oauth.Token `json:"token"`
	BurnedRefreshTokenSHA256      []string     `json:"burned_refresh_token_sha256,omitempty"`
}

type oauthJournalDecision uint8

const (
	oauthJournalError oauthJournalDecision = iota
	oauthJournalEqual
	oauthJournalPredecessor
	oauthJournalDiskLaterOrTie
	oauthJournalLater
	oauthJournalLogout
)

func oauthRefreshTokenSHA256(token *oauth.Token) string {
	if token == nil {
		return ""
	}
	digest := sha256.Sum256([]byte(token.RefreshToken))
	return hex.EncodeToString(digest[:])
}

func oauthRotationDigest(previous, token *oauth.Token) string {
	if previous == nil || token == nil || previous.RefreshToken == "" || token.RefreshToken == "" || previous.RefreshToken == token.RefreshToken {
		return ""
	}
	return oauthRefreshTokenSHA256(previous)
}

// Unchanged credentials establish lineage without claiming a burned token.
func oauthJournalLivePredecessor(record *oauthJournalRecord, live *oauth.Token) bool {
	if record == nil || live == nil {
		return false
	}
	if record.PredecessorRefreshTokenSHA256 != "" {
		// A digest equal to the record token's own refresh digest could only come from an
		// untagged build between 15ef615e and 9e62125f and is cleared by any later recovery,
		// so it is not normalized on read.
		return oauthRefreshTokenSHA256(live) == record.PredecessorRefreshTokenSHA256
	}
	// Same-refresh generations are ordered only by the arbiter's expiry rule.
	return record.Token != nil && live.RefreshToken != "" && live.RefreshToken == record.Token.RefreshToken && arbitrateOAuthJournal(record, nil, live, nil) == oauthJournalLater
}

// The legacy primary digest and optional additional digests form one set.
func oauthJournalWithBurned(record *oauthJournalRecord, digests ...string) oauthJournalRecord {
	copyRecord := *record
	successor := oauthRefreshTokenSHA256(record.Token)
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
func oauthJournalPath(configPath, providerID string) string {
	digest := sha256.Sum256([]byte(providerID))
	return normalizeReloadPath(configPath) + ".oauth-pending-" + hex.EncodeToString(digest[:8]) + ".json"
}

func validOAuthJournal(record *oauthJournalRecord) bool {
	if record == nil || record.Token == nil || record.Token.AccessToken == "" || record.Token.RefreshToken == "" || record.Token.ExpiresAt <= 0 {
		return false
	}
	if record.PredecessorRefreshTokenSHA256 != "" {
		digest, err := hex.DecodeString(record.PredecessorRefreshTokenSHA256)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	for _, burned := range record.BurnedRefreshTokenSHA256 {
		digest, err := hex.DecodeString(burned)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	return true
}

// Missing journals are normal; unreadable or corrupt journals are kept.
func readOAuthJournal(path string) (*oauthJournalRecord, error) {
	data, err := oauthJournalReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		logOAuthJournalFailure(path, "read", "Failed to read OAuth journal")
		return nil, errOAuthJournalRead
	}
	jsonBytes, decodeErr := decodeConfigDocument(path, data)
	if decodeErr != nil {
		logOAuthJournalFailure(path, "invalid", "Failed to read OAuth journal")
		return nil, errOAuthJournalInvalid
	}
	var record oauthJournalRecord
	if json.Unmarshal(jsonBytes, &record) != nil || !validOAuthJournal(&record) {
		logOAuthJournalFailure(path, "invalid", "Failed to read OAuth journal")
		return nil, errOAuthJournalInvalid
	}
	return &record, nil
}

// Only journal I/O receives bounded, context-cancelable retries.
func readOAuthJournalWithRetry(ctx context.Context, path string) (*oauthJournalRecord, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := readOAuthJournal(path)
		if !errors.Is(err, errOAuthJournalRead) || attempt >= len(oauthJournalReadBackoffs) {
			return record, err
		}
		if err := oauthJournalReadWait(ctx, oauthJournalReadBackoffs[attempt]); err != nil {
			return nil, err
		}
	}
}

// Bounded leader retries hand remaining durability work to the store outbox.
func writeOAuthJournalUntilDurable(parent context.Context, path string, record *oauthJournalRecord) error {
	ctx, cancel := context.WithTimeout(parent, oauthJournalWriteBudget)
	defer cancel()
	delay := 50 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeOAuthJournalOnce(path, record); err == nil {
			return nil
		} else if errors.Is(err, errOAuthJournalInvalid) {
			return err
		}
		if err := oauthJournalRetryWait(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// A worker attempts one write without waiting under its refresh sidecar.
func writeOAuthJournalOnce(path string, record *oauthJournalRecord) error {
	if !validOAuthJournal(record) {
		return errOAuthJournalInvalid
	}
	data, err := json.Marshal(record)
	if err != nil {
		return errOAuthJournalInvalid
	}
	data, encErr := encodeSettingsDocument(data, settingsDocumentFormatOf(path))
	if encErr != nil {
		return errOAuthJournalInvalid
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = oauthJournalAtomicWrite(path, data, 0o600)
	}
	if err != nil {
		return errOAuthJournalDisk
	}
	return nil
}

// A successful read of a valid document is required to establish logout.
func readOAuthJournalDiskToken(path, providerID string) (*oauth.Token, error) {
	doc, err := readSettingsDocument(path)
	if err != nil {
		return nil, errOAuthJournalDisk
	}
	return decodeOAuthJournalDiskToken(doc.jsonBytes, providerID)
}

func decodeOAuthJournalDiskToken(data []byte, providerID string) (*oauth.Token, error) {
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, errOAuthJournalDisk
	}
	providersData, exists := document["providers"]
	if !exists {
		return nil, nil
	}
	var providers map[string]json.RawMessage
	if json.Unmarshal(providersData, &providers) != nil || providers == nil {
		return nil, errOAuthJournalDisk
	}
	providerData, exists := providers[providerID]
	if !exists {
		return nil, nil
	}
	var provider map[string]json.RawMessage
	if json.Unmarshal(providerData, &provider) != nil || provider == nil {
		return nil, errOAuthJournalDisk
	}
	tokenData, exists := provider["oauth"]
	if !exists {
		return nil, nil
	}
	var token *oauth.Token
	if json.Unmarshal(tokenData, &token) != nil {
		return nil, errOAuthJournalDisk
	}
	if token != nil && token.AccessToken == "" {
		return nil, errOAuthJournalDisk
	}
	return token, nil
}

// Order: uncertainty, proven logout, equal, burned predecessor, then expiry.
// Without ownership, live is the reference and no predecessor digest is used.
func arbitrateOAuthJournal(record *oauthJournalRecord, predecessor, disk *oauth.Token, diskErr error, live ...*oauth.Token) oauthJournalDecision {
	if diskErr != nil {
		return oauthJournalError
	}
	if disk == nil {
		return oauthJournalLogout
	}
	var reference *oauth.Token
	if record != nil {
		reference = record.Token
	} else if len(live) > 0 {
		reference = live[0]
	}
	if reference == nil {
		return oauthJournalError
	}
	if reflect.DeepEqual(disk, reference) {
		return oauthJournalEqual
	}
	if predecessor != nil && oauthRotationDigest(predecessor, reference) != "" && reflect.DeepEqual(disk, predecessor) ||
		record != nil && (record.PredecessorRefreshTokenSHA256 != "" && oauthRefreshTokenSHA256(disk) == record.PredecessorRefreshTokenSHA256 || slices.Contains(record.BurnedRefreshTokenSHA256, oauthRefreshTokenSHA256(disk))) {
		return oauthJournalPredecessor
	}
	if disk.ExpiresAt >= reference.ExpiresAt {
		return oauthJournalDiskLaterOrTie
	}
	return oauthJournalLater
}

// Caller holds refresh/config locks; equal bytes alone do not prove durability.
func verifyOAuthJournalConfigDurable(path, providerID string, token *oauth.Token) error {
	physical := readSlotOf(path)
	file, err := os.OpenFile(physical, os.O_RDWR, 0)
	if err != nil {
		return errOAuthJournalDisk
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return errOAuthJournalDisk
	}
	jsonBytes, decodeErr := decodeConfigDocument(physical, data)
	if decodeErr != nil {
		return errOAuthJournalDisk
	}
	disk, err := decodeOAuthJournalDiskToken(jsonBytes, providerID)
	if err != nil || !reflect.DeepEqual(disk, token) {
		return errOAuthJournalDisk
	}
	info, err := file.Stat()
	if err != nil {
		return errOAuthJournalDisk
	}
	if file.Sync() != nil {
		return errOAuthJournalDisk
	}
	if err := file.Close(); err != nil {
		return errOAuthJournalDisk
	}
	if oauthJournalConfigSyncParent(filepath.Dir(path)) != nil {
		return errOAuthJournalDisk
	}
	// Detect replacement of the synced inode before authorizing journal removal.
	current, err := os.Stat(physical)
	if err != nil || !os.SameFile(info, current) {
		return errOAuthJournalDisk
	}
	verified, err := os.ReadFile(physical)
	if err != nil || !bytes.Equal(data, verified) {
		return errOAuthJournalDisk
	}
	return nil
}

// Only proven completion or explicit stale/logout arbitration permits removal.
func removeOAuthJournal(path, configPath, providerID string, record *oauthJournalRecord, decision oauthJournalDecision) error {
	latest, err := readOAuthJournal(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(latest, record) {
		return errOAuthJournalDisk
	}
	disk, diskErr := readOAuthJournalDiskToken(configPath, providerID)
	if arbitrateOAuthJournal(record, nil, disk, diskErr) != decision {
		return errOAuthJournalDisk
	}
	switch decision {
	case oauthJournalEqual, oauthJournalDiskLaterOrTie, oauthJournalLogout:
		if record == nil || verifyOAuthJournalConfigDurable(configPath, providerID, disk) != nil {
			return errOAuthJournalDisk
		}
	default:
		return errOAuthJournalDisk
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errOAuthJournalDisk
	}
	if syncConfigParent(filepath.Dir(path)) != nil {
		return errOAuthJournalDisk
	}
	return nil
}
