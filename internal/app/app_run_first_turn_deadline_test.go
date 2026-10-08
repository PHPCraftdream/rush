package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// Revert-check: ExecuteRun's classifyRunTimeout defer and cliLoop.finish pin explicit/default-cap first-turn timeout envelope/error.
func TestRunNonInteractive_FirstTurnDeadline_FlushesTimeoutEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		defaultCap bool
		wantError  string
	}{
		{name: "explicit", source: "--timeout", wantError: "run timeout 3s exceeded (source: --timeout)"},
		{name: "default-cap", source: "default cap (RUSH_RUN_DEFAULT_HARD_TIMEOUT; no --timeout set)", defaultCap: true, wantError: "run timeout 3s exceeded (source: default cap (RUSH_RUN_DEFAULT_HARD_TIMEOUT; no --timeout set); pass --timeout to change the cap)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := make(chan struct{})
			h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
				<-block // the model never answers within the deadline
			})
			t.Cleanup(func() { close(block) })

			cause := &agent.RunTimeoutCause{Duration: 3 * time.Second, Source: tc.source, DefaultCap: tc.defaultCap}
			ctx, cancel := context.WithTimeoutCause(t.Context(), cause.Duration, cause)
			defer cancel()
			res, out, err := h.run(ctx, RunOverrides{})

			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotNil(t, res, "the interrupted first turn still owes an envelope")
			require.Equal(t, "timeout", res.ExitReason)
			require.NotEmpty(t, res.SessionID)
			require.Equal(t, 1, strings.Count(out, `"exit_reason"`), "one envelope is flushed: %q", out)

			var wire RunResult
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &wire))
			require.Equal(t, "timeout", wire.ExitReason)
			require.Equal(t, res.SessionID, wire.SessionID)
			var timeoutErr *RunTimeoutError
			require.ErrorAs(t, err, &timeoutErr)
			require.Same(t, cause, timeoutErr.Cause)
			require.Equal(t, 3*time.Second, timeoutErr.Cause.Duration)
			require.Equal(t, tc.source, timeoutErr.Cause.Source)
			require.Equal(t, tc.defaultCap, timeoutErr.Cause.DefaultCap)
			require.EqualError(t, err, tc.wantError)
			require.Equal(t, "rush run --role smart --session "+res.SessionID, wire.ResumeCommand)
			require.Equal(t, wire.ResumeCommand, timeoutErr.ResumeCommand)
			require.Equal(t, wire.ResumeCommand, res.ResumeCommand)
			require.Equal(t, tc.wantError, wire.Error)
			require.Equal(t, wire.Error, res.Error)
			sess, getErr := h.app.Sessions.Get(context.Background(), res.SessionID)
			require.NoError(t, getErr)
			require.Equal(t, "timeout", sess.EndedReason)
		})
	}
}
