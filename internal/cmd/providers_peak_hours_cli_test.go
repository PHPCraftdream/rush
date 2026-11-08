// CLI regression tests for the `rush providers peak-hours` command
// family: pure helper parsing/keys/summary, the legacy-entry migration
// contract of peakTargetPolicy, and command-level behaviour through the
// runProvidersCmdInIsolatedAppFull harness (seeded rush.json, real RunE,
// persisted file read back), covering the set/add/tz/pause/resume/rm/
// clear loss bugs on legacy entries and the list/show --json shapes.
package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePeakWindowArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantSpecs []config.PeakHoursWindowSpec
		wantErr   bool
		errSubstr string
	}{
		{
			name: "two windows with day specs",
			args: []string{"mon-fri", "14:00-18:00", "sat,sun", "10:00-12:00"},
			wantSpecs: []config.PeakHoursWindowSpec{
				{Days: "mon-fri", Start: "14:00", End: "18:00"},
				{Days: "sat-sun", Start: "10:00", End: "12:00"},
			},
		},
		{
			name: "wrapping pair canonicalises to a range",
			args: []string{"sun,mon", "01:00-02:00"},
			wantSpecs: []config.PeakHoursWindowSpec{
				{Days: "sun-mon", Start: "01:00", End: "02:00"},
			},
		},
		{
			name:      "window without days binds every day",
			args:      []string{"14:00-18:00"},
			wantSpecs: []config.PeakHoursWindowSpec{{Days: "", Start: "14:00", End: "18:00"}},
		},
		{
			name:      "24:00 end is accepted",
			args:      []string{"00:00-24:00"},
			wantSpecs: []config.PeakHoursWindowSpec{{Days: "", Start: "00:00", End: "24:00"}},
		},
		{
			name:      "24:00 as start is rejected",
			args:      []string{"24:00-06:00"},
			wantErr:   true,
			errSubstr: "24:00-06:00",
		},
		{
			name:      "zero-length window is rejected",
			args:      []string{"09:00-09:00"},
			wantErr:   true,
			errSubstr: "zero-length",
		},
		{
			name:      "trailing days token is rejected",
			args:      []string{"14:00-18:00", "sat"},
			wantErr:   true,
			errSubstr: "must be followed by an HH:MM-HH:MM window",
		},
		{
			name:      "two days tokens in a row are rejected",
			args:      []string{"mon", "sat", "10:00-12:00"},
			wantErr:   true,
			errSubstr: "two days specs in a row",
		},
		{
			name:      "garbage token names the argument index",
			args:      []string{"14:00-18:00", "garbage"},
			wantErr:   true,
			errSubstr: "argument 2",
		},
		{
			name:      "days token alone is rejected",
			args:      []string{"mon-fri", "14:00-18:00", "sat"},
			wantErr:   true,
			errSubstr: "must be followed by an HH:MM-HH:MM window",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, err := parsePeakWindowArgs(tt.args)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
				assert.Nil(t, specs)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantSpecs, specs)
		})
	}
}

func TestPeakWindowKey(t *testing.T) {
	// The canonical days text is the identity: an explicit list equal to
	// the whole working week must dedupe against the range form.
	a, err := peakWindowKey(config.PeakHoursWindowSpec{Days: "mon-fri", Start: "09:00", End: "18:00"})
	require.NoError(t, err)
	b, err := peakWindowKey(config.PeakHoursWindowSpec{Days: "mon,tue,wed,thu,fri", Start: "09:00", End: "18:00"})
	require.NoError(t, err)
	assert.Equal(t, a, b, "mon-fri and its explicit day list must produce the same dedup key")

	c, err := peakWindowKey(config.PeakHoursWindowSpec{Days: "mon-fri", Start: "10:00", End: "18:00"})
	require.NoError(t, err)
	assert.NotEqual(t, a, c, "a different start clock must produce a different key")
}

