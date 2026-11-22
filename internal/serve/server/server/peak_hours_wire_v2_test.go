package server

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPeakHoursFromWirePolicyForm pins the weekly-policy decode path of
// peakHoursFromWire, including validation failures and form mixing.
func TestPeakHoursFromWirePolicyForm(t *testing.T) {
	t.Parallel()

	t.Run("full policy form", func(t *testing.T) {
		t.Parallel()
		got, err := peakHoursFromWire(&PeakHoursWirePayload{
			Timezone: "UTC",
			Windows:  []PeakHoursWindowWire{{Days: "mon-fri", Start: "09:00", End: "18:00"}},
			Message:  "m",
		})
		require.NoError(t, err)
		require.Equal(t, &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC",
			Windows: []config.PeakHoursWindowSpec{
				{Days: "mon-fri", Start: "09:00", End: "18:00"},
			},
			Message: "m",
		}}, got)
	})

	t.Run("whole-day every-day window is valid", func(t *testing.T) {
		t.Parallel()
		got, err := peakHoursFromWire(&PeakHoursWirePayload{
			Windows: []PeakHoursWindowWire{{Days: "", Start: "00:00", End: "24:00"}},
		})
		require.NoError(t, err)
		require.NotNil(t, got.Policy)
		require.Len(t, got.Policy.Windows, 1)
		assert.Equal(t, "24:00", got.Policy.Windows[0].End)
	})

	t.Run("zero-length window fails validation", func(t *testing.T) {
		t.Parallel()
		_, err := peakHoursFromWire(&PeakHoursWirePayload{
			Windows: []PeakHoursWindowWire{{Days: "mon", Start: "12:00", End: "12:00"}},
		})
		require.Error(t, err, "a zero-length window must fail policy validation")
	})

	t.Run("mixing forms is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := peakHoursFromWire(&PeakHoursWirePayload{
			Timezone: "UTC",
			Windows:  []PeakHoursWindowWire{{Days: "mon", Start: "09:00", End: "10:00"}},
			Start:    "09:00",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mixes")
	})

	t.Run("legacy payload still decodes as legacy", func(t *testing.T) {
		t.Parallel()
		got, err := peakHoursFromWire(&PeakHoursWirePayload{
			Start: "09:00", End: "18:00", Message: "m",
		})
		require.NoError(t, err, "old clients sending start/end must keep working")
		require.Equal(t, &config.ProviderPeakHours{Legacy: &config.PeakHoursWindow{
			Start: "09:00", End: "18:00", Message: "m",
		}}, got)
	})

	t.Run("empty windows list is an explicit zero-window policy", func(t *testing.T) {
		t.Parallel()
		// A PRESENT-but-empty windows key selects the policy branch, so
		// {"windows":[]} is an explicit zero-window policy (off) rather
		// than an empty legacy shell.
		got, err := peakHoursFromWire(&PeakHoursWirePayload{
			Windows: []PeakHoursWindowWire{},
		})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.Policy, "windows:[] alone must decode as a zero-window policy")
		require.Empty(t, got.Policy.Windows, "the policy must carry zero windows")
		require.Nil(t, got.Legacy, "no legacy leg may be synthesized")
		assert.True(t, got.IsOff(), "a zero-window policy means off")
	})

	t.Run("single-window payload still decodes as a policy", func(t *testing.T) {
		t.Parallel()
		got, err := peakHoursFromWire(&PeakHoursWirePayload{
			Windows: []PeakHoursWindowWire{{Days: "mon", Start: "09:00", End: "10:00"}},
		})
		require.NoError(t, err)
		require.NotNil(t, got.Policy, "a windows-only payload must decode as a policy")
		require.Len(t, got.Policy.Windows, 1)
		assert.Equal(t, config.PeakHoursWindowSpec{Days: "mon", Start: "09:00", End: "10:00"}, got.Policy.Windows[0])
		assert.Nil(t, got.Legacy)
	})
}

// TestPeakHoursToWire pins the config-to-wire mapping for both forms
// and the nil passthrough.
func TestPeakHoursToWire(t *testing.T) {
	t.Parallel()

	t.Run("nil is nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, peakHoursToWire(nil))
	})

	t.Run("legacy maps start/end/message", func(t *testing.T) {
		t.Parallel()
		got := peakHoursToWire(&config.ProviderPeakHours{Legacy: &config.PeakHoursWindow{
			Start: "22:00", End: "06:00", Message: "m",
		}})
		require.NotNil(t, got)
		assert.Equal(t, "22:00", got.Start)
		assert.Equal(t, "06:00", got.End)
		assert.Equal(t, "m", got.Message)
		assert.Empty(t, got.Windows)
	})

	t.Run("policy maps timezone/windows/message/pausedUntil", func(t *testing.T) {
		t.Parallel()
		got := peakHoursToWire(&config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC+08:00",
			Windows: []config.PeakHoursWindowSpec{
				{Days: "sat", Start: "10:00", End: "12:00"},
			},
			Message:     "m",
			PausedUntil: "2026-11-09T10:00:00Z",
		}})
		require.NotNil(t, got)
		assert.Equal(t, "UTC+08:00", got.Timezone)
		require.Len(t, got.Windows, 1)
		assert.Equal(t, PeakHoursWindowWire{Days: "sat", Start: "10:00", End: "12:00"}, got.Windows[0])
		assert.Equal(t, "m", got.Message)
		assert.Equal(t, "2026-11-09T10:00:00Z", got.PausedUntil)
		assert.Empty(t, got.Start, "the policy form must not synthesize legacy fields")
	})
}

