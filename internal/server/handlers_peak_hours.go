package server

// Peak-hours pause/resume/status handlers for the weekly peak-hours
// policy. These follow the same house style as the custom-provider
// handlers in handlers_providers.go.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// handlePauseProviderPeakHours pauses peak-hours throttling for one
// provider until an explicit deadline (duration or RFC3339 "until") or,
// when neither is given, until the currently active peak window ends.
// The pause is written as paused_until on the peak_hours entry of the
// chosen scope; a provider that has no entry in that scope gets one
// cloned from its effective policy first.
func handlePauseProviderPeakHours(a *appPkg.App, c *Client, msg WSMessage) {
	var p PauseProviderPeakHoursPayload
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
	providerCfg, ok := cfg.Providers.Get(p.ID)
	if !ok {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q not found", p.ID))
		return
	}
	scope := scopeFromWire(p.Scope)
	now := time.Now()
	var until time.Time
	switch {
	case p.Until != "":
		t, err := config.ParsePeakPauseDuration(p.Until, now)
		if err != nil {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid until: %v", err))
			return
		}
		until = t
	case p.Duration != "":
		t, err := config.ParsePeakPauseDuration(p.Duration, now)
		if err != nil {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid duration: %v", err))
			return
		}
		until = t
	default:
		// No explicit deadline: pause until the active peak window ends.
		ph := providerCfg.PeakHours
		if ph == nil || ph.IsOff() {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q is not inside an active peak window — pass a duration", p.ID))
			return
		}
		st := ph.Status(now)
		if st.Paused {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("already paused until %s — resume first", st.PausedUntil.Format(time.RFC3339)))
			return
		}
		if !st.InPeak || st.ReopensAt.IsZero() {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q is not inside an active peak window — pass a duration", p.ID))
			return
		}
		until = st.ReopensAt
	}
	entry, err := store.PeakHoursEntryForScope(scope, p.ID)
	if err != nil {
		slog.Warn("ws: failed to resolve peak hours entry", "id", p.ID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if entry == nil {
		// Pause writes the effective policy into the chosen scope, so the
		// paused_until deadline has somewhere to live; AsPolicy already
		// returns an owned copy of the policy leg, and a legacy effective
		// window converts to the equivalent weekly policy so pausing a
		// legacy provider from the web works instead of erroring.
		conv := providerCfg.PeakHours.AsPolicy()
		if conv == nil {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q has no peak-hours policy to pause", p.ID))
			return
		}
		entry = &config.ProviderPeakHours{Policy: conv}
	}
	if entry.Policy == nil {
		// A legacy window converts to the equivalent weekly policy so
		// the pause write never destroys the operator's window; an
		// off-valued legacy entry converts to nil and keeps a fresh
		// policy for the pause stamp alone.
		if conv := entry.AsPolicy(); conv != nil {
			entry.Policy = conv
		} else {
			entry.Policy = &config.PeakHoursPolicy{}
		}
	}
	entry.Policy.PausedUntil = until.Format(time.RFC3339)
	if err := entry.Policy.Validate(); err != nil {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("invalid peakHours: %v", err))
		return
	}
	wrapped := &config.ProviderPeakHours{Policy: entry.Policy}
	// paused_until makes the entry non-zero, so NormalizeForWrite can never
	// drop it; treat a nil result as an invariant violation and refuse.
	if wrapped.NormalizeForWrite() == nil {
		slog.Warn("ws: paused peak hours entry normalized to nil; refusing write", "id", p.ID)
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("failed to persist pause for provider %q", p.ID))
		return
	}
	fieldKey := fmt.Sprintf("providers.%s.peak_hours", p.ID)
	if err := store.SetConfigFieldWithAuthority(scope, fieldKey, wrapped, c.settingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to persist peak hours pause", "id", p.ID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleResumeProviderPeakHours clears an active pause in the chosen
// scope. An empty global entry is removed; an empty workspace entry
// stays explicitly off instead of inheriting a global schedule.
func handleResumeProviderPeakHours(a *appPkg.App, c *Client, msg WSMessage) {
	var p ResumeProviderPeakHoursPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.reply(msg.ID, EventError, nil, "id is required")
		return
	}
	store := a.Store()
	if store.Config() == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	scope := scopeFromWire(p.Scope)
	entry, err := store.PeakHoursEntryForScope(scope, p.ID)
	if err != nil {
		slog.Warn("ws: failed to resolve peak hours entry", "id", p.ID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if entry == nil || entry.Policy == nil || entry.Policy.PausedUntil == "" {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q is not paused in %s scope", p.ID, scope))
		return
	}
	entry.Policy.PausedUntil = ""
	wrapped := &config.ProviderPeakHours{Policy: entry.Policy}
	normalized := wrapped.NormalizeForWrite()
	fieldKey := fmt.Sprintf("providers.%s.peak_hours", p.ID)
	if normalized == nil && scope == config.ScopeGlobal {
		if err := store.RemoveConfigFieldWithAuthority(scope, fieldKey, c.settingsWriteAuthority()); err != nil {
			slog.Warn("ws: failed to clear provider peak_hours", "id", p.ID, "err", err)
			c.reply(msg.ID, EventError, nil, err.Error())
			return
		}
	} else if err := store.SetConfigFieldWithAuthority(scope, fieldKey, normalized, c.settingsWriteAuthority()); err != nil {
		slog.Warn("ws: failed to persist peak hours resume", "id", p.ID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleGetPeakHoursStatus reports the live peak-hours state of one
// provider or, when no id is given, of every configured provider in
// deterministic id order. Read-only: no broadcast.
func handleGetPeakHoursStatus(a *appPkg.App, c *Client, msg WSMessage) {
	var p GetPeakHoursStatusPayload
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
	now := time.Now()
	ids := make([]string, 0, cfg.Providers.Len())
	if p.ID != "" {
		if _, ok := cfg.Providers.Get(p.ID); !ok {
			c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q not found", p.ID))
			return
		}
		ids = append(ids, p.ID)
	} else {
		for id := range cfg.Providers.Seq2() {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	statuses := make([]PeakHoursStatusWire, 0, len(ids))
	for _, id := range ids {
		pc, _ := cfg.Providers.Get(id)
		ph := pc.PeakHours
		var st config.PeakHoursStatus
		if ph != nil && !ph.IsOff() {
			st = ph.Status(now)
		} else {
			st = config.PeakHoursStatus{CheckedAt: now}
		}
		statuses = append(statuses, peakHoursStatusWire(id, st, ph))
	}
	c.reply(msg.ID, EventResponse, PeakHoursStatusesWire{
		CheckedAt: now.Format(time.RFC3339),
		Statuses:  statuses,
	}, "")
}

// peakHoursStatusWire maps a config status into its wire shape, leaving
// RFC3339 strings empty for zero times so they are omitted.
func peakHoursStatusWire(id string, st config.PeakHoursStatus, ph *config.ProviderPeakHours) PeakHoursStatusWire {
	w := PeakHoursStatusWire{
		ProviderID: id,
		CheckedAt:  st.CheckedAt.Format(time.RFC3339),
		InPeak:     st.InPeak,
		Paused:     st.Paused,
		Blocked:    st.Blocked,
	}
	if !st.PausedUntil.IsZero() {
		w.PausedUntil = st.PausedUntil.Format(time.RFC3339)
	}
	if !st.ReopensAt.IsZero() {
		w.ReopensAt = st.ReopensAt.Format(time.RFC3339)
	}
	if !st.NextTransition.IsZero() {
		w.NextTransition = st.NextTransition.Format(time.RFC3339)
	}
	w.Timezone = st.Timezone
	if ph != nil {
		w.Message = ph.EffectiveMessage()
	}
	return w
}
