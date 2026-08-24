// peak_hours window parsing tests: the parsePeakHoursWindow table
// (normal/overnight/boundary windows, clearing forms, error cases) and
// its delegation to config.PeakHoursWindow.Validate.
package cmd

import (
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
