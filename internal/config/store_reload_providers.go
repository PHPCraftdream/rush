// Reload-time rejection of candidate configs that would silently drop a
// live custom provider because its base_url is missing or resolves to
// empty. configureProviders (load_providers.go) only warns and skips on
// the initial Load path; a reload that publishes such a drop would leave
// role pins resolving to a provider that no longer exists, surfacing
// later as an opaque "smart model provider not configured".

package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

// errReloadProviderEndpointDropped is wrapped (fmt.Errorf %w) by the
// error ReloadFromDisk returns when a candidate would drop a live custom
// provider for endpoint reasons; callers surface it unchanged.
var errReloadProviderEndpointDropped = errors.New("live custom provider would be dropped because base_url is missing or empty")

// noteEndpointDroppedProvider records that configureProviders dropped the
// custom provider id because its base_url was missing or empty.
func (c *Config) noteEndpointDroppedProvider(id string) {
	if c.endpointDroppedProviders == nil {
		c.endpointDroppedProviders = make(map[string]struct{})
	}
	c.endpointDroppedProviders[id] = struct{}{}
}

// liveCustomProviderIDs returns the enabled custom (non-known) provider
// ids present in cfg, using the same predicate as configureProviders'
// knownProviderNames set. These are the providers a live snapshot relies
// on; dropping one in a candidate must not pass silently.
func liveCustomProviderIDs(cfg *Config, knownProviders []catwalk.Provider) map[string]struct{} {
	live := make(map[string]struct{})
	if cfg == nil || cfg.Providers == nil {
		return live
	}
	known := make(map[string]bool, len(knownProviders))
	for _, p := range knownProviders {
		known[string(p.ID)] = true
	}
	for id, pc := range cfg.Providers.Seq2() {
		if known[id] || pc.Disable {
			continue
		}
		live[id] = struct{}{}
	}
	return live
}

// rejectLiveProviderEndpointDrops returns an error when any provider
// dropped for endpoint reasons during the candidate's configureProviders
// run was a live custom provider in prev (the snapshot being replaced).
// A provider that was never live — absent from prev, or present but
// disabled — is not a regression and still reloads with a warning.
func rejectLiveProviderEndpointDrops(prev *Config, candidate *Config, knownProviders []catwalk.Provider) error {
	if candidate == nil || len(candidate.endpointDroppedProviders) == 0 {
		return nil
	}
	live := liveCustomProviderIDs(prev, knownProviders)
	var dropped []string
	for id := range candidate.endpointDroppedProviders {
		if _, ok := live[id]; ok {
			dropped = append(dropped, id)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	slices.Sort(dropped)
	return fmt.Errorf("%w: %s", errReloadProviderEndpointDropped, strings.Join(dropped, ", "))
}
