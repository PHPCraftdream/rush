package config

import (
	"crypto/sha256"
	"encoding/json"
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
			hash.Write([]byte(document.path))
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
