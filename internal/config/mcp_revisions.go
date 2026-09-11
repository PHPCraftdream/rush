package config

import (
	"crypto/sha256"
	"encoding/json"
	"maps"
	"reflect"
)

// mcpInputFingerprints fingerprints only the MCP entries in each source
// document. Changes to unrelated config fields therefore preserve an entry's
// revision across reloads.
func mcpInputFingerprints(rush, external []stableConfigDocument) map[string][sha256.Size]byte {
	type document struct {
		path    string
		entries map[string]json.RawMessage
	}
	documents := make([]document, 0, len(rush)+len(external))
	names := make(map[string]struct{})
	for _, sourceDocument := range rush {
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(sourceDocument.data, &root) == nil {
			documents = append(documents, document{path: sourceDocument.path, entries: root.MCP})
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
			documents = append(documents, document{path: sourceDocument.path, entries: root.MCPServers})
			for name := range root.MCPServers {
				names[name] = struct{}{}
			}
		}
	}
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
		previousConfig, previousExists := mcpConfig(previous, name)
		nextConfig, nextExists := mcpConfig(next, name)
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

func mcpNames(cfg *Config) map[string]MCPConfig {
	if cfg == nil {
		return nil
	}
	return cfg.MCP
}
