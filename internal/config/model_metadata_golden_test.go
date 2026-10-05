// Characterization golden test for the model metadata pipeline (#1181).
// Captured BEFORE the single-source refactor: the only golden delta the
// refactor may produce is the zai glm-5.3 family rows (context 1000000,
// max 131072 where zero). Everything else -- every provider x model x
// ContextWindow/DefaultMaxTokens pair and every LiveEfforts entry -- is
// frozen by this file.
package config

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

var updateModelMetadataGolden = flag.Bool("update-model-metadata-golden", false, "rewrite the model metadata golden file")

// TestModelMetadataGolden pins the metadata loadProviders produces from a
// fixed environment: an embedded-catwalk-shaped Z.AI provider whose live
// /models answer (and therefore any `rush providers update zai` dump) has no
// context windows at all, a Codex account catalog served from a cache written
// by an older Rush (gpt-6 entries at 272000), a user-entered Codex value
// BELOW the documented floor, and zeros in rush.json (which are unknown, not
// user values).
func TestModelMetadataGolden(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	zaiModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Recorded shape of z.ai's /models answer: ids and display
		// names only, no context_window field.
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"glm-5.3","display_name":"GLM-5.3"},` +
			`{"id":"glm-5.3-flash","display_name":"GLM-5.3-Flash"},` +
			`{"id":"glm-5.3-flashx","display_name":"GLM-5.3-FlashX"},` +
			`{"id":"glm-4.7","display_name":"GLM-4.7"}` +
			`]}`))
	}))
	defer zaiModels.Close()

	origDocs := discoverZAIEffortDocs
	discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) {
		return []catwalk.Model{
			{ID: "glm-5.3", CanReason: true, ReasoningLevels: []string{"low", "high", "max"}},
			{ID: "glm-5.3-flash", CanReason: true, ReasoningLevels: []string{"low", "high", "max"}},
			{ID: "glm-4.7", CanReason: true, ReasoningLevels: []string{"off", "on"}},
		}, nil
	}
	t.Cleanup(func() { discoverZAIEffortDocs = origDocs })

	// Local CLI detection depends on the machine's PATH; pin it to none so
	// the golden only covers catalog-provided metadata.
	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	seedModelCatalog(t, "openai-codex", discover.CodexBaseURL, "chatgpt-account", []catwalk.Model{
		{
			ID:                     "gpt-6-luna",
			Name:                   "GPT-6 Luna",
			ContextWindow:          272000,
			DefaultMaxTokens:       128000,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "high"},
			DefaultReasoningEffort: "high",
		},
		{
			ID:                     "gpt-6.1-sol",
			Name:                   "GPT-6.1 Sol",
			ContextWindow:          272000,
			DefaultMaxTokens:       128000,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "high"},
			DefaultReasoningEffort: "high",
		},
		{ID: "codex-model", Name: "Codex Model", ContextWindow: 272000, DefaultMaxTokens: 128000},
	})

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("zai", ProviderConfig{
		ID:      "zai",
		BaseURL: zaiModels.URL,
		APIKey:  "zai-test-key",
		// A dump as `rush providers update zai` writes it: the raw
		// server answer carries no context windows, so every value is 0.
		Models: []catwalk.Model{
			{ID: "glm-5.3", Name: "GLM-5.3"},
			{ID: "glm-4.7", Name: "GLM-4.7"},
		},
	})
	cfg.Providers.Set("openai-codex", ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
		// A user-entered value below the documented floor: the user wins.
		Models: []catwalk.Model{
			{ID: "gpt-6-luna", Name: "GPT-6 Luna", ContextWindow: 100000, DefaultMaxTokens: 4096},
		},
	})

	envMap := env.NewFromMap(map[string]string{"ZAI_API_KEY": "zai-test-key"})
	resolver := NewShellVariableResolver(envMap)
	knownProviders, err := Providers(cfg)
	require.NoError(t, err)
	// Keep the golden's original provider surface while sourcing every
	// provider definition from the embedded catwalk fixture.
	knownProviders = filterGoldenProviders(knownProviders)
	for i := range knownProviders {
		if knownProviders[i].ID == catwalk.InferenceProviderZAI {
			knownProviders[i].Models = []catwalk.Model{{ID: "glm-5", Name: "GLM-5", DefaultMaxTokens: 1000}}
		}
	}
	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, knownProviders,
	))

	got := renderModelMetadataGolden(cfg)
	path := filepath.Join("testdata", "model_metadata_golden.txt")
	if *updateModelMetadataGolden {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	expected, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(expected), got,
		"model metadata changed; if (and only if) the change is the intended glm-5.3 family update, regenerate with -update-model-metadata-golden")
}

func filterGoldenProviders(providers []catwalk.Provider) []catwalk.Provider {
	wanted := map[catwalk.InferenceProvider]bool{"openai-codex": true, catwalk.InferenceProviderZAI: true}
	filtered := make([]catwalk.Provider, 0, len(wanted))
	for _, provider := range providers {
		if wanted[provider.ID] {
			filtered = append(filtered, provider)
		}
	}
	return filtered
}

// seedModelCatalog writes a fresh catalog cache entry so cachedProviderModels
// serves the given models without touching the network.
func seedModelCatalog(t *testing.T, provider, endpoint, identity string, models []catwalk.Model) {
	t.Helper()
	entry := cachedModelCatalog{
		Fingerprint: modelCatalogFingerprint(provider, endpoint, identity),
		FetchedAt:   time.Now(),
		Models:      models,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	path := modelCatalogPath(provider)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, atomicWriteFile(path, data, 0o600))
}

// renderModelMetadataGolden renders the sorted (provider, id, context window,
// max tokens) rows of every enabled provider followed by the sorted
// LiveEfforts entries -- the observable metadata surface of loadProviders.
func renderModelMetadataGolden(cfg *Config) string {
	type effortRow struct{ text string }
	var rows []string
	var effortRows []effortRow
	for _, p := range cfg.EnabledProviders() {
		for _, m := range p.Models {
			rows = append(rows, fmt.Sprintf("%s	%s	%d	%d", p.ID, m.ID, m.ContextWindow, m.DefaultMaxTokens))
		}
		for id, info := range p.LiveEfforts {
			effortRows = append(effortRows, effortRow{text: fmt.Sprintf("%s	%s	%s	%s",
				p.ID, id, info.Default, strings.Join(info.Levels, ","))})
		}
	}
	sort.Strings(rows)
	sort.Slice(effortRows, func(i, j int) bool { return effortRows[i].text < effortRows[j].text })
	out := make([]string, 0, len(rows)+2+len(effortRows))
	out = append(out, rows...)
	out = append(out, "", "LIVE EFFORTS")
	for _, row := range effortRows {
		out = append(out, row.text)
	}
	return strings.Join(out, "\n") + "\n"
}
