package app

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// peakWindowCovering returns a peak-hours window that contains now whatever
// the wall clock reads: an hour either side, wrapping past midnight when it
// has to. A fixed 00:00-23:59 window is not "always": End is exclusive, so
// the last minute of every day falls outside it and a test relying on it
// fails once a day (TestRunNonInteractive_PeakRefusalNeverSettles did, at
// 23:59:46).
func peakWindowCovering(now time.Time) config.PeakHoursWindow {
	return config.PeakHoursWindow{
		Start: now.Add(-time.Hour).Format("15:04"),
		End:   now.Add(time.Hour).Format("15:04"),
	}
}

// TestPeakWindowCoveringContainsEveryMinuteOfTheDay proves the helper holds at
// every minute (midnight wrap included) and pins the defect of the fixed
// window it replaces.
//
// Revert-check: make peakWindowCovering return Start "00:00", End "23:59" and
// the 23:59 iteration goes red.
func TestPeakWindowCoveringContainsEveryMinuteOfTheDay(t *testing.T) {
	base := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	for minute := 0; minute < 24*60; minute++ {
		now := base.Add(time.Duration(minute) * time.Minute)
		require.True(t, peakWindowCovering(now).InPeakHours(now), "minute %s", now.Format("15:04"))
	}

	fixed := config.PeakHoursWindow{Start: "00:00", End: "23:59"}
	require.False(t, fixed.InPeakHours(time.Date(2026, 1, 15, 23, 59, 30, 0, time.UTC)),
		"the fixed window misses the last minute of the day")
}
