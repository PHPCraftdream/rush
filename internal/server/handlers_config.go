package server

// Config-surface handlers: the ConfigWire snapshot clients bootstrap
// from, theme/keep-alive/debug toggles, log tailing, and context- and
// skills-path management.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/PHPCraftdream/rush/internal/version"
)

func buildConfigWire(a *appPkg.App) (ConfigWire, bool) {
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		return ConfigWire{}, false
	}
	wire := ConfigWire{
		Models:    make(map[string]ModelEntryWire, len(cfg.Models)),
		Providers: make(map[string]ProviderWire),
	}
	for k, v := range cfg.Models {
		wire.Models[string(k)] = ModelEntryWire{
			Provider: v.Provider,
			Model:    v.Model,
		}
	}

	enabledIDs := make(map[string]config.ProviderConfig)
	for _, ep := range cfg.EnabledProviders() {
		enabledIDs[ep.ID] = ep
	}

	for _, p := range store.KnownProviders() {
		id := string(p.ID)
		if ep, ok := enabledIDs[id]; ok {
			pw := ProviderWire{Name: p.Name, Enabled: true, Type: string(p.Type), APIKeySet: ep.APIKey != "", PeakHours: peakHoursToWire(ep.PeakHours), Models: make([]ModelInfoWire, len(ep.Models))}
			for i, m := range ep.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[id] = pw
		} else {
			pw := ProviderWire{Name: p.Name, Enabled: false, Type: string(p.Type), Models: make([]ModelInfoWire, len(p.Models))}
			for i, m := range p.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[id] = pw
		}
	}

	for _, ep := range cfg.EnabledProviders() {
		if _, exists := wire.Providers[ep.ID]; !exists {
			// Custom provider not in the known catalog.
			// Built-in auto-detected providers (e.g. local-cli) are not user-added
			// custom providers and must not appear in the Custom Providers modal.
			isCustom := ep.Type != cliprovider.ProviderType
			pw := ProviderWire{
				Name:      ep.Name,
				Enabled:   true,
				Type:      string(ep.Type),
				BaseURL:   ep.BaseURL,
				IsCustom:  isCustom,
				APIKeySet: ep.APIKey != "",
				PeakHours: peakHoursToWire(ep.PeakHours),
				Models:    make([]ModelInfoWire, len(ep.Models)),
			}
			for i, m := range ep.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[ep.ID] = pw
		}
	}

	wire.Debug = cfg.Options.Debug
	if cfg.Options != nil {
		wire.ContextPaths = cfg.Options.ContextPaths
		wire.SkillsPaths = cfg.Options.SkillsPaths
		wire.InitializeAs = cfg.Options.InitializeAs
	}
	if cfg.Options != nil && cfg.Options.TUI != nil {
		wire.Theme = cfg.Options.TUI.Theme
	}
	// KeepAliveEnabled: default ON. nil → true; explicit value passes through.
	wire.KeepAliveEnabled = true
	if cfg.Options != nil && cfg.Options.KeepAliveEnabled != nil {
		wire.KeepAliveEnabled = *cfg.Options.KeepAliveEnabled
	}

	for _, m := range cfg.RecentModels[config.SelectedModelTypeSmart] {
		wire.RecentSmartModels = append(wire.RecentSmartModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}
	for _, m := range cfg.RecentModels[config.SelectedModelTypeFast] {
		wire.RecentFastModels = append(wire.RecentFastModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}

	wire.Version = version.FullVersion()
	wire.CWD = store.WorkingDir()

	return wire, true
}

func handleGetConfig(a *appPkg.App, c *Client, msg WSMessage) {
	wire, ok := buildConfigWire(a)
	if !ok {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	c.reply(msg.ID, EventConfig, wire, "")
}

func handleGetLogs(a *appPkg.App, c *Client, msg WSMessage) {
	var p GetLogsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	// Get log file path
	logPath := a.Store().LogPath()
	if logPath == "" {
		c.reply(msg.ID, EventError, nil, "log path not configured")
		return
	}

	// Read last N lines from log file
	logs, err := readLastNLines(logPath, p.Lines)
	if err != nil {
		slog.Error("Failed to read log file", "path", logPath, "error", err)
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("failed to read logs: %v", err))
		return
	}

	c.reply(msg.ID, EventLogs, logs, "")
}

// readLastNLines reads the last N lines from a file (0 = all lines)
func readLastNLines(path string, n int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	if n <= 0 {
		return string(data), nil
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n"), nil
	}

	return strings.Join(lines[len(lines)-n:], "\n"), nil
}

func handleSetTheme(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetThemePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Store().SetTheme(config.ScopeGlobal, p.Theme); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
}

func handleSetKeepAlive(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetKeepAlivePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Store().SetKeepAliveEnabled(config.ScopeGlobal, p.Enabled); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
}

// ── Debug settings ────────────────────────────────────────────────────────────

func handleSetDebug(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetDebugPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	cfg.Options.Debug = p.Debug
	if err := store.SetConfigField(config.ScopeGlobal, "options.debug", p.Debug); err != nil {
		slog.Warn("ws: failed to persist debug setting", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Context paths ─────────────────────────────────────────────────────────────

func handleAddContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddContextPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Path == "" {
		c.reply(msg.ID, EventError, nil, "path is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	if slices.Contains(cfg.Options.ContextPaths, p.Path) {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.ContextPaths = append(cfg.Options.ContextPaths, p.Path)
	if err := store.SetConfigField(config.ScopeGlobal, "options.context_paths", cfg.Options.ContextPaths); err != nil {
		slog.Warn("ws: failed to persist context paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveContextPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.ContextPaths = slices.DeleteFunc(cfg.Options.ContextPaths, func(s string) bool { return s == p.Path })
	if err := store.SetConfigField(config.ScopeGlobal, "options.context_paths", cfg.Options.ContextPaths); err != nil {
		slog.Warn("ws: failed to persist context paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Skills paths ──────────────────────────────────────────────────────────────

func handleGetSkills(a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	paths := []string{}
	if cfg.Options != nil {
		paths = cfg.Options.SkillsPaths
	}
	discovered := skills.Discover(paths)
	commands := skills.DiscoverCommands(skills.DefaultCommandDirs())
	all := append(discovered, commands...)
	infos := make([]SkillInfo, 0, len(all))
	for _, s := range all {
		infos = append(infos, SkillInfo{
			Name:         s.Name,
			Description:  s.Description,
			Path:         s.Path,
			Source:       s.Source,
			Instructions: s.Instructions,
		})
	}
	c.reply(msg.ID, EventSkills, SkillsSnapshot{Skills: infos, Paths: paths}, "")
}

func handleAddSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddSkillsPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Path == "" {
		c.reply(msg.ID, EventError, nil, "path is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	if slices.Contains(cfg.Options.SkillsPaths, p.Path) {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.SkillsPaths = append(cfg.Options.SkillsPaths, p.Path)
	if err := store.SetConfigField(config.ScopeGlobal, "options.skills_paths", cfg.Options.SkillsPaths); err != nil {
		slog.Warn("ws: failed to persist skills paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveSkillsPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.SkillsPaths = slices.DeleteFunc(cfg.Options.SkillsPaths, func(s string) bool { return s == p.Path })
	if err := store.SetConfigField(config.ScopeGlobal, "options.skills_paths", cfg.Options.SkillsPaths); err != nil {
		slog.Warn("ws: failed to persist skills paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
