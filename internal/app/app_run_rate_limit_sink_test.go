// Run-scoped rate-limit diagnostic sink gating: prepareExecuteRun attaches
// agent.WithRateLimitNotifySink only for CLI-origin runs that carry their
// own stderr; every other origin gets no sink at all.

package app

import (
	"bytes"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: deleting the WithRateLimitNotifySink attachment in
// prepareExecuteRun (or widening the gate past OriginCLI / req.Stderr !=
// nil) turns exactly one of the three cases below red.
func TestPrepareExecuteRun_RateLimitSinkGate(t *testing.T) {
	app := newModelEffortApp(t)

	t.Run("CLIWithStderrGetsSink", func(t *testing.T) {
		var stderr bytes.Buffer
		ctx, cancel, _, err := app.prepareExecuteRun(t.Context(), RunRequest{
			Prompt: "p", Origin: message.OriginCLI, Stderr: &stderr, HideSpinner: true,
		})
		require.NoError(t, err)
		t.Cleanup(cancel)
		require.Same(t, &stderr, agent.RateLimitNotifySinkFrom(ctx),
			"a CLI run with its own stderr must carry that stderr as its rate-limit diagnostic sink")
	})

	t.Run("CLIWithoutStderrGetsNoSink", func(t *testing.T) {
		ctx, cancel, _, err := app.prepareExecuteRun(t.Context(), RunRequest{
			Prompt: "p", Origin: message.OriginCLI, HideSpinner: true,
		})
		require.NoError(t, err)
		t.Cleanup(cancel)
		require.Nil(t, agent.RateLimitNotifySinkFrom(ctx))
	})

	t.Run("NonCLIOriginGetsNoSink", func(t *testing.T) {
		var stderr bytes.Buffer
		for _, origin := range []message.Origin{message.OriginWeb, message.OriginSDK} {
			ctx, cancel, _, err := app.prepareExecuteRun(t.Context(), RunRequest{
				Prompt: "p", Origin: origin, Stderr: &stderr, HideSpinner: true,
			})
			require.NoError(t, err)
			cancel()
			require.Nil(t, agent.RateLimitNotifySinkFrom(ctx),
				"origin %q must not receive a rate-limit diagnostic sink", origin)
		}
	})
}
