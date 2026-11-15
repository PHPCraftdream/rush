// Config input fingerprints: file snapshots, discovery/alias-chain digests and
// the staleness and MCP revision inputs built from them.
package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path        string
	Exists      bool
	Size        int64
	ModTime     int64 // UnixNano.
	ContentHash [32]byte
	fingerprint reloadFileFingerprint
}

type reloadFileFingerprint struct {
	exists          bool
	size            int64
	modTime         int64
	digest          [sha256.Size]byte
	owner           int
	nlink           uint64
	identity        configFileIdentity
	discovery       [sha256.Size]byte
	parentDiscovery [sha256.Size]byte
	parentIdentity  configFileIdentity
	// aliasChain excludes mutable regular-file metadata at the leaf.
	aliasChain [sha256.Size]byte
}

// configAliasChainFingerprint records the lexical path and every directory or
// symlink in its discovery chain. Metadata belonging only to a regular-file
// leaf is omitted so a cooperating atomic writer may replace its inode while
// retaining the same physical binding.
func configAliasChainFingerprint(path string) [sha256.Size]byte {
	h := sha256.New()
	abs := configFingerprintPath(path)
	var components []string
	for current := abs; ; current = filepath.Dir(current) {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	slices.Reverse(components)
	for i, component := range components {
		_, _ = io.WriteString(h, component)
		_, _ = io.WriteString(h, "\x00")
		info, statErr := os.Lstat(component)
		if statErr != nil {
			_, _ = io.WriteString(h, "missing:")
			_, _ = io.WriteString(h, statErr.Error())
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		isLeaf := i == len(components)-1
		if isLeaf && info.Mode().IsRegular() {
			// Regular-file permissions and inode identity belong to the fresh
			// descriptor fingerprint, not to the stable alias chain. Atomic
			// publication is allowed to change both while retaining the same
			// logical target.
			_, _ = io.WriteString(h, "regular-leaf|regular")
		} else {
			writeConfigDiscoveryInfo(h, info)
			if info.Mode()&os.ModeSymlink != 0 {
				if target, readErr := os.Readlink(component); readErr == nil {
					_, _ = io.WriteString(h, target)
				}
			}
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return sha256.Sum256(h.Sum(nil))
}

// configDiscoveryFingerprint includes every lexical component leading to the
// file. It makes a symlink retarget or a parent-directory replacement dirty
// even when the final target and bytes happen to return to their old values.
func configDiscoveryFingerprint(path string) [sha256.Size]byte {
	h := sha256.New()
	abs := configFingerprintPath(path)
	var components []string
	for current := abs; ; current = filepath.Dir(current) {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	slices.Reverse(components)
	for _, component := range components {
		_, _ = io.WriteString(h, component)
		_, _ = io.WriteString(h, "\x00")
		info, statErr := os.Lstat(component)
		if statErr != nil {
			_, _ = io.WriteString(h, "missing:")
			if os.IsNotExist(statErr) {
				_, _ = io.WriteString(h, "not-exist")
			} else {
				_, _ = io.WriteString(h, statErr.Error())
			}
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		writeConfigDiscoveryInfo(h, info)
		if info.Mode()&os.ModeSymlink != 0 {
			if target, readErr := os.Readlink(component); readErr == nil {
				_, _ = io.WriteString(h, target)
			}
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return sha256.Sum256(h.Sum(nil))
}

func reloadStalenessState(paths []string, fingerprints map[string]reloadFileFingerprint) ([]string, map[string]fileSnapshot) {
	paths = expandConfigSlotSiblings(paths)
	trackedSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			trackedSet[normalizeDiscoveryPath(path)] = struct{}{}
		}
	}
	tracked := make([]string, 0, len(trackedSet))
	for path := range trackedSet {
		tracked = append(tracked, path)
	}
	slices.Sort(tracked)

	snapshots := make(map[string]fileSnapshot, len(tracked))
	for _, path := range tracked {
		// The fingerprints map only holds SELECTED documents; an unselected
		// slot sibling that exists on disk (rush.ktav selected, ignored
		// rush.json present) must still contribute its real on-disk
		// fingerprint, or the snapshot would publish Exists:false for an
		// existing file and ConfigStaleness would report Dirty forever.
		fingerprint, fpErr := trackedSlotFingerprint(path, fingerprints)
		if fpErr != nil {
			fingerprint = reloadFileFingerprint{}
		}
		snapshots[path] = fileSnapshot{
			Path:        path,
			Exists:      fingerprint.exists,
			Size:        fingerprint.size,
			ModTime:     fingerprint.modTime,
			ContentHash: fingerprint.digest,
			fingerprint: fingerprint,
		}
	}
	return tracked, snapshots
}

func candidateInputsChanged(workingDir string, configPaths, externalPaths []string, fingerprints map[string]reloadFileFingerprint) bool {
	return !sameReloadPathSet(configPaths, lookupConfigCandidates(workingDir)) ||
		!sameReloadPathSet(externalPaths, mcpJSONCandidatePaths(workingDir)) ||
		reloadFingerprintsChangedWithOwner(fingerprints, func(path string) (int, bool, error) {
			globalExternal := filepath.Join(home.Dir(), ".claude", ".mcp.json")
			if normalizeReloadPath(path) == normalizeReloadPath(globalExternal) {
				return homeConfigOwner(), true, nil
			}
			return configOwnerPolicyForPath(path, workingDir)
		})
}

var errReloadDiskChanged = errors.New("config files changed while reloading")

func initialMCPRevisions(cfg *Config) map[string]uint64 {
	result := make(map[string]uint64)
	if cfg != nil {
		for name := range cfg.MCP {
			result[name] = 1
		}
	}
	return result
}

// mcpInputFingerprints fingerprints only the MCP entries in each source
// document. Changes to unrelated config fields therefore preserve an entry's
// revision across reloads.
func mcpInputFingerprints(rush, external []stableConfigDocument) map[string][sha256.Size]byte {
	type document struct {
		path    string
		entries map[string]json.RawMessage
		// rawDigest is the RAW disk digest of a Ktav rush document. It is
		// mixed into the hash of every entry this document DEFINES, so two
		// Ktav documents that decode to identical JSON but differ in raw
		// bytes still produce different revisions (JSON documents keep the
		// legacy entry-only hash). It is never mixed into an entry the
		// document does not define: a write to an unrelated Ktav document
		// (for example the app-managed data file) must not move that
		// entry's revision.
		rawDigest []byte
	}
	documentsByPath := make(map[string]document, len(rush)+len(external))
	names := make(map[string]struct{})
	addDocument := func(sourceDocument stableConfigDocument, entries map[string]json.RawMessage, rawDigest []byte) {
		path := normalizeReloadPath(sourceDocument.path)
		if path == "" {
			path = normalizeDiscoveryPath(sourceDocument.path)
		}
		if existing, ok := documentsByPath[path]; ok {
			if len(existing.entries) == 0 && len(entries) != 0 {
				documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
			}
			return
		}
		documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
	}
	for _, sourceDocument := range rush {
		var rawDigest []byte
		if isKtavConfigPath(sourceDocument.path) && sourceDocument.fingerprint.exists {
			rawDigest = append([]byte(nil), sourceDocument.fingerprint.digest[:]...)
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(sourceDocument.data, &root) == nil {
			addDocument(sourceDocument, root.MCP, rawDigest)
			for name := range root.MCP {
				names[name] = struct{}{}
			}
		}
	}
	for _, sourceDocument := range external {
		var root struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(sourceDocument.data, &root) == nil {
			addDocument(sourceDocument, root.MCPServers, nil)
			for name := range root.MCPServers {
				names[name] = struct{}{}
			}
		}
	}
	documents := make([]document, 0, len(documentsByPath))
	for _, document := range documentsByPath {
		documents = append(documents, document)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].path < documents[j].path })
	result := make(map[string][sha256.Size]byte, len(names))
	for name := range names {
		hash := sha256.New()
		for _, document := range documents {
			path := normalizeReloadPath(document.path)
			if path == "" {
				path = normalizeDiscoveryPath(document.path)
			}
			hash.Write([]byte(path))
			if raw, ok := document.entries[name]; ok {
				if len(document.rawDigest) > 0 {
					hash.Write(document.rawDigest)
				}
				hash.Write([]byte{1})
				hash.Write(raw)
			} else {
				hash.Write([]byte{0})
			}
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		result[name] = digest
	}
	return result
}
