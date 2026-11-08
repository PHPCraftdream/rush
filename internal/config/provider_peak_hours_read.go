package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// PeakHoursEntryForScope returns the peak_hours entry a SINGLE scope's
// config file currently stores for providerID — not the merged
// effective view. Returns (nil, nil) when that scope has no provider
// entry or no peak_hours on it. Read-only: no locks, no reload; a
// missing file is (nil, nil); genuine read/decode errors are surfaced.
func (s *ConfigStore) PeakHoursEntryForScope(scope Scope, providerID string) (*ProviderPeakHours, error) {
	if providerID == "" {
		return nil, fmt.Errorf("peak_hours read: empty provider id")
	}
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	// Mirror the write path's target resolution (writeTargetSlot then
	// resolveConfigWriteTarget) WITHOUT acquiring the inter-process lock:
	// this helper is read-only and tolerates a torn concurrent write
	// exactly like any other unlocked reader.
	selected := writeTargetSlot(path)
	target, err := s.resolveConfigWriteTarget(selected)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve config file %q: %w", path, err)
	}
	raw, _, err := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	doc, err := decodeSettingsDocument(target.selectedPath, raw, target.expected)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Providers map[string]struct {
			PeakHours json.RawMessage `json:"peak_hours"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(doc.jsonBytes, &probe); err != nil {
		return nil, fmt.Errorf("failed to decode config file: %w", err)
	}
	entry, ok := probe.Providers[providerID]
	if !ok || len(bytes.TrimSpace(entry.PeakHours)) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(entry.PeakHours), []byte("null")) {
		return nil, nil
	}
	var ph ProviderPeakHours
	if err := json.Unmarshal(entry.PeakHours, &ph); err != nil {
		return nil, err
	}
	return &ph, nil
}
