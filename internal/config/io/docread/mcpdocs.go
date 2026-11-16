// MCP document loading (.mcp.json and rush.json mcpServers) and the per-entry
// revision bookkeeping derived from those documents.
package docread

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

// mcpJSONFile represents the .mcp.json file format (Claude Code compatible).
type mcpJSONFile struct {
	MCPServers map[string]mcpJSONEntry `json:"mcpServers"`
}

// mcpJSONEntry is a single server entry in the .mcp.json format.
type mcpJSONEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// loadMCPJSON reads a .mcp.json file and converts its entries to MCPConfig.
func loadMCPJSON(path string) (map[string]cfgmodel.MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return LoadMCPJSONBytes(data)
}

// LoadMCPJSONBytes parses JSON bytes in the external .mcp.json format.
func LoadMCPJSONBytes(data []byte) (map[string]cfgmodel.MCPConfig, error) {
	var file mcpJSONFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	result := make(map[string]cfgmodel.MCPConfig, len(file.MCPServers))
	for name, entry := range file.MCPServers {
		mcpType := cfgmodel.MCPStdio
		switch cfgmodel.MCPType(entry.Type) {
		case cfgmodel.MCPSSE:
			mcpType = cfgmodel.MCPSSE
		case cfgmodel.MCPHttp:
			mcpType = cfgmodel.MCPHttp
		}

		result[name] = cfgmodel.MCPConfig{
			Type:    mcpType,
			Command: entry.Command,
			Args:    entry.Args,
			Env:     entry.Env,
			URL:     entry.URL,
			Headers: entry.Headers,
			Source:  cfgmodel.MCPSourceExternal,
			Timeout: 60, // Higher default for external servers (uvx, npx etc. may be slow to start).
		}
	}
	return result, nil
}

// McpJSONCandidatePaths returns every location that may provide .mcp.json,
// including files that do not currently exist. Keeping the candidate set
// explicit lets staleness tracking detect later additions.
func McpJSONCandidatePaths(workingDir string) []string {
	var paths []string

	// Global: ~/.claude/.mcp.json
	if homeDir := home.Dir(); homeDir != "" {
		if path := eligibleConfigCandidate(filepath.Join(homeDir, ".claude", ".mcp.json"), fileid.HomeConfigOwner()); path != "" {
			paths = append(paths, path)
		}
	}

	// Project root: <workingDir>/.mcp.json
	if workingDir != "" {
		if path := eligibleWorkspaceConfig(filepath.Join(datadir.CanonicalConfigPath(workingDir), ".mcp.json"), workingDir); path != "" {
			paths = append(paths, path)
		}
	}

	return paths
}

// discoverMCPJSONFiles returns existing .mcp.json files in priority order
// (lowest priority first): global (~/.claude/.mcp.json), then project root.
func discoverMCPJSONFiles(workingDir string) []string {
	var paths []string
	for _, path := range McpJSONCandidatePaths(workingDir) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			paths = append(paths, path)
		}
	}
	return paths
}

// ConfigAndMCPStalenessPaths combines config paths with all external MCP
// candidate paths, including files that do not currently exist.
func ConfigAndMCPStalenessPaths(configPaths []string, workingDir string) []string {
	return append(slices.Clone(configPaths), McpJSONCandidatePaths(workingDir)...)
}

// loadExternalMCPServers discovers and loads all .mcp.json files, returning
// a merged map of server configs. Later files override earlier ones.
func loadExternalMCPServers(workingDir string) map[string]cfgmodel.MCPConfig {
	return loadExternalMCPServersFromPaths(discoverMCPJSONFiles(workingDir))
}

// loadExternalMCPServersFromPaths loads one previously discovered, stable
// path set. Reload uses this variant so discovery itself is part of the
// candidate fingerprint instead of being repeated halfway through the read.
func loadExternalMCPServersFromPaths(paths []string) map[string]cfgmodel.MCPConfig {
	result := make(map[string]cfgmodel.MCPConfig)
	for _, path := range paths {
		servers, err := loadMCPJSON(path)
		if err != nil {
			slog.Warn("Failed to load .mcp.json", "path", path, "err", err)
			continue
		}
		slog.Info("Loaded MCP servers from .mcp.json", "path", path, "count", len(servers))
		for name, cfg := range servers {
			result[name] = cfg
		}
	}
	return result
}

func loadExternalMCPServersFromStablePaths(paths []string, fingerprints map[string]Fingerprint) map[string]cfgmodel.MCPConfig {
	_, result, err := loadExternalMCPDocumentsStable(paths, fingerprints)
	if err != nil {
		slog.Warn("Failed to load .mcp.json", "error", err)
	}
	return result
}