func TestPeakHoursSummary(t *testing.T) {
	t.Run("nil entry renders the em dash", func(t *testing.T) {
		assert.Equal(t, "—", peakHoursSummary(nil))
	})

	t.Run("empty entry renders the em dash", func(t *testing.T) {
		assert.Equal(t, "—", peakHoursSummary(&config.ProviderPeakHours{}))
	})

	// peakHoursSummary marks in-peak against time.Now() internally and
	// takes no instant parameter, so the marker cases below construct
	// windows around the current time; no arbitrary now-based windows
	// are assumed.
	now := time.Now()
	t.Run("legacy inside its window shows the marker", func(t *testing.T) {
		start := now.Add(-time.Hour).Format("15:04")
		end := now.Add(time.Hour).Format("15:04")
		ph := &config.ProviderPeakHours{Legacy: &config.PeakHoursWindow{Start: start, End: end}}
		assert.Equal(t, start+"-"+end+" *", peakHoursSummary(ph))
	})

	t.Run("legacy outside its window shows no marker", func(t *testing.T) {
		start := now.Add(2 * time.Hour).Format("15:04")
		end := now.Add(3 * time.Hour).Format("15:04")
		ph := &config.ProviderPeakHours{Legacy: &config.PeakHoursWindow{Start: start, End: end}}
		assert.Equal(t, start+"-"+end, peakHoursSummary(ph),
			"a window starting hours from now must not show the in-peak marker")
	})

	t.Run("policy renders the window count and timezone", func(t *testing.T) {
		ph := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC+02:00",
			Windows: []config.PeakHoursWindowSpec{
				{Days: "mon-fri", Start: "14:00", End: "18:00"},
				{Days: "sat,sun", Start: "10:00", End: "12:00"},
			},
		}}
		got := peakHoursSummary(ph)
		// The in-peak marker is appended when now happens to fall inside
		// a window, so only the prefix is asserted deterministically and
		// the remainder must be either "" or the marker.
		assert.True(t, strings.HasPrefix(got, "2 windows (UTC+02:00)"),
			"expected the compact count+timezone prefix, got %q", got)
		assert.True(t, got == "2 windows (UTC+02:00)" || got == "2 windows (UTC+02:00) *",
			"the only allowed suffix is the in-peak marker, got %q", got)
	})
}

// TestPeakTargetPolicyLegacyPreservation is the P1 regression guard for
// the tz/add/rm/pause loss bug: composing the editable policy from a
// legacy entry must carry the legacy window AND message instead of
// dropping them.
func TestPeakTargetPolicyLegacyPreservation(t *testing.T) {
	t.Run("legacy-only entry converts window and message", func(t *testing.T) {
		entry := &config.ProviderPeakHours{
			Legacy: &config.PeakHoursWindow{Start: "20:00", End: "23:00", Message: "m"},
		}
		got := peakTargetPolicy(entry, "fallback")
		require.NotNil(t, got)
		assert.Equal(t, []config.PeakHoursWindowSpec{{Days: "", Start: "20:00", End: "23:00"}}, got.Windows,
			"the legacy window must become one whole-week window")
		assert.Equal(t, "m", got.Message, "the legacy message must win over the fallback")
	})

	t.Run("nil entry yields a fresh policy with the fallback message", func(t *testing.T) {
		got := peakTargetPolicy(nil, "fallback")
		require.NotNil(t, got)
		assert.Equal(t, &config.PeakHoursPolicy{Message: "fallback"}, got)
	})

	t.Run("policy entry is returned as an equal copy", func(t *testing.T) {
		orig := &config.PeakHoursPolicy{
			Timezone: "UTC+01:00",
			Windows:  []config.PeakHoursWindowSpec{{Days: "sat", Start: "10:00", End: "12:00"}},
			Message:  "already-weekly",
		}
		got := peakTargetPolicy(&config.ProviderPeakHours{Policy: orig}, "fallback")
		require.NotNil(t, got)
		assert.Equal(t, orig, got)
	})
}

// The harness seeds the global rush.json with the raw providers object
// and runs the real RunE; helpers below read the persisted file back as
// ProviderPeakHours so both stored forms decode through their real
// UnmarshalJSON.

func readPeakEntries(t *testing.T, path string) map[string]*config.ProviderPeakHours {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	out := make(map[string]*config.ProviderPeakHours, len(doc.Providers))
	for id, prov := range doc.Providers {
		// peak_hours lives INSIDE the provider object, so decode the
		// sub-field — not the whole provider — as ProviderPeakHours.
		var fields struct {
			PeakHours *config.ProviderPeakHours `json:"peak_hours"`
		}
		require.NoError(t, json.Unmarshal(prov, &fields), "provider %q", id)
		out[id] = fields.PeakHours
	}
	return out
}

