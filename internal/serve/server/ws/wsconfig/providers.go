package wsconfig

// Provider-configuration handlers: provider API keys, custom providers,
// and per-provider peak hours, plus the wire-to-config helpers they share.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	hub "github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wirePkg "github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

func HandleSetProviderKey(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleSetProviderKey(a, c, msg)
}

func HandleRemoveProviderKey(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleRemoveProviderKey(a, c, msg)
}

func HandleAddCustomProvider(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleAddCustomProvider(a, c, msg)
}

func HandleUpdateCustomProvider(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleUpdateCustomProvider(a, c, msg)
}

func handleSetProviderKey(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	if err := store.SetProviderAPIKeyWithAuthority(config.ScopeGlobal, p.ProviderID, p.APIKey, c.SettingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to set provider API key", "provider", p.ProviderID, "err", err)
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	// Broadcast updated config to all clients
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveProviderKey(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.RemoveProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	if err := store.RemoveProviderAPIKeyWithAuthority(config.ScopeGlobal, p.ProviderID, c.SettingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to remove provider API key", "provider", p.ProviderID, "err", err)
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Custom providers ──────────────────────────────────────────────────────────

// PeakHoursFromWire and PeakHoursToWire are narrow compatibility exports for
// package-server callers while peak-hours tests and handlers move packages.
func PeakHoursFromWire(w *wirePkg.PeakHoursWirePayload) (*config.ProviderPeakHours, error) {
	return peakHoursFromWire(w)
}

func PeakHoursToWire(w *config.ProviderPeakHours) *wirePkg.PeakHoursWirePayload {
	return peakHoursToWire(w)
}

// peakHoursFromWire converts the optional WS payload into a validated
// config.ProviderPeakHours. The payload carries either the legacy
// single-window form (start/end/message) or the weekly policy form
// (timezone/windows/pausedUntil, optionally with message); mixing the two
// forms is rejected. Returns (nil, nil) when the payload is absent
// (feature off). Returns (nil, err) when the payload fails validation.
// The caller is responsible for replying with EventError on err.
func peakHoursFromWire(w *wirePkg.PeakHoursWirePayload) (*config.ProviderPeakHours, error) {
	if w == nil {
		return nil, nil
	}
	if w.Timezone != "" || w.Windows != nil || w.PausedUntil != "" {
		if w.Start != "" || w.End != "" {
			return nil, fmt.Errorf("peakHours mixes legacy start/end with the weekly windows form")
		}
		windows := make([]config.PeakHoursWindowSpec, len(w.Windows))
		for i, win := range w.Windows {
			windows[i] = config.PeakHoursWindowSpec{Days: win.Days, Start: win.Start, End: win.End}
		}
		policy := config.PeakHoursPolicy{
			Timezone:    w.Timezone,
			Windows:     windows,
			Message:     w.Message,
			PausedUntil: w.PausedUntil,
		}
		if err := policy.Validate(); err != nil {
			return nil, err
		}
		return &config.ProviderPeakHours{Policy: &policy}, nil
	}
	window := config.PeakHoursWindow{Start: w.Start, End: w.End, Message: w.Message}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	return &config.ProviderPeakHours{Legacy: &window}, nil
}

// peakHoursToWire converts a config.ProviderPeakHours pointer into the WS
// payload shape, preserving which form (legacy single window or weekly
// policy) the entry uses. Returns nil when the entry is absent (feature
// off) so the JSON field is omitted.
func peakHoursToWire(w *config.ProviderPeakHours) *wirePkg.PeakHoursWirePayload {
	if w == nil {
		return nil
	}
	if w.Legacy != nil {
		return &wirePkg.PeakHoursWirePayload{Start: w.Legacy.Start, End: w.Legacy.End, Message: w.Legacy.Message}
	}
	if w.Policy == nil {
		return nil
	}
	windows := make([]wirePkg.PeakHoursWindowWire, len(w.Policy.Windows))
	for i, win := range w.Policy.Windows {
		windows[i] = wirePkg.PeakHoursWindowWire{Days: win.Days, Start: win.Start, End: win.End}
	}
	return &wirePkg.PeakHoursWirePayload{
		Timezone:    w.Policy.Timezone,
		Windows:     windows,
		Message:     w.Policy.Message,
		PausedUntil: w.Policy.PausedUntil,
	}
}

func ScopeFromWire(s string) config.Scope { return scopeFromWire(s) }

// scopeFromWire resolves a provider-config wire scope string ("global" /
// "local", case-insensitive) into a config.Scope. Empty or unrecognised
// values default to config.ScopeGlobal, matching every scope-aware CLI
// command's default (rush providers, rush mcp, rush claude-init, ...).
func scopeFromWire(s string) config.Scope {
	if strings.EqualFold(s, "local") {
		return config.ScopeWorkspace
	}
	return config.ScopeGlobal
}

func handleAddCustomProvider(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.AddCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" || p.BaseURL == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "id and baseUrl are required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	if _, exists := cfg.Providers.Get(p.ID); exists {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("provider %q already exists", p.ID))
		return
	}
	models := make([]catwalk.Model, len(p.Models))
	for i, m := range p.Models {
		models[i] = catwalk.Model{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			CostPer1MIn:   m.CostPer1MIn,
			CostPer1MOut:  m.CostPer1MOut,
		}
	}
	providerCfg := config.ProviderConfig{
		ID:        p.ID,
		Name:      cmp.Or(p.Name, p.ID),
		Type:      catwalk.Type(cmp.Or(p.Type, "openai-compat")),
		BaseURL:   p.BaseURL,
		APIKey:    p.APIKey,
		Models:    models,
		PeakHours: peakHours,
	}
	scope := scopeFromWire(p.Scope)
	field := fmt.Sprintf("providers.%s", p.ID)
	if err := store.SetConfigFieldsWithAuthority(scope, customProviderWriteFields(field, providerCfg, scope), c.SettingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to persist custom provider", "id", p.ID, "err", err)
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	cfg.Providers.Set(p.ID, providerCfg)
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveCustomProvider(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.RemoveCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "id is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	field := fmt.Sprintf("providers.%s", p.ID)
	if err := store.RemoveConfigFieldWithAuthority(scopeFromWire(p.Scope), field, c.SettingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to remove custom provider", "id", p.ID, "err", err)
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	// RemoveConfigField auto-reloads and publishes a new snapshot, making cfg
	// stale. Remove the explicitly requested provider from the current live
	// config as well, including catalog providers with no stored override.
	if liveCfg := store.Config(); liveCfg != nil {
		liveCfg.Providers.Del(p.ID)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateCustomProvider(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.UpdateCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.OldID == "" || p.ID == "" || p.BaseURL == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "oldId, id and baseUrl are required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	// Preserve the existing key when the edit form omits it. A non-empty
	// key explicitly replaces the current value.
	apiKey := p.APIKey
	if apiKey == "" {
		if existing, ok := cfg.Providers.Get(p.OldID); ok {
			apiKey = existing.APIKey
		}
	}
	// Build updated models.
	models := make([]catwalk.Model, len(p.Models))
	for i, m := range p.Models {
		models[i] = catwalk.Model{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			CostPer1MIn:   m.CostPer1MIn,
			CostPer1MOut:  m.CostPer1MOut,
		}
	}
	providerCfg := config.ProviderConfig{
		ID:        p.ID,
		Name:      cmp.Or(p.Name, p.ID),
		Type:      catwalk.Type(cmp.Or(p.Type, "openai-compat")),
		BaseURL:   p.BaseURL,
		APIKey:    apiKey,
		Models:    models,
		PeakHours: peakHours,
	}
	scope := scopeFromWire(p.Scope)
	newProviderField := fmt.Sprintf("providers.%s", p.ID)
	if p.OldID != p.ID {
		if _, exists := cfg.Providers.Get(p.ID); exists {
			c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("provider %q already exists", p.ID))
			return
		}
	}
	if err := store.SetConfigFieldsWithAuthority(scope, customProviderWriteFields(newProviderField, providerCfg, scope), c.SettingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to persist updated custom provider", "id", p.ID, "err", err)
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	if p.OldID != p.ID {
		if err := store.RemoveConfigFieldWithAuthority(scope, fmt.Sprintf("providers.%s", p.OldID), c.SettingsWriteAuthority()); err != nil {
			slog.Warn("ws: failed to remove old custom provider; rolling back new provider", "id", p.OldID, "new_id", p.ID, "err", err)
			if rollbackErr := store.RemoveConfigFieldWithAuthority(scope, newProviderField, c.SettingsWriteAuthority()); rollbackErr != nil {
				slog.Warn("ws: failed to roll back new custom provider after rename failure", "id", p.ID, "err", rollbackErr)
			}
			c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
			return
		}
	}
	if p.OldID != p.ID {
		cfg.Providers.Del(p.OldID)
	}
	cfg.Providers.Set(p.ID, providerCfg)
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

// handleSetProviderPeakHours sets or clears ONLY the peak_hours field on any
// provider — built-in/catwalk-known (e.g. "anthropic", "zai") or custom.
// Unlike handleUpdateCustomProvider (which replaces every field and is only
// safe on a custom provider the client fully owns), this is a targeted
// single-field write, mirroring `rush providers set <id> --peak-hours` on
// the CLI side. This is what lets the web UI manage peak hours for a
// built-in provider without needing to know/round-trip its type, base URL,
// API key, or model list. The payload may use the legacy single-window
// form or the weekly policy form; both are validated before persisting.
// A null/off value persists an explicit null override in the workspace
// scope (suppressing a global policy) and removes the field in the global
// scope (inherit-nothing); persistence failure fails the request without
// mutating in-memory state.
func HandleSetProviderPeakHours(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleSetProviderPeakHours(a, c, msg)
}

func HandleSetMCPDisabled(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	SetMCPDisabled(ctx, a, c, msg)
}

func handleSetProviderPeakHours(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetProviderPeakHoursPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "id is required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	providerCfg, ok := cfg.Providers.Get(p.ID)
	if !ok {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("provider %q not found", p.ID))
		return
	}
	scope := scopeFromWire(p.Scope)
	fieldKey := fmt.Sprintf("providers.%s.peak_hours", p.ID)
	// Workspace off stays explicit; global off removes the stored override.
	normalized := peakHours.NormalizeForWrite()
	if normalized == nil {
		if scope == config.ScopeWorkspace {
			if err := store.SetConfigFieldWithAuthority(scope, fieldKey, nil, c.SettingsWriteAuthority()); err != nil {
				slog.Warn("ws: failed to persist explicit peak_hours off", "id", p.ID, "err", err)
				c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("failed to persist peak_hours: %v", err))
				return
			}
		} else if err := store.RemoveConfigFieldWithAuthority(scope, fieldKey, c.SettingsWriteAuthority()); err != nil {
			slog.Warn("ws: failed to clear provider peak_hours", "id", p.ID, "err", err)
			c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("failed to persist peak_hours: %v", err))
			return
		}
	} else {
		if err := store.SetConfigFieldWithAuthority(scope, fieldKey, normalized, c.SettingsWriteAuthority()); err != nil {
			c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("failed to persist peak_hours: %v", err))
			return
		}
	}
	// Update the in-memory merged map so buildConfigWire reflects the
	// change immediately, without a full config reload. A nil normalized
	// reflects the removal.
	providerCfg.PeakHours = normalized
	cfg.Providers.Set(p.ID, providerCfg)
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

// MCP server configuration handlers: enable/disable, add, remove, and
// update of configured MCP servers.

func SetMCPDisabled(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetMCPDisabledPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	var err error
	// Trusted-UI connections may write settings while the settings lock is
	// active: route to the Owner's TrustedUI variants, which lift ONLY the
	// settings-lock veto. Non-UI clients keep the guarded behavior.
	trusted := c.SettingsWriteAuthority() == config.SettingsAuthorityTrustedUI
	if p.Disabled {
		// Mutations route through the App's own MCP owner (task #923): a second
		// application-mode App holds a standalone owner, and package-level calls
		// would resolve the process-current (first App's) owner and fail with
		// mcp.ErrMCPConfigStoreBusy. Nil-safe: a SkipMCP App keeps the legacy
		// package-function behavior.
		if trusted {
			err = a.MCPOwner().DisableServerTrustedUI(ctx, store, p.Name)
		} else {
			err = a.MCPOwner().DisableServer(ctx, store, p.Name)
		}
	} else {
		if trusted {
			err = a.MCPOwner().EnableServerTrustedUI(ctx, store, p.Name)
		} else {
			err = a.MCPOwner().EnableServer(ctx, store, p.Name)
		}
	}
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleAddMCPServer(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.AddMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.Name == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "name is required")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	// Trusted-UI connections may write settings while the settings lock is
	// active: route to the Owner's TrustedUI variant, which lifts ONLY the
	// settings-lock veto. Non-UI clients keep the guarded behavior.
	if c.SettingsWriteAuthority() == config.SettingsAuthorityTrustedUI {
		if err := a.MCPOwner().AddServerTrustedUI(ctx, store, p.Name, mcpCfg); err != nil {
			c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
			return
		}
		c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	if err := a.MCPOwner().AddServer(ctx, store, p.Name, mcpCfg); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveMCPServer(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.RemoveMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	// Trusted-UI connections may write settings while the settings lock is
	// active: route to the Owner's TrustedUI variant, which lifts ONLY the
	// settings-lock veto. Non-UI clients keep the guarded behavior.
	if c.SettingsWriteAuthority() == config.SettingsAuthorityTrustedUI {
		if err := a.MCPOwner().RemoveServerTrustedUI(store, p.Name); err != nil {
			c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
			return
		}
		c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	if err := a.MCPOwner().RemoveServer(store, p.Name); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMCPServer(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.UpdateMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	if p.OldName == "" || p.Name == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "oldName and name are required")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	// Trusted-UI connections may write settings while the settings lock is
	// active: route to the Owner's TrustedUI variant, which lifts ONLY the
	// settings-lock veto. Non-UI clients keep the guarded behavior.
	if c.SettingsWriteAuthority() == config.SettingsAuthorityTrustedUI {
		if err := a.MCPOwner().ReplaceServerTrustedUI(ctx, store, p.OldName, p.Name, mcpCfg); err != nil {
			c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
			return
		}
		c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	if err := a.MCPOwner().ReplaceServer(ctx, store, p.OldName, p.Name, mcpCfg); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}
