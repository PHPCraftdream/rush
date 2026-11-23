package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Cancel is session-scoped in the coordinator, so the finished run's cancel
// entry must be gone BEFORE its deny gate is lifted: from that moment on a new
// run may start on the same session and a stale entry would stop it.
func TestAPIRunBackendDropsCancelEntryBeforeLiftingGate(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error, 1)
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	backend := newAPIRunBackend(app)
	var cancelAtRelease []bool
	manager.onRelease = func(sessionID, ownerToken string) {
		if ownerToken == "run-order" {
			cancelAtRelease = append(cancelAtRelease, backend.Cancel(sessionID, ownerToken))
		}
	}
	done, err := backend.Start(t.Context(), "run-order", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	runReturn <- nil
	require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)

	require.Equal(t, []bool{false}, cancelAtRelease, "cancel entry still registered when the gate was lifted")
	require.Empty(t, coordinator.cancelled, "a stale cancel reached the coordinator after the run ended")
}
