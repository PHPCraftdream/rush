package agent

// REVERT-CHECK:
//   - TestTurnStallProviderRetriesAreSpaced: dropping the
//     lastStallRetryNanos window (retries 15 s apart, abort in ~45 s).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTurnStallToolDetachesAtWarnThreshold was DELETEd as an obsolete
// contract: it pinned the deleted stall-detach ACTION (stallDetacherTool's
// "still running" immediate result at the warn threshold with abort off).
// What survives is the generic stall clock: the provider-retry spacing below
// and the crossing/zero-timeout policy in turn_stall_tool_test.go /
// turn_stall_zero_timeout_test.go.

func TestTurnStallProviderRetriesAreSpaced(t *testing.T) {
	sc, _, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.setPhase("provider", "test-model", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)
	require.Equal(t, int32(1), sc.stallRetries.Load())

	// The next sampler tick 15 s later must leave the re-issued request alone.
	fireStallSampler(t, set, t0, 11*time.Minute+stallSamplerTick)
	require.Equal(t, int32(1), sc.stallRetries.Load(), "a retry needs a full warn window before the next one")
	require.Empty(t, *aborts)

	fireStallSampler(t, set, t0, 11*time.Minute+streamIdleTimeoutDefault+stallSamplerTick)
	require.Equal(t, int32(2), sc.stallRetries.Load())
	require.Empty(t, *aborts)
}
