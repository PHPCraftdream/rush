package cahgen

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Schema fixtures are independent of the frozen and live catalogs.
func schemaManifest(codexFields string) []byte {
	return []byte(fmt.Sprintf(`{"version":"0.16.1","claude":[{"name":"ol","model":"claude-opus-99","effort":"low","display":"Opus (1M) – low"}],"codex":[{"name":"ls","model":"gpt-99-sol","effort":"low","display":"GPT 99 Sol - Low"%s}],"acceptedEfforts":{"gpt-99-sol":["low","high","ultra"]}}`, codexFields))
}

// Revert-check: Registry efforts without aliases remain accepted and documented.
func TestUnknownCodexRegistryEfforts(t *testing.T) {
	m, err := Decode(schemaManifest(""))
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		o    map[string]Override
	}{
		{"no override", nil},
		{"no caps", map[string]Override{"gpt-99-sol": {Context: 300000}}},
		{"matching caps", map[string]Override{"gpt-99-sol": {Measured: true, EffortCaps: []string{"low", "high", "ultra"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Transform(m, tc.o)
			require.NoError(t, err)
			var spec Spec
			for _, s := range out.Models {
				if s.Model == "gpt-99-sol" {
					spec = s
				}
			}
			require.Equal(t, []string{"low", "high", "ultra"}, spec.Efforts)
			require.Contains(t, string(out.Specs), `[]string{"low", "high", "ultra"}`)
			require.Equal(t, "low", out.Names["ls"].Effort)
			require.Len(t, out.Names, 2)
			for _, effort := range []string{"high", "ultra"} {
				note := "local-cli/cli-codex-gpt-99-sol@" + effort + " accepted (no cah code)"
				require.Contains(t, out.Notes, note)
				require.Contains(t, string(out.Cmd), note)
			}
		})
	}
	_, err = Transform(m, map[string]Override{"gpt-99-sol": {Measured: true, EffortCaps: []string{"low", "high"}}})
	require.ErrorContains(t, err, "acceptedEfforts measured cap mismatch gpt-99-sol")
	m.AcceptedEfforts = nil
	out, err := Transform(m, nil)
	require.NoError(t, err)
	for _, s := range out.Models {
		if s.Model == "gpt-99-sol" {
			require.Equal(t, []string{"low"}, s.Efforts)
		}
	}
}

// Revert-check: Max-only windows are valid; compare only when both are present.
func TestFixtureSchemaMaxContextWindow(t *testing.T) {
	for _, tc := range []struct {
		name, fields, wantError string
	}{
		{"max only", `,"maxContextWindow":1000000`, ""},
		{"equal", `,"contextWindow":300000,"maxContextWindow":300000`, ""},
		{"lower", `,"contextWindow":300000,"maxContextWindow":200000`, "maxContextWindow must be >= contextWindow"},
		{"zero", `,"maxContextWindow":0`, "maxContextWindow must be positive integer"},
		{"negative", `,"maxContextWindow":-1`, "maxContextWindow must be positive integer"},
		{"fraction", `,"maxContextWindow":1.5`, "maxContextWindow must be positive integer"},
		{"null", `,"maxContextWindow":null`, "maxContextWindow must be positive integer"},
		{"string", `,"maxContextWindow":"1000000"`, "maxContextWindow must be positive integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Decode(schemaManifest(tc.fields))
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			_, err = Transform(m, nil)
			require.NoError(t, err)
			if tc.name == "max only" {
				require.Zero(t, m.Codex[0].ContextWindow)
				require.EqualValues(t, 1000000, m.Codex[0].MaxContextWindow)
			}
		})
	}
	m, err := Decode(schemaManifest(""))
	require.NoError(t, err)
	m.Codex[0].ContextWindow = 300000
	m.Codex[0].MaxContextWindow = 200000
	_, err = Transform(m, nil)
	require.ErrorContains(t, err, "invalid or duplicate codex entry")
}