const legacySeedJSON = `{
  "providers": {
    "legacy": {
      "name": "Legacy", "type": "openai", "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1", "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "20:00", "end": "23:00"}
    }
  }
}`

const policySeedJSON = `{
  "providers": {
    "polprov": {
      "name": "Policy", "type": "openai", "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1", "models": [{"id": "gpt-4o"}],
      "peak_hours": {"windows": [{"days": "mon-fri", "start": "14:00", "end": "18:00"}, {"start": "20:00", "end": "23:00"}], "message": "weekly-msg"}
    }
  }
}`

func peakPolicyOf(t *testing.T, path, id string) *config.PeakHoursPolicy {
	t.Helper()
	providers := readPeakEntries(t, path)
	p, ok := providers[id]
	require.True(t, ok, "provider %q must still be present", id)
	require.NotNil(t, p, "provider %q entry must be non-nil", id)
	require.NotNil(t, p.Policy, "provider %q must be stored in the weekly policy form", id)
	assert.Nil(t, p.Legacy, "provider %q must no longer use the legacy form", id)
	return p.Policy
}

// TestPeakHoursSet_ReplacesLegacyWindowsWithPolicy: set on a legacy
// entry migrates it to the policy form and REPLACES the windows (the
// command contract), dropping the old 20:00-23:00 window.
func TestPeakHoursSet_ReplacesLegacyWindowsWithPolicy(t *testing.T) {
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursSetCmd, legacySeedJSON, "legacy 09:00-10:00")
	pol := peakPolicyOf(t, dataPath, "legacy")
	require.Len(t, pol.Windows, 1)
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "09:00", End: "10:00"}, pol.Windows[0])
	assert.Equal(t, "", pol.Message)
}

// TestPeakHoursAdd_PreservesLegacyWindow is the direct regression
// assertion for the legacy-loss bug: add must append to the converted
// legacy window, not replace it.
func TestPeakHoursAdd_PreservesLegacyWindow(t *testing.T) {
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursAddCmd, legacySeedJSON, "legacy 09:00-10:00")
	pol := peakPolicyOf(t, dataPath, "legacy")
	require.Len(t, pol.Windows, 2, "the legacy window must be preserved alongside the added one")
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "20:00", End: "23:00"}, pol.Windows[0])
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "09:00", End: "10:00"}, pol.Windows[1])
}

// TestPeakHoursTz_PreservesLegacyWindow: setting a timezone on a legacy
// entry converts the entry to the policy form keeping the window.
func TestPeakHoursTz_PreservesLegacyWindow(t *testing.T) {
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursTzCmd, legacySeedJSON, "legacy UTC+02:00")
	pol := peakPolicyOf(t, dataPath, "legacy")
	assert.Equal(t, "UTC+02:00", pol.Timezone)
	require.Len(t, pol.Windows, 1, "the legacy window must survive the tz migration")
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "20:00", End: "23:00"}, pol.Windows[0])
}

// pauseAndReseed runs the pause command via the harness and returns the
// policy JSON of the persisted result, so a resume invocation in a
// FRESH temp dir can be seeded with exactly what pause wrote (the
// harness gives each invocation its own isolated store).
func pauseAndReseed(t *testing.T, id string) string {
	t.Helper()
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursPauseCmd, legacySeedJSON, id+" 2h")
	pol := peakPolicyOf(t, dataPath, id)
	raw, err := json.Marshal(&config.ProviderPeakHours{Policy: pol})
	require.NoError(t, err)
	return string(raw)
}

func seedWithPeakJSON(providerID, peakJSON string) string {
	return `{
  "providers": {
    "` + providerID + `": {
      "name": "P", "type": "openai", "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1", "models": [{"id": "gpt-4o"}],
      "peak_hours": ` + peakJSON + `
    }
  }
}`
}

