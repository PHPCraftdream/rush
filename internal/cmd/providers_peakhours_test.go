// peak_hours window parsing tests: the parsePeakHoursWindow table
// (normal/overnight/boundary windows, clearing forms, error cases), its
// delegation to config.PeakHoursWindow.Validate, and the `providers set
// --peak-hours` CLI's message-preservation behavior on a time-only update.
package cmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePeakHoursWindow(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNil   bool
		wantStart string
		wantEnd   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "normal window",
			input:     "09:00-18:00",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "overnight window",
			input:     "22:00-06:00",
			wantNil:   false,
			wantStart: "22:00",
			wantEnd:   "06:00",
		},
		{
			name:      "boundary start end of day",
			input:     "00:00-23:59",
			wantNil:   false,
			wantStart: "00:00",
			wantEnd:   "23:59",
		},
		{
			name:    "empty clears (nil)",
			input:   "",
			wantNil: true,
		},
		{
			name:    "off clears (nil)",
			input:   "off",
			wantNil: true,
		},
		{
			name:    "OFF clears case-insensitive",
			input:   "OFF",
			wantNil: true,
		},
		{
			name:    "whitespace-only clears (nil)",
			input:   "   ",
			wantNil: true,
		},
		{
			name:      "trims whitespace",
			input:     "  09:00 - 18:00  ",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "missing dash",
			input:     "09:00",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
		{
			name:      "missing end time",
			input:     "09:00-",
			wantErr:   true,
			errSubstr: "both start and end must be set",
		},
		{
			name:      "bad start format no leading zero",
			input:     "9:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "bad end format dash instead of colon",
			input:     "09:00-18-00",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "hour out of range",
			input:     "24:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "minute out of range",
			input:     "09:00-18:60",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "garbage input",
			input:     "nope",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parsePeakHoursWindow(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr)
				}
				assert.Nil(t, w)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, w)
				return
			}
			require.NotNil(t, w)
			assert.Equal(t, tt.wantStart, w.Start)
			assert.Equal(t, tt.wantEnd, w.End)
		})
	}
}

func TestParsePeakHoursWindow_ReusesConfigValidate(t *testing.T) {
	// The parser must delegate to PeakHoursWindow.Validate, so a window
	// that Validate rejects must also be rejected here.
	w := config.PeakHoursWindow{Start: "09:00", End: ""}
	require.Error(t, w.Validate())

	_, err := parsePeakHoursWindow("09:00-")
	require.Error(t, err)
}

// TestProvidersSet_PeakHoursMessagePreservedOnTimeOnlyUpdate is a
// regression for the CLI's own documented contract ("Only the flags you
// pass are written — unset fields are left untouched"): --peak-hours only
// carries HH:MM-HH:MM (there is no CLI flag for the message, which is
// web-UI-only), so updating just the time window must not silently drop a
// message configured earlier through the web UI.
func TestProvidersSet_PeakHoursMessagePreservedOnTimeOnlyUpdate(t *testing.T) {
	seedJSON := `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00", "message": "Ping #ops-oncall first."}
    }
  }
}`
	_, globalDataPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedJSON, "with-peak --peak-hours 10:00-20:00")

	raw, err := os.ReadFile(globalDataPath)
	require.NoError(t, err)

	var out struct {
		Providers map[string]struct {
			PeakHours *config.PeakHoursWindow `json:"peak_hours"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))

	p, ok := out.Providers["with-peak"]
	require.True(t, ok, "provider must still be present after the update")
	require.NotNil(t, p.PeakHours, "peak_hours must not be cleared by a time-only update")
	assert.Equal(t, "10:00", p.PeakHours.Start, "the time window must actually update")
	assert.Equal(t, "20:00", p.PeakHours.End)
	assert.Equal(t, "Ping #ops-oncall first.", p.PeakHours.Message,
		"the message configured through the web UI must survive a CLI-side time-only --peak-hours update")
}
