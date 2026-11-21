package providers

import (
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
)

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