// TestPeakHoursPauseAndResume_LegacyEntry covers the pause write (policy
// form, legacy window preserved, bounded paused_until) and both resume
// outcomes: windows kept when the converted entry still holds them, and
// the whole field removed when nothing else remains.
func TestPeakHoursPauseAndResume_LegacyEntry(t *testing.T) {
	before := time.Now()
	peakJSON := pauseAndReseed(t, "legacy")
	pol := readPeakPolicyJSON(t, peakJSON)

	require.Len(t, pol.Windows, 1, "the converted legacy window must be preserved by pause")
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "20:00", End: "23:00"}, pol.Windows[0])
	require.NotEmpty(t, pol.PausedUntil)
	until, err := time.Parse(time.RFC3339, pol.PausedUntil)
	require.NoError(t, err, "paused_until must be a valid RFC3339 timestamp")
	require.True(t, until.After(before), "paused_until must be in the future")
	require.True(t, until.Before(before.Add(7*24*time.Hour)), "paused_until must be within the 7-day cap")

	// Resume with windows present: the field REMAINS holding the windows.
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursResumeCmd,
		seedWithPeakJSON("legacy", peakJSON), "legacy")
	pol = peakPolicyOf(t, dataPath, "legacy")
	require.Len(t, pol.Windows, 1, "the windows must survive resume")
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "", Start: "20:00", End: "23:00"}, pol.Windows[0])
	assert.Equal(t, "", pol.PausedUntil, "resume must clear paused_until")

	// Resume on a paused-only entry (pause ran on a legacy entry whose
	// empty window converts to nothing): the whole field is removed via
	// the NormalizeForWrite off path.
	pol = readPeakPolicyJSON(t, peakJSON)
	bare := &config.PeakHoursPolicy{PausedUntil: pol.PausedUntil}
	bareJSON, err := json.Marshal(&config.ProviderPeakHours{Policy: bare})
	require.NoError(t, err)
	_, barePath, _, runErr := runProvidersCmdInIsolatedAppFull(t, peakHoursResumeCmd,
		seedWithPeakJSON("legacy", string(bareJSON)), "legacy", "")
	require.NoError(t, runErr)
	providers := readPeakEntries(t, barePath)
	entry, ok := providers["legacy"]
	require.True(t, ok, "the provider entry must still be present")
	// Removing peak_hours leaves a nil entry pointer behind: both legs
	// must be gone without dereferencing it.
	if entry != nil {
		assert.Nil(t, entry.Policy, "a paused-only entry must be removed entirely by resume")
		assert.Nil(t, entry.Legacy, "no leftover legacy leg may remain")
	}
	raw, err := os.ReadFile(barePath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "peak_hours", "the empty peak_hours field must be gone from the file")
}

func readPeakPolicyJSON(t *testing.T, peakJSON string) *config.PeakHoursPolicy {
	t.Helper()
	var p config.ProviderPeakHours
	require.NoError(t, json.Unmarshal([]byte(peakJSON), &p))
	require.NotNil(t, p.Policy)
	return p.Policy
}

// TestPeakHoursRm_RemovesNumberedPolicyWindow: rm indexes the stored
// policy windows and preserves the rest.
func TestPeakHoursRm_RemovesNumberedPolicyWindow(t *testing.T) {
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursRmCmd, policySeedJSON, "polprov 2")
	pol := peakPolicyOf(t, dataPath, "polprov")
	require.Len(t, pol.Windows, 1, "exactly the targeted window must be removed")
	assert.Equal(t, config.PeakHoursWindowSpec{Days: "mon-fri", Start: "14:00", End: "18:00"}, pol.Windows[0])
	assert.Equal(t, "weekly-msg", pol.Message, "the message must survive rm")
}

func TestPeakHoursRm_OutOfRangeIsAnError(t *testing.T) {
	_, dataPath, _, runErr := runProvidersCmdInIsolatedAppFull(t, peakHoursRmCmd, policySeedJSON, "polprov 3", "")
	require.Error(t, runErr, "an out-of-range window number must fail")
	providers := readPeakEntries(t, dataPath)
	require.NotNil(t, providers["polprov"])
	require.NotNil(t, providers["polprov"].Policy)
	assert.Len(t, providers["polprov"].Policy.Windows, 2, "a failed rm must persist nothing")
}

