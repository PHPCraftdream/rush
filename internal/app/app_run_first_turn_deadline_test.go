package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A `--timeout` (or the default cap) expiring while the user's own first turn
// is still generating must still end the run with one JSON envelope naming the
// session and exit_reason "canceled": wrappers parse stdout, and an empty one
// is indistinguishable from a crash.
//
// Revert-check: dropping the synthesized envelope in exitCanceled leaves
// stdout empty and this test red.
func TestRunNonInteractive_FirstTurnDeadline_FlushesCanceledEnvelope(t *testing.T) {
	block := make(chan struct{})
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		<-block // the model never answers within the deadline
	})
	// Registered after the harness so it runs first: the server's Close waits
	// for this handler.
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, out, err := h.run(ctx, RunOverrides{})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, res, "the interrupted first turn still owes an envelope")
	require.Equal(t, "canceled", res.ExitReason)
	require.NotEmpty(t, res.SessionID)
	require.Equal(t, 1, strings.Count(out, `"exit_reason"`), "one envelope is flushed: %q", out)

	var wire RunResult
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &wire))
	require.Equal(t, "canceled", wire.ExitReason)
	require.Equal(t, res.SessionID, wire.SessionID)
	require.Contains(t, wire.Error, "deadline exceeded")
}
