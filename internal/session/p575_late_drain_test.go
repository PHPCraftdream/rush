package session_test

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func notifyAdmissionRefusal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func TestAdmissionRefusalNotificationAllowsRepeatedObservations(t *testing.T) {
	ch := make(chan struct{}, 1)
	notifyAdmissionRefusal(ch)
	notifyAdmissionRefusal(ch)
	select {
	case <-ch:
	default:
		t.Fatal("admission notification missing")
	}
	notifyAdmissionRefusal(ch)
	select {
	case <-ch:
	default:
		t.Fatal("repeated admission notification missing")
	}
}

func waitForBackgroundAdmissionRelease(t *testing.T, pump *session.RunQueuePump, sessionID string) {
	t.Helper()
	awaitPump(t, func() bool {
		release, admitted := pump.AdmitSessionForTest(sessionID)
		if !admitted {
			return false
		}
		release(nil)
		return true
	}, "background admission must be released")
}

func TestDrainSessionNow_LateStartDoesNotInheritCompletedIdentities(t *testing.T) {
	tl := newTestTimeline()
	gate := newGatedCoordinator()
	gate.identities = []string{"assistant-tool-step", "assistant-final"}
	pump, sessionID := oneEntrySetupReported(t, "late-identity", gate, tl)

	tl.addf("releasing background before drain starts")
	gate.release <- nil
	waitForBackgroundAdmissionRelease(t, pump, sessionID)
	tl.addf("background admission released; starting late drain")

	called := false
	ctx := session.WithDrainAssistantIdentity(context.Background(), func(map[string]struct{}, string) {
		called = true
	})
	result, err := pump.DrainSessionNow(ctx, sessionID)
	tl.addf("late drain returned result=%v err=%v callback=%v", result, err, called)
	require.NoError(t, err)
	require.Equal(t, session.DrainNoWork, result)
	require.False(t, called, "a late drain must not inherit an execution it never observed")
	require.Equal(t, int64(1), gate.calls.Load())
}
