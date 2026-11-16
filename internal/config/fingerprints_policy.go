package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

type (
	reloadFileFingerprint = docread.Fingerprint
	fileSnapshot          = docread.FileSnapshot
	stableConfigDocument  = docread.StableConfigDocument
	stableConfigAlias     = docread.StableConfigAlias
)

var errReloadDiskChanged = errors.New("config files changed while reloading")

func candidateInputsChanged(workingDir string, configPaths, externalPaths []string, fingerprints map[string]reloadFileFingerprint) bool {
	return !docread.SameReloadPathSet(configPaths, docread.LookupConfigCandidates(workingDir)) ||
		!docread.SameReloadPathSet(externalPaths, docread.McpJSONCandidatePaths(workingDir)) ||
		docread.ReloadFingerprintsChangedWithOwner(fingerprints, func(path string) (int, bool, error) {
			globalExternal := filepath.Join(home.Dir(), ".claude", ".mcp.json")
			if docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(globalExternal) {
				return homeConfigOwner(), true, nil
			}
			return docread.ConfigOwnerPolicyForPath(path, workingDir)
		})
}

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
		path := docread.NormalizeReloadPath(sourceDocument.Path)
		if path == "" {
			path = docread.NormalizeDiscoveryPath(sourceDocument.Path)
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
		if docread.IsKtavConfigPath(sourceDocument.Path) && sourceDocument.Fingerprint.Exists {
			rawDigest = append([]byte(nil), sourceDocument.Fingerprint.Digest[:]...)
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
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
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
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
			path := docread.NormalizeReloadPath(document.path)
			if path == "" {
				path = docread.NormalizeDiscoveryPath(document.path)
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
