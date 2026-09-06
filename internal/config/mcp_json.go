package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/home"
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
func loadMCPJSON(path string) (map[string]MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return loadMCPJSONBytes(data)
}

func loadMCPJSONBytes(data []byte) (map[string]MCPConfig, error) {
	var file mcpJSONFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	result := make(map[string]MCPConfig, len(file.MCPServers))
	for name, entry := range file.MCPServers {
		mcpType := MCPStdio
		switch MCPType(entry.Type) {
		case MCPSSE:
			mcpType = MCPSSE
		case MCPHttp:
			mcpType = MCPHttp
		}

		result[name] = MCPConfig{
			Type:    mcpType,
			Command: entry.Command,
			Args:    entry.Args,
			Env:     entry.Env,
			URL:     entry.URL,
			Headers: entry.Headers,
			Source:  MCPSourceExternal,
			Timeout: 60, // Higher default for external servers (uvx, npx etc. may be slow to start).
		}
	}
	return result, nil
}

// mcpJSONCandidatePaths returns every location that may provide .mcp.json,
// including files that do not currently exist. Keeping the candidate set
// explicit lets staleness tracking detect later additions.
func mcpJSONCandidatePaths(workingDir string) []string {
	var paths []string

	// Global: ~/.claude/.mcp.json
	if homeDir := home.Dir(); homeDir != "" {
		if path := eligibleConfigCandidate(filepath.Join(homeDir, ".claude", ".mcp.json"), homeConfigOwner()); path != "" {
			paths = append(paths, path)
		}
	}

	// Project root: <workingDir>/.mcp.json
	if workingDir != "" {
		if path := eligibleWorkspaceConfig(filepath.Join(canonicalConfigPath(workingDir), ".mcp.json"), workingDir); path != "" {
			paths = append(paths, path)
		}
	}

	return paths
}

// discoverMCPJSONFiles returns existing .mcp.json files in priority order
// (lowest priority first): global (~/.claude/.mcp.json), then project root.
func discoverMCPJSONFiles(workingDir string) []string {
	var paths []string
	for _, path := range mcpJSONCandidatePaths(workingDir) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			paths = append(paths, path)
		}
	}
	return paths
}

func configAndMCPStalenessPaths(configPaths []string, workingDir string) []string {
	return append(slices.Clone(configPaths), mcpJSONCandidatePaths(workingDir)...)
}

// loadExternalMCPServers discovers and loads all .mcp.json files, returning
// a merged map of server configs. Later files override earlier ones.
func loadExternalMCPServers(workingDir string) map[string]MCPConfig {
	return loadExternalMCPServersFromPaths(discoverMCPJSONFiles(workingDir))
}

// loadExternalMCPServersFromPaths loads one previously discovered, stable
// path set. Reload uses this variant so discovery itself is part of the
// candidate fingerprint instead of being repeated halfway through the read.
func loadExternalMCPServersFromPaths(paths []string) map[string]MCPConfig {
	result := make(map[string]MCPConfig)
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

func loadExternalMCPServersFromStablePaths(paths []string, fingerprints map[string]reloadFileFingerprint) map[string]MCPConfig {
	_, result, err := loadExternalMCPDocumentsStable(paths, fingerprints)
	if err != nil {
		slog.Warn("Failed to load .mcp.json", "error", err)
	}
	return result
}

func loadExternalMCPServersFromStableDocuments(paths []string, fingerprints map[string]reloadFileFingerprint) (map[string]MCPConfig, error) {
	_, result, err := loadExternalMCPDocumentsStable(paths, fingerprints)
	return result, err
}

func loadExternalMCPDocumentsStable(paths []string, fingerprints map[string]reloadFileFingerprint) ([]stableConfigDocument, map[string]MCPConfig, error) {
	result := make(map[string]MCPConfig)
	documents, err := readStableConfigDocuments(paths)
	if err != nil {
		return nil, nil, err
	}
	for _, document := range documents {
		if !document.present {
			fingerprints[document.path] = reloadFileFingerprint{}
			continue
		}
		data := document.data
		fingerprints[document.path] = document.fingerprint
		servers, err := loadMCPJSONBytes(data)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid JSON in .mcp.json %s: %w", document.path, err)
		}
		for name, cfg := range servers {
			result[name] = cfg
		}
	}
	return documents, result, nil
}

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

// mergeExternalMCPServersFromDocuments applies the same MCP precedence used
// by transaction evaluation. Rush documents are already ordered from lowest
// to highest priority, followed by external documents in global-to-project
// order. A complete Rush definition wins over every external definition;
// disabled-only Rush entries are overlays for an otherwise external server.
func mergeExternalMCPServersFromDocuments(cfg *Config, rushDocuments, externalDocuments []stableConfigDocument) {
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	rushDefined := make(map[string]struct{}, len(cfg.MCP))
	for name := range cfg.MCP {
		rushDefined[name] = struct{}{}
	}
	overlays := mcpDisabledOverlays(rushDocuments)
	for _, document := range externalDocuments {
		if !document.present {
			continue
		}
		servers, err := loadMCPJSONBytes(document.data)
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

func mcpDisabledOverlays(documents []stableConfigDocument) map[string]bool {
	overlays := make(map[string]bool)
	for _, document := range documents {
		if !document.present {
			continue
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(document.data, &root) != nil {
			continue
		}
		for name, raw := range root.MCP {
			var entry map[string]json.RawMessage
			if json.Unmarshal(raw, &entry) != nil || !isMCPDisabledOnlyEntry(entry, true) {
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

func readMCPDisabledOverride(store *ConfigStore, scope Scope, name string) (bool, bool) {
	path, err := store.configPath(scope)
	if err != nil {
		return false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	entry, ok := mcpEntryFromJSON(data, name)
	if !isMCPDisabledOnlyEntry(entry, ok) {
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

func isMCPDisabledOnlyEntry(entry map[string]json.RawMessage, exists bool) bool {
	if !exists || len(entry) != 1 {
		return false
	}
	_, hasDisabled := entry["disabled"]
	return hasDisabled
}

func mcpEntryFromJSON(data []byte, name string) (map[string]json.RawMessage, bool) {
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
