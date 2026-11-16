// Package cfgmerge owns the JSON document merge pipeline: the folded
// load of all candidate config bytes (loadFromBytes), the MCP
// disabled-overlay sanitation, and the custom-roles overlay strip that
// happens before the generic merge. Extracted from the root config
// package in D2-1.
package cfgmerge

import (
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/config/domains/peak"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/qjebbs/go-jsons"
)

// LoadFromBytes is the moved root loadFromBytes (load_files.go, D2-1),
// exported so the root package can forward its unexported
// loadFromBytes wrapper. mustMarshalConfig was inspected for this file
// per the lane plan but is actually defined in the root load.go:388 and
// is not referenced by this package, so it stayed in the root.
func LoadFromBytes(configs [][]byte) (*cfgmodel.Config, error) {
	if len(configs) == 0 {
		return &cfgmodel.Config{}, nil
	}

	var err error
	configs, err = sanitizeMCPDisabledOverlays(configs)
	if err != nil {
		return nil, err
	}
	configs, roles, err := StripCustomRoleOverlays(configs)
	if err != nil {
		return nil, err
	}
	// Peak-hours scopes are resolved outside the generic merge: the
	// weekly policy form must override between scopes entirely, and an
	// incomplete legacy fragment over a policy is a hard error.
	stripped, peakOverlays, err := peak.StripPeakHoursOverlays(configs)
	if err != nil {
		return nil, err
	}
	resolvedPeakHours := make(map[string]json.RawMessage, len(peakOverlays))
	for id, providerOverlays := range peakOverlays {
		winner, err := peak.ResolvePeakHoursOverlays(id, providerOverlays)
		if err != nil {
			return nil, err
		}
		resolvedPeakHours[id] = winner
	}
	data, err := jsons.Merge(stripped)
	if err != nil {
		return nil, err
	}
	data, err = peak.ApplyPeakHoursOverlays(data, resolvedPeakHours)
	if err != nil {
		return nil, err
	}
	var config cfgmodel.Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if len(roles) != 0 {
		config.Roles = roles
	}
	warnUnknownModelSlots(data)
	return &config, nil
}
