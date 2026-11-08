package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeDocs loads the given JSON documents as config layers (doc 0 =
// lowest priority) the same way loadFromBytes folds them in production.
func mergeDocs(t *testing.T, docs ...string) (*Config, error) {
	t.Helper()
	raws := make([][]byte, len(docs))
	for i, doc := range docs {
		raws[i] = []byte(doc)
	}
	return loadFromBytes(raws)
}

// policyDoc is the global weekly policy used by the overlay tests.
const policyDoc = `{"providers":{"prov":{"peak_hours":{
	"timezone":"UTC",
	"windows":[{"days":"mon-fri","start":"09:00","end":"18:00"}],
	"message":"g"}}}}`

// TestMergeLegacyPartialDeepMergePreserved pins that a legacy fragment
// field-merges into earlier legacy state (later wins per key).
func TestMergeLegacyPartialDeepMergePreserved(t *testing.T) {
	t.Parallel()

	cfg, err := mergeDocs(t,
		`{"providers":{"prov":{"peak_hours":{"start":"09:00","end":"18:00"}}}}`,
		`{"providers":{"prov":{"peak_hours":{"start":"10:00"}}}}`,
	)
	require.NoError(t, err)
	pc, ok := cfg.Providers.Get("prov")
	require.True(t, ok, "provider must survive the merge")
	require.NotNil(t, pc.PeakHours, "peak_hours must survive the merge")
	require.NotNil(t, pc.PeakHours.Legacy, "merged legacy state stays legacy")
	require.Nil(t, pc.PeakHours.Policy)
	assert.Equal(t, "10:00", pc.PeakHours.Legacy.Start, "later fragment wins per key")
	assert.Equal(t, "18:00", pc.PeakHours.Legacy.End, "earlier end is preserved by the deep merge")
}

// TestMergeLegacyFragmentAlone pins today's behaviour: a lone incomplete
// legacy fragment loads without error, keeping whatever fields it has.
func TestMergeLegacyFragmentAlone(t *testing.T) {
	t.Parallel()

	cfg, err := mergeDocs(t,
		`{"providers":{"prov":{"peak_hours":{"start":"09:00"}}}}`,
	)
	require.NoError(t, err, "a lone start-only legacy fragment must not error")
	pc, ok := cfg.Providers.Get("prov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Legacy)
	assert.Equal(t, "09:00", pc.PeakHours.Legacy.Start)
	assert.Empty(t, pc.PeakHours.Legacy.End, "no end was configured, none is invented")
}

// TestMergePolicyReplacedEntirely pins that the weekly policy form
// never field-merges between scopes: a later policy replaces the
// earlier one wholesale, including unset fields.
func TestMergePolicyReplacedEntirely(t *testing.T) {
	t.Parallel()

	cfg, err := mergeDocs(t, policyDoc,
		`{"providers":{"prov":{"peak_hours":{
			"timezone":"UTC+08:00",
			"windows":[{"days":"sat","start":"10:00","end":"12:00"}]}}}}`,
	)
	require.NoError(t, err)
	pc, ok := cfg.Providers.Get("prov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Policy)
	require.Nil(t, pc.PeakHours.Legacy)
	p := pc.PeakHours.Policy
	require.Len(t, p.Windows, 1, "the local policy must replace, not merge, the global windows")
	assert.Equal(t, "UTC+08:00", p.Timezone)
	assert.Equal(t, PeakHoursWindowSpec{Days: "sat", Start: "10:00", End: "12:00"}, p.Windows[0])
	assert.Empty(t, p.Message, "the global message must not leak into the replaced policy")
}

// TestMergeExplicitSuppression covers the explicit-off overlays:
// an empty windows list, and peak_hours null over either form.
func TestMergeExplicitSuppression(t *testing.T) {
	t.Parallel()

	t.Run("windows:[] over policy is explicit off", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t, policyDoc, `{"providers":{"prov":{"peak_hours":{"windows":[]}}}}`)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.NotNil(t, pc.PeakHours)
		require.NotNil(t, pc.PeakHours.Policy, "windows:[] decodes as an empty policy")
		require.Empty(t, pc.PeakHours.Policy.Windows, "explicit empty windows = off")
		assert.Empty(t, pc.PeakHours.Policy.Message)
	})

	t.Run("null over policy turns the feature off", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t, policyDoc, `{"providers":{"prov":{"peak_hours":null}}}`)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.Nil(t, pc.PeakHours, "explicit null leaves the pointer nil (off)")
	})

	t.Run("null over legacy turns the feature off", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"providers":{"prov":{"peak_hours":{"start":"09:00","end":"18:00"}}}}`,
			`{"providers":{"prov":{"peak_hours":null}}}`,
		)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.Nil(t, pc.PeakHours, "explicit null clears the pointer too")
	})

	t.Run("empty windows with a message over policy is explicit off", func(t *testing.T) {
		t.Parallel()
		// The windows key always marshals, so a locally stored
		// {windows:[], message} re-decodes as policy form and replaces
		// the global policy wholesale instead of merging its message
		// onto the global windows.
		cfg, err := mergeDocs(t, policyDoc, `{"providers":{"prov":{"peak_hours":{"windows":[],"message":"m"}}}}`)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.NotNil(t, pc.PeakHours)
		require.NotNil(t, pc.PeakHours.Policy)
		require.Nil(t, pc.PeakHours.Legacy)
		require.Empty(t, pc.PeakHours.Policy.Windows, "the global windows must be suppressed")
		assert.Equal(t, "m", pc.PeakHours.Policy.Message)
	})
}

// TestMergeLegacyCompleteReplacesPolicy pins that a complete legacy
// window (both start and end) over a policy replaces it wholesale.
func TestMergeLegacyCompleteReplacesPolicy(t *testing.T) {
	t.Parallel()

	cfg, err := mergeDocs(t, policyDoc,
		`{"providers":{"prov":{"peak_hours":{"start":"08:00","end":"09:00"}}}}`,
	)
	require.NoError(t, err)
	pc, ok := cfg.Providers.Get("prov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Legacy, "a complete legacy window replaces the policy")
	assert.Equal(t, "08:00", pc.PeakHours.Legacy.Start)
	assert.Equal(t, "09:00", pc.PeakHours.Legacy.End)
	require.Nil(t, pc.PeakHours.Policy, "the policy form is gone after the replacement")
}

// TestMergeMessageOnlyOverlay preserves legacy merging without composing formats.
func TestMergeMessageOnlyOverlay(t *testing.T) {
	t.Parallel()

	overlay := `{"providers":{"prov":{"peak_hours":{"message":"new"}}}}`
	_, err := mergeDocs(t, policyDoc, overlay)
	require.ErrorContains(t, err, "incomplete legacy peak_hours fragment")
	require.ErrorContains(t, err, "prov")

	cfg, err := mergeDocs(t,
		`{"providers":{"prov":{"peak_hours":{"start":"09:00","end":"18:00","message":"old"}}}}`, overlay)
	require.NoError(t, err)
	pc, ok := cfg.Providers.Get("prov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.Equal(t, &PeakHoursWindow{Start: "09:00", End: "18:00", Message: "new"}, pc.PeakHours.Legacy)
	require.Nil(t, pc.PeakHours.Policy)
}

func TestMergePeakHoursRejectsMalformedSourceBeforeOverlay(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"message":"new","unknown":true}`,
		`{"timezone":"UTC"}`,
		`{"windows":[],"unknown":true}`,
		`{"windows":[{"start":"09:00","end":"10:00","unknown":true}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()
			malformed := `{"providers":{"prov":{"peak_hours":` + payload + `}}}`
			for _, docs := range [][]string{
				{malformed},
				{policyDoc, malformed},
				{malformed, policyDoc},
			} {
				_, err := mergeDocs(t, docs...)
				require.ErrorContains(t, err, "provider prov", "documents: %v", docs)
			}
		})
	}
}

// TestMergeIncompleteFragmentOverPolicy pins the asymmetric behaviour:
// fragment-first is applied before the policy (policy wins silently),
// but a fragment arriving AFTER a policy is a hard error naming the
// provider, because guessing would silently change semantics.
func TestMergeIncompleteFragmentOverPolicy(t *testing.T) {
	t.Parallel()

	t.Run("fragment first, policy second: policy wins silently", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"providers":{"prov":{"peak_hours":{"start":"08:00"}}}}`,
			policyDoc,
		)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.NotNil(t, pc.PeakHours)
		require.NotNil(t, pc.PeakHours.Policy, "the later policy replaces the fragment")
		require.Len(t, pc.PeakHours.Policy.Windows, 1)
	})

	t.Run("policy first, fragment second: hard error", func(t *testing.T) {
		t.Parallel()
		_, err := mergeDocs(t,
			policyDoc,
			`{"providers":{"prov":{"peak_hours":{"start":"08:00"}}}}`,
		)
		require.Error(t, err, "an incomplete legacy fragment cannot be applied to a policy")
		assert.Contains(t, err.Error(), "prov", "the error must name the provider id")
	})
}

