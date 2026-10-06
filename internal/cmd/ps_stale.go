// `rush ps`: current-slot lookup and STALE-MODEL cause detection.
package cmd

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

// psSlotNames is the fixed display and JSON order of the four model slots.
var psSlotNames = []string{"smart", "fast", "worker", "reviewer"}

// psSlotModel is one slot's currently selected provider/model pair.
type psSlotModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// psSlots holds the current slot selection; known=false means the settings
// file could not be read and slots are reported as unknown.
type psSlots struct {
	known  bool
	bySlot map[string]psSlotModel
}

// readPsSlots reads the four slots straight from the global settings file.
// Deliberately the lightest read-only path: `rush ps` must not open the
// config store or the sessions DB. Any failure degrades to known=false.
func readPsSlots() psSlots {
	data, err := os.ReadFile(config.GlobalConfigData())
	if err != nil {
		return psSlots{}
	}
	var doc struct {
		Models map[string]psSlotModel `json:"models"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return psSlots{}
	}
	out := psSlots{known: true, bySlot: map[string]psSlotModel{}}
	for _, slot := range psSlotNames {
		if m, ok := doc.Models[slot]; ok && m.Model != "" {
			out.bySlot[slot] = m
		}
	}
	return out
}

// Stale-model causes, in detection-precedence order.
const (
	psCauseSlotAtStart     = "slot@start"
	psCausePerCall         = "per-call"
	psCauseSessionOverride = "session-override"
)

// psStaleModel is one stale-model finding for a live row.
type psStaleModel struct {
	Model string `json:"model"`
	Cause string `json:"cause"`
	Slot  string `json:"slot,omitempty"`
}

// psStaleModels lists the live row's models that no current slot points to,
// most recently used first, each with the cause that explains it. Stopped
// and stale rows never flag, and unreadable slots disable detection.
func psStaleModels(e heartbeat.Entry, slots psSlots) []psStaleModel {
	if !slots.known || (e.State != "running" && e.State != "idle") {
		return nil
	}
	current := map[string]bool{}
	for _, m := range slots.bySlot {
		current[psModelKey(m.Provider, m.Model)] = true
	}
	var out []psStaleModel
	for _, key := range psModelKeysByRecency(e) {
		m := e.Models[key]
		mk := psModelKey(m.Provider, m.Model)
		if mk == "" || mk == psModelKey("", psOtherModelName) || current[mk] {
			continue
		}
		cause, slot := psStaleCause(e.SlotsAtStart, m.Provider, m.Model, m.Sources)
		out = append(out, psStaleModel{Model: mk, Cause: cause, Slot: slot})
	}
	return out
}

// psStaleCause explains why a live process is using a model no slot points
// to: it was a slot when the process started (slot@start), it carries call
// sources (per-call, e.g. title/summary/keepalive), or the session picked
// it itself (session-override).
func psStaleCause(slotsAtStart map[string]string, provider, model string, sources []string) (cause, slot string) {
	if s := psSlotAtStartFor(slotsAtStart, provider, model); s != "" {
		return psCauseSlotAtStart, s
	}
	if len(sources) > 0 {
		return psCausePerCall, ""
	}
	return psCauseSessionOverride, ""
}

// psSlotAtStartFor returns the slot whose start-time selection matches the
// model, tolerating both "provider/model" and bare-model value forms.
func psSlotAtStartFor(slotsAtStart map[string]string, provider, model string) string {
	pair := psModelKey(provider, model)
	for _, slot := range slices.Sorted(maps.Keys(slotsAtStart)) {
		v := strings.ToLower(strings.TrimSpace(slotsAtStart[slot]))
		if v != "" && (v == pair || v == strings.ToLower(model)) {
			return slot
		}
	}
	return ""
}
