// MCP policy helpers that combine external documents with the root config.
package config

import (
	"crypto/sha256"
	"encoding/json"
	"maps"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"
)

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