// TestMergeEmptyObjectOverlays pins the "{}" overlay behaviour: over a
// policy it is an explicit off (an empty shell survives the merge);
// over legacy state it is a no-op field-merge that changes nothing.
func TestMergeEmptyObjectOverlays(t *testing.T) {
	t.Parallel()

	t.Run("empty object over policy is off", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t, policyDoc, `{"providers":{"prov":{"peak_hours":{}}}}`)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.NotNil(t, pc.PeakHours)
		require.NotNil(t, pc.PeakHours.Legacy,
			"the empty shell decodes through the legacy leg (no policy key present)")
		require.Nil(t, pc.PeakHours.Policy)
		assert.True(t, pc.PeakHours.IsOff(), "an empty shell means off")
	})

	t.Run("empty object over legacy is a no-op", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"providers":{"prov":{"peak_hours":{"start":"09:00","end":"18:00"}}}}`,
			`{"providers":{"prov":{"peak_hours":{}}}}`,
		)
		require.NoError(t, err)
		pc, ok := cfg.Providers.Get("prov")
		require.True(t, ok)
		require.NotNil(t, pc.PeakHours)
		require.NotNil(t, pc.PeakHours.Legacy)
		assert.Equal(t, "09:00", pc.PeakHours.Legacy.Start, "legacy state is unchanged")
		assert.Equal(t, "18:00", pc.PeakHours.Legacy.End)
	})
}

// TestMergeNonObjectPeakHours pins the type error for a non-object
// peak_hours value.
func TestMergeNonObjectPeakHours(t *testing.T) {
	t.Parallel()

	_, err := mergeDocs(t, `{"providers":{"prov":{"peak_hours":"09:00-18:00"}}}`)
	require.Error(t, err, "a string peak_hours must be rejected at merge time")
	assert.Contains(t, err.Error(), "must be an object or null")
	assert.Contains(t, err.Error(), "prov", "the error must name the provider id")
}

// TestMergePeakHoursOnlyProviderSurvives pins that a provider entry
// carrying ONLY peak_hours survives the merge and keeps its policy.
func TestMergePeakHoursOnlyProviderSurvives(t *testing.T) {
	t.Parallel()

	cfg, err := mergeDocs(t, `{"providers":{"solo":{"peak_hours":{
		"windows":[{"days":"mon","start":"09:00","end":"10:00"}]}}}}`)
	require.NoError(t, err)
	pc, ok := cfg.Providers.Get("solo")
	require.True(t, ok, "a peak_hours-only provider entry must survive")
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Policy)
	require.Len(t, pc.PeakHours.Policy.Windows, 1)
	assert.Equal(t, "09:00", pc.PeakHours.Policy.Windows[0].Start)
}

// TestProviderPeakHoursJSONRoundTrip locks the wire/file JSON shapes of
// both forms plus UnmarshalJSON's form detection and rejection paths.
func TestProviderPeakHoursJSONRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("legacy marshals with message omitted when empty", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "09:00", End: "18:00"}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"start":"09:00","end":"18:00"}`, string(data))
	})

	t.Run("legacy marshals with message", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(ProviderPeakHours{Legacy: &PeakHoursWindow{
			Start: "09:00", End: "18:00", Message: "m",
		}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"start":"09:00","end":"18:00","message":"m"}`, string(data))
	})

	t.Run("policy marshals timezone/windows/message/paused_until", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(ProviderPeakHours{Policy: &PeakHoursPolicy{
			Timezone: "UTC", Message: "m", PausedUntil: "2026-11-09T10:00:00Z",
			Windows: []PeakHoursWindowSpec{{Days: "mon", Start: "09:00", End: "10:00"}},
		}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"timezone":"UTC","message":"m",
			"paused_until":"2026-11-09T10:00:00Z",
			"windows":[{"days":"mon","start":"09:00","end":"10:00"}]}`, string(data))
	})

	t.Run("null decodes to both legs nil", func(t *testing.T) {
		t.Parallel()
		var p ProviderPeakHours
		require.NoError(t, json.Unmarshal([]byte("null"), &p))
		require.Nil(t, p.Legacy)
		require.Nil(t, p.Policy)
	})

	t.Run("mixing legacy and policy is rejected", func(t *testing.T) {
		t.Parallel()
		var p ProviderPeakHours
		err := json.Unmarshal([]byte(`{"windows":[{"days":"mon","start":"09:00","end":"10:00"}],
			"start":"09:00"}`), &p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mixes")
	})

	t.Run("message-only stays legacy", func(t *testing.T) {
		t.Parallel()
		var p ProviderPeakHours
		require.NoError(t, json.Unmarshal([]byte(`{"message":"m"}`), &p))
		require.NotNil(t, p.Legacy, "old semantics: a message-only document is a legacy empty window")
		require.Nil(t, p.Policy)
		assert.Empty(t, p.Legacy.Start)
		assert.Empty(t, p.Legacy.End)
		assert.Equal(t, "m", p.Legacy.Message)
	})

	t.Run("message-only policy marshals with explicit empty windows", func(t *testing.T) {
		t.Parallel()
		// The windows key has no omitempty: even a nil slice must reach the
		// file so the stored object re-decodes as POLICY form (explicit
		// off), not as a message-only legacy overlay that would re-inherit
		// a global schedule.
		data, err := json.Marshal(ProviderPeakHours{Policy: &PeakHoursPolicy{Message: "m"}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"windows":null,"message":"m"}`, string(data))

		var back ProviderPeakHours
		require.NoError(t, json.Unmarshal(data, &back))
		require.NotNil(t, back.Policy, "the explicit windows key selects policy form")
		require.Nil(t, back.Legacy)
		assert.True(t, back.IsOff(), "empty windows still mean off")
	})
}

// TestNormalizeForWrite pins which off-shaped values collapse to nil.
func TestNormalizeForWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   *ProviderPeakHours
		wantNil bool
	}{
		{"nil stays nil", nil, true},
		{"empty legacy collapses", &ProviderPeakHours{Legacy: &PeakHoursWindow{}}, true},
		{"equal legacy start/end collapses", &ProviderPeakHours{Legacy: &PeakHoursWindow{
			Start: "09:00", End: "09:00",
		}}, true},
		{"real legacy stays", &ProviderPeakHours{Legacy: &PeakHoursWindow{
			Start: "09:00", End: "18:00",
		}}, false},
		{"zero policy collapses", &ProviderPeakHours{Policy: &PeakHoursPolicy{}}, true},
		{"paused-only policy stays", &ProviderPeakHours{Policy: &PeakHoursPolicy{
			PausedUntil: "2026-11-09T10:00:00Z",
		}}, false},
		{"windows-only policy stays", &ProviderPeakHours{Policy: &PeakHoursPolicy{
			Windows: []PeakHoursWindowSpec{{Days: "mon", Start: "09:00", End: "10:00"}},
		}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.value.NormalizeForWrite()
			if tc.wantNil {
				require.Nil(t, got, "%s must normalize to nil", tc.name)
				return
			}
			require.NotNil(t, got, "%s must survive normalization", tc.name)
			require.Same(t, tc.value, got, "a non-off value normalizes to itself")
		})
	}
}

// TestConfigStorePeakHoursScopes is the ConfigStore integration test:
// a policy written in one scope is visible via the merged Config and
// PeakHoursEntryForScope, per-scope reads stay isolated, and a second
// scope's legacy entry coexists with the first scope's policy.
func TestConfigStorePeakHoursScopes(t *testing.T) {
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	pinKtavAutoMigrateOffForLegacyTest(t)
	_, dataDir := isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	// Reload discovers STANDARD slots only: the env global slot
	// (RUSH_GLOBAL_DATA/rush.json) and the workspace walk root/.rush/rush.json.
	// Seed those exact files so writes survive the auto-reload after each
	// SetConfigField, with a full provider skeleton so configureProviders
	// accepts the peak_hours-only edit.
	globalPath := filepath.Join(dataDir, "rush.json")
	wsDir := filepath.Join(root, ".rush")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	wsPath := filepath.Join(wsDir, "rush.json")
	skeleton := `{"providers":{"phprov":{"name":"PH Prov","type":"openai","api_key":"sk-1234567890abcdef","base_url":"https://ph.example.test/v1","models":[{"id":"ph-model"}]}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(skeleton), 0o600))
	require.NoError(t, os.WriteFile(wsPath, []byte(skeleton), 0o600))
	s := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: globalPath,
		workspacePath:  wsPath,
	})
	s.workingDir = root

	policy := &PeakHoursPolicy{
		Timezone: "UTC",
		Windows:  []PeakHoursWindowSpec{{Days: "mon-fri", Start: "09:00", End: "18:00"}},
	}
	require.NoError(t, s.SetConfigField(ScopeGlobal, "providers.phprov.peak_hours",
		ProviderPeakHours{Policy: policy}))

	// The merged effective view shows the policy.
	cfg := s.Config()
	require.NotNil(t, cfg)
	pc, ok := cfg.Providers.Get("phprov")
	require.True(t, ok, "provider must be visible after the write")
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Policy)
	require.Equal(t, policy, pc.PeakHours.Policy)

	// The single-scope read matches, and the other scope has nothing.
	entry, err := s.PeakHoursEntryForScope(ScopeGlobal, "phprov")
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.NotNil(t, entry.Policy)
	require.Equal(t, policy, entry.Policy)
	other, err := s.PeakHoursEntryForScope(ScopeWorkspace, "phprov")
	require.NoError(t, err)
	require.Nil(t, other, "the workspace scope must not see the global-only entry")

	// A legacy entry in the workspace scope coexists per-scope.
	legacy := &PeakHoursWindow{Start: "22:00", End: "06:00"}
	require.NoError(t, s.SetConfigField(ScopeWorkspace, "providers.phprov.peak_hours",
		ProviderPeakHours{Legacy: legacy}))

	cfg = s.Config()
	pc, ok = cfg.Providers.Get("phprov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Legacy, "the workspace legacy leg wins in the merged view")
	assert.Equal(t, "22:00", pc.PeakHours.Legacy.Start)

	wsEntry, err := s.PeakHoursEntryForScope(ScopeWorkspace, "phprov")
	require.NoError(t, err)
	require.NotNil(t, wsEntry)
	require.NotNil(t, wsEntry.Legacy)
	assert.Equal(t, "06:00", wsEntry.Legacy.End)
	gEntry, err := s.PeakHoursEntryForScope(ScopeGlobal, "phprov")
	require.NoError(t, err)
	require.NotNil(t, gEntry)
	require.NotNil(t, gEntry.Policy, "the global scope keeps its own policy form")

	// Explicit local off with only a message: the empty windows list must
	// survive the write (the windows key always marshals), so the entry
	// stays policy form and suppresses the inherited global policy after
	// the reload instead of degrading to a message-only legacy overlay.
	require.NoError(t, s.SetConfigField(ScopeWorkspace, "providers.phprov.peak_hours",
		ProviderPeakHours{Policy: &PeakHoursPolicy{Message: "locally off"}}))
	cfg = s.Config()
	pc, ok = cfg.Providers.Get("phprov")
	require.True(t, ok)
	require.NotNil(t, pc.PeakHours)
	require.NotNil(t, pc.PeakHours.Policy, "an explicit empty-windows policy stays policy form")
	require.Empty(t, pc.PeakHours.Policy.Windows, "the global windows must be suppressed")
	assert.Equal(t, "locally off", pc.PeakHours.Policy.Message)
}

// TestSettingsLockBlocksPeakHoursFieldWrites mirrors the settings-lock
// suite's setup: while settings are locked, BOTH SetConfigField and
// RemoveConfigField reject a peak_hours path with ErrSettingsLocked —
// the pause/resume paths inherit this gate, so no hidden unlock exists.
func TestSettingsLockBlocksPeakHoursFieldWrites(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "providers.phprov.peak_hours",
		ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "09:00", End: "18:00"}}))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	locked, err := s.SettingsLocked()
	require.NoError(t, err)
	require.True(t, locked)

	before := fileBytes(t, p)
	err = s.SetConfigField(ScopeGlobal, "providers.phprov.peak_hours.paused_until",
		"2026-11-09T10:00:00Z")
	require.ErrorIs(t, err, ErrSettingsLocked, "a paused_until write is gated by the settings lock")
	require.Equal(t, before, fileBytes(t, p), "locked writes must not touch the file")

	require.ErrorIs(t, s.RemoveConfigField(ScopeGlobal, "providers.phprov.peak_hours"),
		ErrSettingsLocked, "removing the whole field is gated too")
	require.Equal(t, before, fileBytes(t, p))
}
