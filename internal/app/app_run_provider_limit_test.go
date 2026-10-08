package app

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Revert-check: limitwords.Classify's Codex type branch must latch provider_limit.
func TestCLIProviderLimit1282CodexShape(t *testing.T) {
	// https://github.com/acmiyaguchi/fen/issues/583; not reproduced locally.
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":1800}}`)
	})
	before := time.Now()
	result, _, err := h.run(loopCtx(t), RunOverrides{})
	require.Error(t, err)
	require.Equal(t, "provider_limit", result.ExitReason)
	require.NotEmpty(t, result.ResumeCommand)
	reset, parseErr := time.Parse(time.RFC3339, result.QuotaResetAt)
	require.NoError(t, parseErr)
	require.True(t, reset.After(before.Add(29*time.Minute)))
	require.EqualValues(t, 3, h.requests.Load())
}
