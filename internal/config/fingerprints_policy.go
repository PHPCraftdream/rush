package config

import (
	"crypto/sha256"
	"encoding/json"
	"maps"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
	"github.com/tidwall/gjson"
)

// Existing model-only documents keep their exact v1 signature payload.
// Role-aware payloads also bind tools presence, preventing unsigned widening.
func canonicalModelsAndRolesJSON(data []byte) (string, bool, error) {
	models, modelsPresent, err := canonicalModelsJSON(gjson.GetBytes(data, "models").Raw)
	if err != nil {
		return "", false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return "", false, err
	}
	rawRoles, rolesPresent, err := cfgmodel.CustomRolesDocumentField(document)
	if err != nil {
		return "", false, err
	}
	if !rolesPresent {
		return models, modelsPresent, nil
	}
	roles, err := cfgmodel.DecodeCustomRoles(rawRoles)
	if err != nil {
		return "", false, err
	}
	if len(roles) == 0 {
		return models, modelsPresent, nil
	}
	canonicalRoles, _, err := canonicalModelsJSON(string(rawRoles))
	if err != nil {
		return "", false, err
	}
	payload := map[string]json.RawMessage{"roles": json.RawMessage(canonicalRoles)}
	if modelsPresent {
		payload["models"] = json.RawMessage(models)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	return "roles-v1|" + string(encoded), true, nil
}

type (
	reloadFileFingerprint = docread.Fingerprint
	fileSnapshot          = docread.FileSnapshot
	stableConfigDocument  = docread.StableConfigDocument
	stableConfigAlias     = docread.StableConfigAlias
)

// errReloadDiskChanged aliases the cfgstore sentinel so the root retry loop
// (errors.Is) recognizes the leaf reload engine's instability error.
var errReloadDiskChanged = cfgstore.ErrReloadDiskChanged

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

// MCP policy helpers that combine external documents with the root config.
// mergeExternalMCPServers injects .mcp.json servers into the config's MCP map.
// Servers already defined in rush.json take full precedence. For external
// servers, the disabled state is read from the rush config store.
func mergeExternalMCPServers(cfg *Config, store *ConfigStore, external map[string]MCPConfig, loadedPaths []string) {
	_ = store
	_ = loadedPaths
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	for name, extCfg := range external {
		if _, exists := cfg.MCP[name]; exists {
			// A complete Rush definition takes precedence over .mcp.json.
			continue
		}
		cfg.MCP[name] = extCfg
	}
}

func readMCPDisabledOverride(store *ConfigStore, scope Scope, name string) (bool, bool) {
	path, err := store.ConfigPath(scope)
	if err != nil {
		return false, false
	}
	// The scope path is a logical slot: ReadSettingsDocument selects the
	// active file (rush.ktav wins) and decodes Ktav to JSON-shaped bytes.
	// A read or decode failure stays "no override", as before.
	doc, err := docread.ReadSettingsDocument(path)
	if err != nil {
		return false, false
	}
	entry, ok := docread.McpEntryFromJSON(doc.JSONBytes, name)
	if !docread.IsMCPDisabledOnlyEntry(entry, ok) {
		return false, false
	}
	raw, ok := entry["disabled"]
	if !ok {
		return false, false
	}
	var disabled bool
	if json.Unmarshal(raw, &disabled) != nil {
		return false, false
	}
	return disabled, true
}

// mcpRevisionDiff advances only names whose effective definition or exact
// source input changed. A missing revision is deliberately treated as zero so
// an in-memory staged entry receives a real admission token.
func mcpRevisionDiff(current map[string]uint64, previous, next *Config, previousInputs, nextInputs map[string][sha256.Size]byte, force ...string) map[string]uint64 {
	result := maps.Clone(current)
	if result == nil {
		result = make(map[string]uint64)
	}
	names := make(map[string]struct{})
	for name := range mcpNames(previous) {
		names[name] = struct{}{}
	}
	for name := range mcpNames(next) {
		names[name] = struct{}{}
	}
	for name := range previousInputs {
		names[name] = struct{}{}
	}
	for name := range nextInputs {
		names[name] = struct{}{}
	}
	for _, name := range force {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	forced := make(map[string]struct{}, len(force))
	for _, name := range force {
		forced[name] = struct{}{}
	}
	for name := range names {
		previousConfig, previousExists := cfgstore.MCPConfigFor(previous, name)
		nextConfig, nextExists := cfgstore.MCPConfigFor(next, name)
		previousInput, previousHasInput := previousInputs[name]
		nextInput, nextHasInput := nextInputs[name]
		if _, ok := forced[name]; ok || previousExists != nextExists ||
			previousExists && !reflect.DeepEqual(previousConfig, nextConfig) ||
			previousHasInput != nextHasInput ||
			previousHasInput && nextHasInput && previousInput != nextInput {
			result[name]++
		}
	}
	return result
}

func mcpNames(cfg *Config) map[string]cfgmodel.MCPConfig {
	if cfg == nil {
		return nil
	}
	return cfg.MCP
}

const cacheRevKey = cfgmodel.CacheRevKey

func applyModelsIntegrity(doc string) (string, error) {
	return cfgmodel.ApplyModelsIntegrity(doc)
}

func touchesModelsKey(keys []string) bool {
	return cfgmodel.TouchesModelsKey(keys)
}

func modelsIntegrityOf(data []byte, lockExists bool) error {
	return cfgmodel.ModelsIntegrityOf(data, lockExists)
}
