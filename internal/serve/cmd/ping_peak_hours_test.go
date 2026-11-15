package cmd

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestFormatProviderPeakHoursStatusPreservesLegacyAndWeekly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
	require.Empty(t, formatProviderPeakHoursStatus(nil, now))
	legacy := &config.PeakHoursWindow{Start: "09:00", End: "18:00"}
	require.Equal(t, formatPeakHoursStatus(legacy, now),
		formatProviderPeakHoursStatus(&config.ProviderPeakHours{Legacy: legacy}, now))

	weekly := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Days: "mon", Start: "09:00", End: "18:00"}},
	}}
	require.NoError(t, weekly.Validate())
	require.Equal(t, "1 weekly windows (UTC; in peak (reopens 2026-11-09 18:00))",
		formatProviderPeakHoursStatus(weekly, now))
	weekly.Policy.PausedUntil = "2026-11-09T13:00:00Z"
	require.Equal(t, "1 weekly windows (UTC; paused until 2026-11-09 13:00)",
		formatProviderPeakHoursStatus(weekly, now))
	weekly.Policy.Windows = nil
	weekly.Policy.PausedUntil = ""
	require.Equal(t, "0 weekly windows (UTC; open)", formatProviderPeakHoursStatus(weekly, now))
}