// TestPeakHoursWireJSONShapes locks the JSON key sets of the status
// payloads: zero optional fields must be omitted by omitempty.
func TestPeakHoursWireJSONShapes(t *testing.T) {
	t.Parallel()

	t.Run("status omits zero optional fields", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(PeakHoursStatusWire{
			ProviderID: "p",
			CheckedAt:  "2026-11-09T12:00:00Z",
		})
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(data, &decoded))
		for _, absent := range []string{
			"paused", "pausedUntil", "reopensAt",
			"nextTransition", "timezone", "message",
		} {
			assert.NotContains(t, decoded, absent, "%s must be omitted when zero", absent)
		}
		assert.Contains(t, decoded, "providerId")
		assert.Contains(t, decoded, "checkedAt")
		assert.Contains(t, decoded, "inPeak")
		assert.Contains(t, decoded, "blocked")
	})

	t.Run("statuses payload shape", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(PeakHoursStatusesWire{
			CheckedAt: "2026-11-09T12:00:00Z",
			Statuses: []PeakHoursStatusWire{{
				ProviderID: "p",
				CheckedAt:  "2026-11-09T12:00:00Z",
			}},
		})
		require.NoError(t, err)
		assert.JSONEq(t, `{"checkedAt":"2026-11-09T12:00:00Z",
			"statuses":[{"providerId":"p","checkedAt":"2026-11-09T12:00:00Z",
			"inPeak":false,"blocked":false}]}`, string(data))
	})
}

// TestPauseResumePayloadDecode covers the pause/resume request payload
// variants across duration/until/scope combinations.
func TestPauseResumePayloadDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		payload  string
		id       string
		duration string
		until    string
		scope    string
	}{
		{"duration only", `{"id":"p","duration":"2h"}`, "p", "2h", "", ""},
		{"until only", `{"id":"p","until":"2026-11-09T14:00:00Z"}`, "p", "", "2026-11-09T14:00:00Z", ""},
		{
			"both set", `{"id":"p","duration":"2h","until":"2026-11-09T14:00:00Z"}`,
			"p", "2h", "2026-11-09T14:00:00Z", "",
		},
		{"local scope", `{"id":"p","duration":"2h","scope":"local"}`, "p", "2h", "", "local"},
		{"empty object", `{}`, "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got PauseProviderPeakHoursPayload
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &got))
			assert.Equal(t, tc.id, got.ID)
			assert.Equal(t, tc.duration, got.Duration)
			assert.Equal(t, tc.until, got.Until)
			assert.Equal(t, tc.scope, got.Scope)
		})
	}

	t.Run("resume payload scope", func(t *testing.T) {
		t.Parallel()
		var got ResumeProviderPeakHoursPayload
		require.NoError(t, json.Unmarshal([]byte(`{"id":"p","scope":"local"}`), &got))
		assert.Equal(t, "p", got.ID)
		assert.Equal(t, "local", got.Scope)
	})
}

// TestScopeTablePeakHoursCommands pins the scope classification of the
// peak-hours commands and that a non-admin principal is refused.
func TestScopeTablePeakHoursCommands(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		CmdPauseProviderPeakHours, CmdResumeProviderPeakHours, CmdGetPeakHoursStatus,
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			scope, ok := ScopeFor(command)
			require.True(t, ok)
			require.Equal(t, "admin", scope)
			for _, insufficient := range []string{"read", "run", "config"} {
				principal := Principal{Scopes: []string{insufficient}}
				assert.ErrorIs(t, AuthorizeCommand(principal, command, CommandPolicy{AllowAdmin: true}),
					ErrScopeForbidden, "scope=%s command=%s", insufficient, command)
			}
			admin := Principal{Scopes: []string{"admin"}}
			assert.ErrorIs(t, AuthorizeCommand(admin, command, CommandPolicy{AllowAdmin: false}), ErrAdminDisallowed)
			assert.NoError(t, AuthorizeCommand(admin, command, CommandPolicy{AllowAdmin: true}))
		})
	}
}
