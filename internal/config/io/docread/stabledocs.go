// Stable config document reads: bytes, owner policy and fingerprints bound to
// one observation of each file.
package docread

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
)

// StableConfigDocument is the exact byte document used to build a candidate.
// Keeping the bytes and fingerprint together prevents a second read from
// supplying overlay metadata that belongs to a different generation.
type StableConfigDocument struct {
	Path        string
	Data        []byte
	Fingerprint Fingerprint
	Present     bool
	Aliases     []StableConfigAlias
}

type StableConfigAlias struct {
	Path        string
	Fingerprint Fingerprint
}

// ErrConfigReloadUnstable indicates that no stable file snapshot could be
// built within the bounded reload retry budget.
var ErrConfigReloadUnstable = errors.New("config files remained unstable during reload")

// NormalizeReloadPath returns the canonical spelling used for reload bookkeeping.
func NormalizeReloadPath(path string) string {
	if path == "" {
		return ""
	}
	return datadir.CanonicalConfigPath(path)
}

// NormalizeDiscoveryPath returns the absolute cleaned spelling used for file discovery.
func NormalizeDiscoveryPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func ReadStableConfigDocumentsWithOwner(paths []string, owner func(string) (int, bool, error)) ([]StableConfigDocument, error) {
	documents := make([]StableConfigDocument, 0, len(paths))
	seenPresent := make(map[fileid.FileIdentity]int, len(paths))
	seenAbsent := make(map[[sha256.Size]byte]int, len(paths))
	for _, path := range paths {
		discoveryPath := NormalizeDiscoveryPath(path)
		if discoveryPath == "" {
			continue
		}
		expectedOwner, enforceOwner := 0, false
		var ownerErr error
		if owner != nil {
			expectedOwner, enforceOwner, ownerErr = owner(discoveryPath)
			if ownerErr != nil {
				return nil, ownerErr
			}
		}
		data, fingerprint, readErr := ReadStableConfigFileOwned(discoveryPath, expectedOwner, enforceOwner)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("failed to open config file %s: %w", discoveryPath, readErr)
		}
		decoded := data
		if readErr == nil {
			var decodeErr error
			decoded, decodeErr = DecodeConfigDocument(discoveryPath, data)
			if decodeErr != nil {
				return nil, decodeErr
			}
		}
		alias := StableConfigAlias{Path: discoveryPath, Fingerprint: fingerprint}
		if readErr != nil {
			if fingerprint.Discovery != ([sha256.Size]byte{}) {
				if index, ok := seenAbsent[fingerprint.Discovery]; ok {
					documents[index].Aliases = append(documents[index].Aliases, alias)
					continue
				}
				seenAbsent[fingerprint.Discovery] = len(documents)
			}
			documents = append(documents, StableConfigDocument{Path: discoveryPath, Aliases: []StableConfigAlias{alias}})
			continue
		}
		if fingerprint.Identity.Valid {
			if index, ok := seenPresent[fingerprint.Identity]; ok {
				documents[index].Aliases = append(documents[index].Aliases, alias)
				continue
			}
			seenPresent[fingerprint.Identity] = len(documents)
		}
		documents = append(documents, StableConfigDocument{
			Path: discoveryPath, Data: decoded, Fingerprint: fingerprint, Present: fingerprint.Exists,
			Aliases: []StableConfigAlias{alias},
		})
	}
	return documents, nil
}

// ReadStableConfigFileOwned binds the trust decision, bytes, physical file
// identity, and discovery chain to one open file descriptor. A path is only a
// name: stat-then-ReadFile would allow a symlink or parent directory to be
// retargeted between those operations.
func ReadStableConfigFileOwned(path string, expectedOwner int, enforceOwner bool) ([]byte, Fingerprint, error) {
	for attempt := 0; attempt < stableReadMaxAttempts; attempt++ {
		beforeDiscovery := ConfigDiscoveryFingerprint(path)
		beforeParentIdentity := fileid.ConfigParentIdentity(path)
		cfghooks.RunConfigBeforeOpenHook(path)
		file, err := fileid.OpenStableConfigFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, Fingerprint{
					Discovery:       beforeDiscovery,
					ParentDiscovery: ConfigDiscoveryFingerprint(filepath.Dir(path)),
					ParentIdentity:  beforeParentIdentity,
					AliasChain:      ConfigAliasChainFingerprint(path),
				}, err
			}
			return nil, Fingerprint{}, err
		}
		data, fingerprint, retry, readErr := func() ([]byte, Fingerprint, bool, error) {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil {
				return nil, Fingerprint{}, false, statErr
			}
			if !info.Mode().IsRegular() {
				return nil, Fingerprint{}, false, fmt.Errorf("%w: %s", fileid.ErrConfigNonRegular, path)
			}
			owner, ownerKnown := fileid.ConfigFileOwner(info)
			if enforceOwner && (!ownerKnown || owner != expectedOwner) {
				return nil, Fingerprint{}, false, fileid.ErrConfigOwnerMismatch()
			}
			first, readErr := fileid.ReadOpenedConfigBytes(file)
			if readErr != nil {
				return nil, Fingerprint{}, false, readErr
			}
			second, readErr := fileid.ReadOpenedConfigBytes(file)
			if readErr != nil {
				return nil, Fingerprint{}, false, readErr
			}
			if !bytes.Equal(first, second) {
				return nil, Fingerprint{}, true, nil
			}
			cfghooks.RunConfigAfterStableReadHook(path)
			finalInfo, statErr := file.Stat()
			if statErr != nil {
				return nil, Fingerprint{}, false, statErr
			}
			afterDiscovery := ConfigDiscoveryFingerprint(path)
			if beforeDiscovery != afterDiscovery {
				return nil, Fingerprint{}, true, nil
			}
			afterParentIdentity := fileid.ConfigParentIdentity(path)
			if beforeParentIdentity != afterParentIdentity {
				return nil, Fingerprint{}, true, nil
			}
			pathMatches, identityErr := fileid.ConfigFilePathIdentityMatches(path, file, info)
			if identityErr != nil {
				if os.IsNotExist(identityErr) && attempt+1 < stableReadMaxAttempts {
					return nil, Fingerprint{}, true, nil
				}
				return nil, Fingerprint{}, false, identityErr
			}
			if !pathMatches {
				return nil, Fingerprint{}, true, nil
			}
			identity := fileid.FileIdentityOfOpened(file, finalInfo)
			nlink := fileid.FileNlinkOfOpened(file, finalInfo)
			return second, Fingerprint{
				Exists: true, Size: int64(len(second)), ModTime: finalInfo.ModTime().UnixNano(),
				Digest: sha256.Sum256(second), Owner: owner, Nlink: nlink,
				Identity: identity, Discovery: afterDiscovery,
				ParentDiscovery: ConfigDiscoveryFingerprint(filepath.Dir(path)),
				ParentIdentity:  afterParentIdentity,
				AliasChain:      ConfigAliasChainFingerprint(path),
			}, false, nil
		}()
		if retry {
			continue
		}
		if readErr != nil {
			return nil, Fingerprint{}, readErr
		}
		return data, fingerprint, nil
	}
	return nil, Fingerprint{}, fileid.ErrStableReadUnstable()
}

// ReadStableConfigFile returns bytes that were observed identically by two
// consecutive reads. A fingerprint is made from those exact bytes; callers
// must not fingerprint a path before reading it because that admits a stale
// candidate when the file changes between the two operations.
func ReadStableConfigFile(path string) ([]byte, Fingerprint, error) {
	return ReadStableConfigFileOwned(path, 0, false)
}
