package app

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	xstrings "github.com/charmbracelet/x/exp/strings"
)

// parseModelStr parses a model string into provider filter and model ID.
// Format: "model-name" or "provider/model-name" or "synthetic/moonshot/kimi-k2".
// This function only checks if the first component is a valid provider name; if not,
// it treats the entire string as a model ID (which may contain slashes).
func parseModelStr(providers map[string]config.ProviderConfig, modelStr string) (providerFilter, modelID string) {
	parts := strings.Split(modelStr, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	// Check if the first part is a valid provider name
	if _, ok := providers[parts[0]]; ok {
		return parts[0], strings.Join(parts[1:], "/")
	}

	// First part is not a valid provider, treat entire string as model ID
	return "", modelStr
}

// modelMatch represents a found model. known is false for the unverified
// passthrough case below (an explicit provider/model that matched nothing in
// that provider's catalog) — callers use it to warn the operator without
// blocking the selection.
type modelMatch struct {
	provider string
	modelID  string
	known    bool
}

func findModels(providers map[string]config.ProviderConfig, smartModel, fastModel string) ([]modelMatch, []modelMatch, error) {
	smartProviderFilter, smartModelID := parseModelStr(providers, smartModel)
	fastProviderFilter, fastModelID := parseModelStr(providers, fastModel)

	// Validate provider filters exist.
	for _, pf := range []struct {
		filter, label string
	}{
		{smartProviderFilter, "smart"},
		{fastProviderFilter, "fast"},
	} {
		if pf.filter != "" {
			if _, ok := providers[pf.filter]; !ok {
				return nil, nil, fmt.Errorf("%s model: provider %q not found in configuration. Use 'rush models' to list available models", pf.label, pf.filter)
			}
		}
	}

	// Find matching models in a single pass.
	var smartMatches, fastMatches []modelMatch
	for name, provider := range providers {
		if provider.Disable {
			continue
		}
		for _, m := range provider.Models {
			if filter(smartModelID, smartProviderFilter, m.ID, name) {
				smartMatches = append(smartMatches, modelMatch{provider: name, modelID: m.ID, known: true})
			}
			if filter(fastModelID, fastProviderFilter, m.ID, name) {
				fastMatches = append(fastMatches, modelMatch{provider: name, modelID: m.ID, known: true})
			}
		}
	}

	// Unverified passthrough: an explicit "provider/model" that matched
	// nothing in that provider's cached catalog is still accepted, since the
	// catalog can be stale (the provider added a model after our last sync —
	// e.g. zai/glm-5.3-flash existing live before `rush providers update`
	// pulled it in) or entirely absent (openai-compat providers with no
	// declared models). Only eligible when a provider WAS named explicitly:
	// a bare "model" search across every provider's catalog has no anchor to
	// fall back to, so a typo there still fails loudly instead of guessing
	// which provider was meant.
	if smartModelID != "" && smartProviderFilter != "" && len(smartMatches) == 0 {
		smartMatches = append(smartMatches, modelMatch{provider: smartProviderFilter, modelID: smartModelID, known: false})
	}
	if fastModelID != "" && fastProviderFilter != "" && len(fastMatches) == 0 {
		fastMatches = append(fastMatches, modelMatch{provider: fastProviderFilter, modelID: fastModelID, known: false})
	}

	return smartMatches, fastMatches, nil
}

func filter(modelFilter, providerFilter, model, provider string) bool {
	return modelFilter != "" && strings.EqualFold(model, modelFilter) &&
		(providerFilter == "" || strings.EqualFold(provider, providerFilter))
}

// ResolveModel does the same smart lookup that --model on `rush run` does,
// but exposed as a public method so the `rush models set` CLI can share
// the rules. modelStr is "model" or "provider/model"; the returned values
// are the unique provider id and the canonical model id from its catalog.
// known is false when modelID was accepted as an unverified "provider/model"
// passthrough rather than a real catalog hit — callers should surface that
// to the operator (see findModels' doc comment for why this is allowed at
// all). Returns an error on no-match or ambiguity.
func (app *App) ResolveModel(modelStr string) (provider, modelID string, known bool, err error) {
	providers := app.config.Config().Providers.Copy()
	matches, _, fErr := findModels(providers, modelStr, "")
	if fErr != nil {
		return "", "", false, fErr
	}
	m, vErr := validateMatches(matches, modelStr, "model")
	if vErr != nil {
		return "", "", false, vErr
	}
	return m.provider, m.modelID, m.known, nil
}

// Validate and return a single match.
func validateMatches(matches []modelMatch, modelID, label string) (modelMatch, error) {
	switch {
	case len(matches) == 0:
		return modelMatch{}, fmt.Errorf("%s model %q not found", label, modelID)
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.provider
		}
		return modelMatch{}, fmt.Errorf(
			"%s model: model %q found in multiple providers: %s. Please specify provider using 'provider/model' format",
			label,
			modelID,
			xstrings.EnglishJoin(names, true),
		)
	}
	return matches[0], nil
}
