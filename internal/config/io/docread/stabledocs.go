// Stable config document reads: bytes, owner policy and fingerprints bound to
// one observation of each file.
package docread

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stableConfigDocument is the exact byte document used to build a candidate.
// Keeping the bytes and fingerprint together prevents a second read from
// supplying overlay metadata that belongs to a different generation.
type stableConfigDocument struct {
	path        string
	data        []byte
	fingerprint reloadFileFingerprint
	present     bool
	aliases     []stableConfigAlias
}

type stableConfigAlias struct {
	path        string
	fingerprint reloadFileFingerprint
}

func loadConfigCandidateStableForWorkingDir(configPaths []string, workingDir string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	documents, err := readStableConfigDocumentsWithOwner(configPaths, func(path string) (int, bool, error) {
		return configOwnerPolicyForPath(path, workingDir)
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, document := range documents {
		if document.present && len(document.data) > 0 && !json.Valid(document.data) {
			return nil, nil, nil, nil, fmt.Errorf("invalid JSON in config file %s", document.path)
		}
		if document.present {
			if err := validateMCPDisabledOverlays(document.data); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("invalid MCP configuration in config file %s: %w", document.path, err)
			}
		}
	}
	configs, loaded, fingerprints := configDocumentBytes(documents)
	addDocumentAliasFingerprints(fingerprints, documents)

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, loaded, fingerprints, documents, nil
}

func readStableConfigDocumentsWithOwner(paths []string, owner func(string) (int, bool, error)) ([]stableConfigDocument, error) {
	documents := make([]stableConfigDocument, 0, len(paths))
	seenPresent := make(map[configFileIdentity]int, len(paths))
	seenAbsent := make(map[[sha256.Size]byte]int, len(paths))
	for _, path := range paths {
		discoveryPath := normalizeDiscoveryPath(path)
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
		data, fingerprint, readErr := readStableConfigFileOwned(discoveryPath, expectedOwner, enforceOwner)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("failed to open config file %s: %w", discoveryPath, readErr)
		}
		decoded := data
		if readErr == nil {
			var decodeErr error
			decoded, decodeErr = decodeConfigDocument(discoveryPath, data)
			if decodeErr != nil {
				return nil, decodeErr
			}
		}
		alias := stableConfigAlias{path: discoveryPath, fingerprint: fingerprint}
		if readErr != nil {
			if fingerprint.discovery != ([sha256.Size]byte{}) {
				if index, ok := seenAbsent[fingerprint.discovery]; ok {
					documents[index].aliases = append(documents[index].aliases, alias)
					continue
				}
				seenAbsent[fingerprint.discovery] = len(documents)
			}
			documents = append(documents, stableConfigDocument{path: discoveryPath, aliases: []stableConfigAlias{alias}})
			continue
		}
		if fingerprint.identity.valid {
			if index, ok := seenPresent[fingerprint.identity]; ok {
				documents[index].aliases = append(documents[index].aliases, alias)
				continue
			}
			seenPresent[fingerprint.identity] = len(documents)
		}
		documents = append(documents, stableConfigDocument{
			path: discoveryPath, data: decoded, fingerprint: fingerprint, present: fingerprint.exists,
			aliases: []stableConfigAlias{alias},
		})
	}
	return documents, nil
}

// readStableConfigFileOwned binds the trust decision, bytes, physical file
// identity, and discovery chain to one open file descriptor. A path is only a
// name: stat-then-ReadFile would allow a symlink or parent directory to be
// retargeted between those operations.
func readStableConfigFileOwned(path string, expectedOwner int, enforceOwner bool) ([]byte, reloadFileFingerprint, error) {
	for attempt := 0; attempt < stableReadMaxAttempts; attempt++ {
		beforeDiscovery := configDiscoveryFingerprint(path)
		beforeParentIdentity := configParentIdentity(path)
		runConfigBeforeOpenHook(path)
		file, err := openStableConfigFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, reloadFileFingerprint{
					discovery:       beforeDiscovery,
					parentDiscovery: configDiscoveryFingerprint(filepath.Dir(path)),
					parentIdentity:  beforeParentIdentity,
					aliasChain:      configAliasChainFingerprint(path),
				}, err
			}
			return nil, reloadFileFingerprint{}, err
		}
		data, fingerprint, retry, readErr := func() ([]byte, reloadFileFingerprint, bool, error) {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil {
				return nil, reloadFileFingerprint{}, false, statErr
			}
			if !info.Mode().IsRegular() {
				return nil, reloadFileFingerprint{}, false, fmt.Errorf("%w: %s", ErrConfigNonRegular, path)
			}
			owner, ownerKnown := configFileOwner(info)
			if enforceOwner && (!ownerKnown || owner != expectedOwner) {
				return nil, reloadFileFingerprint{}, false, errConfigOwnerMismatch
			}
			first, readErr := readOpenedConfigBytes(file)
			if readErr != nil {
				return nil, reloadFileFingerprint{}, false, readErr
			}
			second, readErr := readOpenedConfigBytes(file)
			if readErr != nil {
				return nil, reloadFileFingerprint{}, false, readErr
			}
			if !bytes.Equal(first, second) {
				return nil, reloadFileFingerprint{}, true, nil
			}
			runConfigAfterStableReadHook(path)
			finalInfo, statErr := file.Stat()
			if statErr != nil {
				return nil, reloadFileFingerprint{}, false, statErr
			}
			afterDiscovery := configDiscoveryFingerprint(path)
			if beforeDiscovery != afterDiscovery {
				return nil, reloadFileFingerprint{}, true, nil
			}
			afterParentIdentity := configParentIdentity(path)
			if beforeParentIdentity != afterParentIdentity {
				return nil, reloadFileFingerprint{}, true, nil
			}
			pathMatches, identityErr := configFilePathIdentityMatches(path, file, info)
			if identityErr != nil {
				if os.IsNotExist(identityErr) && attempt+1 < stableReadMaxAttempts {
					return nil, reloadFileFingerprint{}, true, nil
				}
				return nil, reloadFileFingerprint{}, false, identityErr
			}
			if !pathMatches {
				return nil, reloadFileFingerprint{}, true, nil
			}
			identity := configFileIdentityOfOpened(file, finalInfo)
			nlink := configFileNlinkOfOpened(file, finalInfo)
			return second, reloadFileFingerprint{
				exists: true, size: int64(len(second)), modTime: finalInfo.ModTime().UnixNano(),
				digest: sha256.Sum256(second), owner: owner, nlink: nlink,
				identity: identity, discovery: afterDiscovery,
				parentDiscovery: configDiscoveryFingerprint(filepath.Dir(path)),
				parentIdentity:  afterParentIdentity,
				aliasChain:      configAliasChainFingerprint(path),
			}, false, nil
		}()
		if retry {
			continue
		}
		if readErr != nil {
			return nil, reloadFileFingerprint{}, readErr
		}
		return data, fingerprint, nil
	}
	return nil, reloadFileFingerprint{}, errStableReadUnstable
}

// readStableConfigFile returns bytes that were observed identically by two
// consecutive reads. A fingerprint is made from those exact bytes; callers
// must not fingerprint a path before reading it because that admits a stale
// candidate when the file changes between the two operations.
func readStableConfigFile(path string) ([]byte, reloadFileFingerprint, error) {
	return readStableConfigFileOwned(path, 0, false)
}
