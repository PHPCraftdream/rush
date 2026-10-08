package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Revert-check: removing quotaPhase's DB retry branch shuts down work on the first read error.
func TestCLIQuotaActiveReadRetryAndHeartbeat(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			oldPause, oldLimit, oldHeartbeat := cliDBRetryPause, cliDBErrorRetryOverallLimit, cliOpenScopeWaitNoticeInterval
			cliDBRetryPause, cliDBErrorRetryOverallLimit, cliOpenScopeWaitNoticeInterval = 10*time.Millisecond, 80*time.Millisecond, 50*time.Millisecond
			t.Cleanup(func() {
				cliDBRetryPause, cliDBErrorRetryOverallLimit, cliOpenScopeWaitNoticeInterval = oldPause, oldLimit, oldHeartbeat
				cliQuotaActiveWorkSeam = nil
				cliLoopStderr = nil
			})
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) { quotaResponse(w) })
			var out syncBuffer
			cliLoopStderr = &out
			calls := 0
			recoveredAt := time.Time{}
			cliQuotaActiveWorkSeam = func(active bool, err error) (bool, error) {
				calls++
				if persistent || calls < 3 {
					return false, fmt.Errorf("injected active work DB failure")
				}
				if recoveredAt.IsZero() {
					recoveredAt = time.Now()
				}
				return time.Since(recoveredAt) < 150*time.Millisecond, nil
			}
			res, _, err := h.run(loopCtx(t), RunOverrides{})
			require.Error(t, err)
			require.Greater(t, calls, 2)
			if persistent {
				require.Equal(t, "error", res.ExitReason)
				require.Contains(t, out.String(), "giving up")
			} else {
				require.Equal(t, "provider_limit", res.ExitReason)
				require.Contains(t, out.String(), "still has active work")
				require.LessOrEqual(t, strings.Count(out.String(), "still has active work"), 4)
			}
		})
	}
}
