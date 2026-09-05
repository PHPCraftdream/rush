package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

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

// discoverMCPJSONFiles returns paths to .mcp.json files in priority order
// (lowest priority first): global (~/.claude/.mcp.json), then project root.
func discoverMCPJSONFiles(workingDir string) []string {
	var paths []string

	// Global: ~/.claude/.mcp.json
	if homeDir := home.Dir(); homeDir != "" {
		global := filepath.Join(homeDir, ".claude", ".mcp.json")
		if _, err := os.Stat(global); err == nil {
			paths = append(paths, global)
		}
	}

	// Project root: <workingDir>/.mcp.json
	if workingDir != "" {
		project := filepath.Join(workingDir, ".mcp.json")
		if _, err := os.Stat(project); err == nil {
			paths = append(paths, project)
		}
	}

	return paths
}

// loadExternalMCPServers discovers and loads all .mcp.json files, returning
// a merged map of server configs. Later files override earlier ones.
func loadExternalMCPServers(workingDir string) map[string]MCPConfig {
	result := make(map[string]MCPConfig)
	for _, path := range discoverMCPJSONFiles(workingDir) {
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
func mergeExternalMCPServers(cfg *Config, store *ConfigStore, external map[string]MCPConfig) {
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	for name, extCfg := range external {
		if _, exists := cfg.MCP[name]; exists {
			// Rush's own config defines this server — it takes precedence.
			continue
		}
		// Check if the user has toggled this server off via the UI. Decode the
		// literal map key directly; constructing a dynamic gjson path would
		// treat dots, wildcards, and backslashes in a server name as syntax.
		if disabled, ok := readMCPDisabledOverride(store, ScopeWorkspace, name); ok {
			extCfg.Disabled = disabled
		} else if disabled, ok := readMCPDisabledOverride(store, ScopeGlobal, name); ok {
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
	var root struct {
		MCP map[string]struct {
			Disabled *bool `json:"disabled"`
		} `json:"mcp"`
	}
	if json.Unmarshal(data, &root) != nil {
		return false, false
	}
	entry, ok := root.MCP[name]
	if !ok || entry.Disabled == nil {
		return false, false
	}
	return *entry.Disabled, true
}
