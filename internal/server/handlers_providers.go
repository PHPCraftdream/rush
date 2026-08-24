package server

// Provider-configuration handlers: provider API keys, custom providers,
// and per-provider peak hours, plus the wire-to-config helpers they share.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"

	"charm.land/catwalk/pkg/catwalk"
)

func handleSetProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := store.SetProviderAPIKey(config.ScopeGlobal, p.ProviderID, p.APIKey); err != nil {
		slog.Warn("ws: failed to set provider API key", "provider", p.ProviderID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Broadcast updated config to all clients
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := store.RemoveProviderAPIKey(config.ScopeGlobal, p.ProviderID); err != nil {
		slog.Warn("ws: failed to remove provider API key", "provider", p.ProviderID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Custom providers ──────────────────────────────────────────────────────────

// peakHoursFromWire converts the optional WS payload into a validated
// config.PeakHoursWindow. Returns (nil, nil) when the payload is absent
// (feature off). Returns (nil, err) when the payload fails validation.
// The caller is responsible for replying with EventError on err.
func peakHoursFromWire(w *PeakHoursWirePayload) (*config.PeakHoursWindow, error) {
	if w == nil {
		return nil, nil
	}
	window := config.PeakHoursWindow{Start: w.Start, End: w.End}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	return &window, nil
}

// peakHoursToWire converts a config.PeakHoursWindow pointer into the WS
// payload shape. Returns nil when the window is absent (feature off) so
// the JSON field is omitted.
func peakHoursToWire(w *config.PeakHoursWindow) *PeakHoursWirePayload {
	if w == nil {
		return nil
	}
	return &PeakHoursWirePayload{Start: w.Start, End: w.End}
}

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

func handleAddCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" || p.BaseURL == "" {
		c.reply(msg.ID, EventError, nil, "id and baseUrl are required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if _, exists := cfg.Providers.Get(p.ID); exists {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q already exists", p.ID))
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
	cfg.Providers.Set(p.ID, providerCfg)
	if err := store.SetConfigField(scopeFromWire(p.Scope), fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
		slog.Warn("ws: failed to persist custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.reply(msg.ID, EventError, nil, "id is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	cfg.Providers.Del(p.ID)
	// RemoveConfigField returns an error when there is no override to
	// delete, which is expected for a default-provider id that only
	// exists in the built-in catalog — benign in that case. A real
	// failure (e.g. disk / parse error) surfaces the same way. Scope must
	// match the scope the provider was added under (p.Scope), or this is
	// a silent no-op against the wrong config file.
	if err := store.RemoveConfigField(scopeFromWire(p.Scope), fmt.Sprintf("providers.%s", p.ID)); err != nil {
		slog.Warn("ws: failed to remove custom provider override (benign for default/catalog providers with no override set)", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.OldID == "" || p.ID == "" || p.BaseURL == "" {
		c.reply(msg.ID, EventError, nil, "oldId, id and baseUrl are required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove the old entry. Uses the SAME scope as the update target: a
	// rename is expected to stay within the scope the operator picked in
	// the edit form, not silently move between global/local.
	scope := scopeFromWire(p.Scope)
	cfg.Providers.Del(p.OldID)
	if p.OldID != p.ID {
		if err := store.RemoveConfigField(scope, fmt.Sprintf("providers.%s", p.OldID)); err != nil {
			slog.Warn("ws: failed to remove old custom provider", "id", p.OldID, "err", err)
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
		APIKey:    p.APIKey,
		Models:    models,
		PeakHours: peakHours,
	}
	cfg.Providers.Set(p.ID, providerCfg)
	if err := store.SetConfigField(scope, fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
		slog.Warn("ws: failed to persist updated custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleSetProviderPeakHours sets or clears ONLY the peak_hours field on any
// provider — built-in/catwalk-known (e.g. "anthropic", "zai") or custom.
// Unlike handleUpdateCustomProvider (which replaces every field and is only
// safe on a custom provider the client fully owns), this is a targeted
// single-field write, mirroring `rush providers set <id> --peak-hours` on
// the CLI side. This is what lets the web UI manage peak hours for a
// built-in provider without needing to know/round-trip its type, base URL,
// API key, or model list.
func handleSetProviderPeakHours(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetProviderPeakHoursPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.reply(msg.ID, EventError, nil, "id is required")
		return
	}
	peakHours, err := peakHoursFromWire(p.PeakHours)
	if err != nil {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	providerCfg, ok := cfg.Providers.Get(p.ID)
	if !ok {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q not found", p.ID))
		return
	}
	scope := scopeFromWire(p.Scope)
	fieldKey := fmt.Sprintf("providers.%s.peak_hours", p.ID)
	if peakHours == nil {
		if err := store.RemoveConfigField(scope, fieldKey); err != nil {
			slog.Warn("ws: failed to clear provider peak_hours", "id", p.ID, "err", err)
		}
	} else {
		if err := store.SetConfigField(scope, fieldKey, peakHours); err != nil {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("failed to persist peak_hours: %v", err))
			return
		}
	}
	// Update the in-memory merged map so buildConfigWire reflects the
	// change immediately, without a full config reload.
	providerCfg.PeakHours = peakHours
	cfg.Providers.Set(p.ID, providerCfg)
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
