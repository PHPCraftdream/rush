package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// R6B-2: the test-only interval override honours the one-second floor like
// every other source: a value below it is ignored (the built-in default
// stands), a value at or above it is used. Not parallel (process-wide state).
//
// Revert-check: returning the override unconditionally from
// defaultSupervisionInterval (the pre-fix code) makes the 500ms case red.
func TestSupervisionDefaultInterval_OverrideBelowFloorIsIgnored(t *testing.T) {
	c := &coordinator{}
	ctx := context.Background()

	t.Run("below the floor", func(t *testing.T) {
		t.Cleanup(SetSupervisionDefaultIntervalForTest(500 * time.Millisecond))
		require.Equal(t, supervisionDefaultInterval, c.resolveSupervisionConfig(ctx).Interval)
		require.Equal(t, supervisionDefaultInterval, DefaultSupervisionConfig().Interval)
	})
	t.Run("at the floor", func(t *testing.T) {
		t.Cleanup(SetSupervisionDefaultIntervalForTest(supervisionMinIntervalFloor))
		require.Equal(t, supervisionMinIntervalFloor, c.resolveSupervisionConfig(ctx).Interval)
	})
	t.Run("above the floor", func(t *testing.T) {
		t.Cleanup(SetSupervisionDefaultIntervalForTest(1100 * time.Millisecond))
		require.Equal(t, 1100*time.Millisecond, c.resolveSupervisionConfig(ctx).Interval)
	})
}