// TestPeakHoursRm_MigratesLegacyEntry: rm's numbers index the POLICY
// windows after peakTargetPolicy converts a legacy entry, so rm on a
// legacy entry migrates it and removes the targeted window; with zero
// windows left, NormalizeForWrite drops the whole field.
func TestPeakHoursRm_MigratesLegacyEntry(t *testing.T) {
	_, dataPath, _, runErr := runProvidersCmdInIsolatedAppFull(t, peakHoursRmCmd, legacySeedJSON, "legacy 1", "")
	require.NoError(t, runErr, "rm on a legacy-only entry must migrate it and remove window 1")
	providers := readPeakEntries(t, dataPath)
	entry := providers["legacy"]
	if entry != nil {
		assert.Nil(t, entry.Legacy, "no legacy leg may remain after the migration")
		assert.Nil(t, entry.Policy, "a zero-window result must normalize to field removal")
	}
	raw, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "peak_hours", "the emptied peak_hours field must be gone from the file")
}

// TestPeakHoursClear_RemovesTheField: clear drops providers.<id>.peak_
// hours from the scope file entirely.
func TestPeakHoursClear_RemovesTheField(t *testing.T) {
	_, dataPath := runProvidersCmdInIsolatedApp(t, peakHoursClearCmd, policySeedJSON, "polprov")
	providers := readPeakEntries(t, dataPath)
	cleared, ok := providers["polprov"]
	require.True(t, ok, "the provider entry must still be present")
	if cleared != nil {
		assert.Nil(t, cleared.Policy, "clear must remove the policy leg")
		assert.Nil(t, cleared.Legacy, "clear must remove the legacy leg too")
	}
	raw, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "peak_hours", "the field must be gone from the persisted file")
}

// TestPeakHoursShowJSON_Shape parses the captured stdout of show --json
// and asserts the emitted keys for both stored forms; the harness
// captures real stdout, so the real encoder output is checked.
func TestPeakHoursShowJSON_Shape(t *testing.T) {
	t.Run("legacy form", func(t *testing.T) {
		out, _ := runProvidersCmdInIsolatedApp(t, peakHoursShowCmd, legacySeedJSON, "legacy --json")
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &doc), "stdout: %q", out)
		assert.Equal(t, "legacy", doc["id"])
		// Without an explicit --global/--local, show renders the EFFECTIVE
		// merged view and labels it "effective" (see peakHoursShowCmd).
		assert.Equal(t, "effective", doc["scope"])
		windows, ok := doc["windows"].([]any)
		require.True(t, ok, "windows must be an array")
		require.Len(t, windows, 1)
		w := windows[0].(map[string]any)
		assert.Equal(t, "20:00", w["start"])
		assert.Equal(t, "23:00", w["end"])
		status, ok := doc["status"].(map[string]any)
		require.True(t, ok, "status must always be present")
		for _, key := range []string{"checked_at", "in_peak", "paused", "blocked"} {
			assert.Contains(t, status, key, "status.%s must be emitted", key)
		}
	})

	t.Run("policy form", func(t *testing.T) {
		out, _ := runProvidersCmdInIsolatedApp(t, peakHoursShowCmd, policySeedJSON, "polprov --json")
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &doc), "stdout: %q", out)
		assert.Equal(t, "polprov", doc["id"])
		assert.Equal(t, "weekly-msg", doc["message"])
		windows, ok := doc["windows"].([]any)
		require.True(t, ok)
		require.Len(t, windows, 2)
		first := windows[0].(map[string]any)
		assert.Equal(t, "mon-fri", first["days"])
		assert.Equal(t, "14:00", first["start"])
		status, ok := doc["status"].(map[string]any)
		require.True(t, ok)
		for _, key := range []string{"checked_at", "in_peak", "paused", "blocked"} {
			assert.Contains(t, status, key)
		}
	})
}

// TestPeakHoursListJSON_Shape checks the NDJSON list item keys.
func TestPeakHoursListJSON_Shape(t *testing.T) {	out, _ := runProvidersCmdInIsolatedApp(t, peakHoursListCmd, policySeedJSON, "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1, "one provider must yield exactly one NDJSON line")
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &doc))
	assert.Equal(t, "polprov", doc["id"])
	assert.InDelta(t, 2, doc["windows"], 0, "windows must be a count")
	_, ok := doc["status"].(map[string]any)
	require.True(t, ok, "status must always be present in list --json")
}
