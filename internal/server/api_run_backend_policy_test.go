package server

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// startPolicyRun starts a run as a principal with the given permission mode and
// scopes against the real permission service.
func startPolicyRun(t *testing.T, allowAuto bool, mode string, scopes []string) (backend APIRunBackend, sessionID string, policies permission.SessionPolicyManager, runReturn chan error, done <-chan RunOutcome, err error) {
	t.Helper()
	runReturn = make(chan error, 1)
	manager := &apiRunBackendGateManager{}
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	// Use the real permission service, which implements both gate interfaces.
	app.Permissions = manager.Service
	sess, createErr := app.Sessions.Create(t.Context(), "policy")
	require.NoError(t, createErr)
	policies, ok := app.Permissions.(permission.SessionPolicyManager)
	require.True(t, ok)
	backend = newAPIRunBackendWithPolicy(app, nil, allowAuto)
	ctx := WithPrincipal(t.Context(), Principal{TokenID: "token", Scopes: scopes, Permission: mode})
	done, err = backend.Start(ctx, "run-policy", sess.ID, "prompt", "", "")
	if err == nil {
		awaitAPIRunGateEvent(t, coordinator.runStarted)
	}
	return backend, sess.ID, policies, runReturn, done, err
}

func awaitPolicyInactive(t *testing.T, policies permission.SessionPolicyManager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if active, _ := policies.SessionPolicyDecision(sessionID, "bash", "execute"); !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session policy was not released after the run finished")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAPIRunBackendAutoModeRequiresAdminOrListenerFlag(t *testing.T) {
	_, sessionID, policies, _, _, err := startPolicyRun(t, false, "auto", []string{"run"})
	require.ErrorIs(t, err, ErrAPIRunAutoApproveDenied)
	active, _ := policies.SessionPolicyDecision(sessionID, "bash", "execute")
	require.False(t, active, "a refused auto run must not leave a policy armed")
}

func TestAPIRunBackendAutoModeAllowedByListenerFlagOrAdminScope(t *testing.T) {
	for name, tc := range map[string]struct {
		allow  bool
		scopes []string
	}{
		"listener flag": {allow: true, scopes: []string{"run"}},
		"admin scope":   {allow: false, scopes: []string{"run", "admin"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, sessionID, policies, runReturn, done, err := startPolicyRun(t, tc.allow, "auto", tc.scopes)
			require.NoError(t, err)
			active, allowed := policies.SessionPolicyDecision(sessionID, "bash", "execute")
			require.True(t, active)
			require.True(t, allowed)
			runReturn <- nil
			require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
			awaitPolicyInactive(t, policies, sessionID)
		})
	}
}

func TestAPIRunBackendReadonlyModeAllowsOnlyReadTools(t *testing.T) {
	_, sessionID, policies, runReturn, done, err := startPolicyRun(t, false, "readonly", []string{"run"})
	require.NoError(t, err)
	active, allowed := policies.SessionPolicyDecision(sessionID, "view", "read")
	require.True(t, active)
	require.True(t, allowed)
	active, allowed = policies.SessionPolicyDecision(sessionID, "bash", "execute")
	require.True(t, active)
	require.False(t, allowed)
	runReturn <- nil
	awaitAPIRunGateOutcome(t, done)
	awaitPolicyInactive(t, policies, sessionID)
}

func TestAPIRunBackendEmptyPermissionDefaultsToAskThroughBroker(t *testing.T) {
	runReturn := make(chan error, 1)
	manager := &apiRunBackendGateManager{}
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	app.Permissions = manager.Service
	sess, err := app.Sessions.Create(t.Context(), "default ask")
	require.NoError(t, err)
	broker := NewAPIPermissionBroker(app.Permissions, "", nil)
	require.NoError(t, broker.Start(t.Context()))
	defer broker.Stop()
	backend := newAPIRunBackendWithPolicy(app, broker, false)
	ctx := WithPrincipal(t.Context(), Principal{TokenID: "token", Scopes: []string{"run"}})
	done, err := backend.Start(ctx, "run-default", sess.ID, "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)

	broker.mu.Lock()
	_, registered := broker.registrations["run-default"]
	broker.mu.Unlock()
	require.True(t, registered, "an empty permission mode must ask through the broker")
	policies, ok := app.Permissions.(permission.SessionPolicyManager)
	require.True(t, ok)
	active, _ := policies.SessionPolicyDecision(sess.ID, "bash", "execute")
	require.False(t, active, "no readonly or auto policy may be armed for ask")

	runReturn <- nil
	awaitAPIRunGateOutcome(t, done)
	deadline := time.Now().Add(5 * time.Second)
	for {
		broker.mu.Lock()
		_, stillRegistered := broker.registrations["run-default"]
		broker.mu.Unlock()
		if !stillRegistered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker registration was not released when the run finished")
		}
		time.Sleep(time.Millisecond)
	}
}
