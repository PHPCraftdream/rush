package config

import (
	"encoding/json"
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
		paths = append(paths, filepath.Join(homeDir, ".claude", ".mcp.json"))
	}

	// Project root: <workingDir>/.mcp.json
	if workingDir != "" {
		paths = append(paths, filepath.Join(workingDir, ".mcp.json"))
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

// mergeExternalMCPServers injects .mcp.json servers into the config's MCP map.
// Servers already defined in rush.json take full precedence. For external
// servers, the disabled state is read from the rush config store.
func mergeExternalMCPServers(cfg *Config, store *ConfigStore, external map[string]MCPConfig, loadedPaths []string) {
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	for name, extCfg := range external {
		disabled, overridden := externalMCPDisabledOverride(store, name, loadedPaths)
		if _, exists := cfg.MCP[name]; exists && !overridden {
			// A complete Rush definition takes precedence over .mcp.json.
			continue
		}
		if overridden {
			extCfg.Disabled = disabled
		}
		cfg.MCP[name] = extCfg
	}
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

// externalMCPDisabledOverride returns an overlay only when no complete Rush
// definition owns name. Project definitions are deliberately treated as
// complete definitions because they cannot be represented by Scope.
func externalMCPDisabledOverride(store *ConfigStore, name string, loadedPaths []string) (bool, bool) {
	if hasNonOverlayMCPDefinition(store, name, loadedPaths) {
		return false, false
	}
	if disabled, ok := readMCPDisabledOverride(store, ScopeWorkspace, name); ok {
		return disabled, true
	}
	return readMCPDisabledOverride(store, ScopeGlobal, name)
}

func hasNonOverlayMCPDefinition(store *ConfigStore, name string, loadedPaths []string) bool {
	workspacePath, _ := store.configPath(ScopeWorkspace)
	globalPath, _ := store.configPath(ScopeGlobal)
	paths := append(slices.Clone(loadedPaths), workspacePath, globalPath)
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entry, ok := mcpEntryFromJSON(data, name)
		if !ok {
			continue
		}
		if path == filepath.Clean(workspacePath) || path == filepath.Clean(globalPath) {
			if isMCPDisabledOnlyEntry(entry, true) {
				continue
			}
		}
		return true
	}
	return false
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