func loadExternalMCPServersFromStableDocuments(paths []string, fingerprints map[string]Fingerprint) (map[string]cfgmodel.MCPConfig, error) {
	_, result, err := loadExternalMCPDocumentsStable(paths, fingerprints)
	return result, err
}

func loadExternalMCPDocumentsStable(paths []string, fingerprints map[string]Fingerprint) ([]StableConfigDocument, map[string]cfgmodel.MCPConfig, error) {
	return LoadExternalMCPDocumentsStableForWorkingDir(paths, fingerprints, "")
}

func LoadExternalMCPDocumentsStableForWorkingDir(paths []string, fingerprints map[string]Fingerprint, workingDir string) ([]StableConfigDocument, map[string]cfgmodel.MCPConfig, error) {
	result := make(map[string]cfgmodel.MCPConfig)
	documents, err := readStableConfigDocumentsWithOwner(paths, func(path string) (int, bool, error) {
		// The global external file follows the home policy; the project file
		// follows the working-directory policy. This is intentionally separate
		// from rush.json provenance.
		globalPath := filepath.Join(home.Dir(), ".claude", ".mcp.json")
		if NormalizeReloadPath(path) == NormalizeReloadPath(globalPath) {
			return fileid.HomeConfigOwner(), true, nil
		}
		return configOwnerForWorkingDir(workingDir)
	})
	if err != nil {
		return nil, nil, err
	}
	for _, document := range documents {
		if !document.Present {
			fingerprints[document.Path] = Fingerprint{}
			for _, alias := range document.Aliases {
				fingerprints[alias.Path] = alias.Fingerprint
			}
			continue
		}
		data := document.Data
		fingerprints[document.Path] = document.Fingerprint
		for _, alias := range document.Aliases {
			fingerprints[alias.Path] = alias.Fingerprint
		}
		servers, err := LoadMCPJSONBytes(data)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid JSON in .mcp.json %s: %w", document.Path, err)
		}
		for name, cfg := range servers {
			result[name] = cfg
		}
	}
	return documents, result, nil
}

// MergeExternalMCPServersFromDocuments applies the same MCP precedence used
// by transaction evaluation. Rush documents are already ordered from lowest
// to highest priority, followed by external documents in global-to-project
// order. A complete Rush definition wins over every external definition;
// disabled-only Rush entries are overlays for an otherwise external server.
func MergeExternalMCPServersFromDocuments(cfg *cfgmodel.Config, rushDocuments, externalDocuments []StableConfigDocument) {
	if cfg.MCP == nil {
		cfg.MCP = make(cfgmodel.MCPs)
	}
	rushDefined := make(map[string]struct{}, len(cfg.MCP))
	for name := range cfg.MCP {
		rushDefined[name] = struct{}{}
	}
	overlays := mcpDisabledOverlays(rushDocuments)
	for _, document := range externalDocuments {
		if !document.Present {
			continue
		}
		servers, err := LoadMCPJSONBytes(document.Data)
		if err != nil {
			continue
		}
		for name, external := range servers {
			if _, defined := rushDefined[name]; defined {
				continue
			}
			if disabled, ok := overlays[name]; ok {
				external.Disabled = disabled
			}
			cfg.MCP[name] = external
		}
	}
}

func mcpDisabledOverlays(documents []StableConfigDocument) map[string]bool {
	overlays := make(map[string]bool)
	for _, document := range documents {
		if !document.Present {
			continue
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(document.Data, &root) != nil {
			continue
		}
		for name, raw := range root.MCP {
			var entry map[string]json.RawMessage
			if json.Unmarshal(raw, &entry) != nil || !IsMCPDisabledOnlyEntry(entry, true) {
				continue
			}
			var disabled bool
			if json.Unmarshal(entry["disabled"], &disabled) == nil {
				overlays[name] = disabled
			}
		}
	}
	return overlays
}

func IsMCPDisabledOnlyEntry(entry map[string]json.RawMessage, exists bool) bool {
	if !exists || len(entry) != 1 {
		return false
	}
	_, hasDisabled := entry["disabled"]
	return hasDisabled
}

func McpEntryFromJSON(data []byte, name string) (map[string]json.RawMessage, bool) {
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(data, &root) != nil {
		return nil, false
	}
	raw, ok := root.MCP[name]
	if !ok {
		return nil, false
	}
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil || entry == nil {
		return nil, false
	}
	return entry, true
}

// NormalizeReloadPath returns the canonical reload-path spelling.
func NormalizeReloadPath(path string) string {
	if path == "" {
		return ""
	}
	return datadir.CanonicalConfigPath(path)
}
