package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPeakNextTransitionStaysWithinEightRealDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		start string
		end   string
		want  time.Time
	}{
		{"outside horizon", "23:00", "23:30", time.Time{}},
		{"inside horizon", "11:00", "11:30", time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := PeakHoursPolicy{
				Timezone: "UTC", Windows: []PeakHoursWindowSpec{{Days: "tue", Start: tc.start, End: tc.end}},
				PausedUntil: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			}
			require.NoError(t, p.Validate())
			status := p.Evaluate(now)
			require.True(t, status.Paused)
			require.False(t, status.Blocked)
			require.True(t, status.NextTransition.Equal(tc.want), "transition=%s expected=%s", status.NextTransition, tc.want)
		})
	}
}
