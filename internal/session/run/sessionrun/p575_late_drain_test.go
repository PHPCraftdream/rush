package sessionrun

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/run/sessiontest"
	"github.com/stretchr/testify/require"
)

func TestLiveIdentityNotificationAllowsReplay(t *testing.T) {
	ch := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sessiontest.NotifyLiveIdentity(ch, "assistant-tool-step")
		sessiontest.NotifyLiveIdentity(ch, "assistant-tool-step")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Освобождаем отправителя и в мутанте с блокирующей записью.
		select {
		case <-ch:
		default:
		}
		t.Fatal("identity replay notifier blocked")
	}
	select {
	case id := <-ch:
		require.Equal(t, "assistant-tool-step", id)
	default:
		t.Fatal("identity notification missing")
	}
}

func TestAdmissionRefusalNotificationAllowsRepeatedObservations(t *testing.T) {
	ch := make(chan struct{}, 1)
	sessiontest.NotifyAdmissionRefusal(ch)
	sessiontest.NotifyAdmissionRefusal(ch)
	select {
	case <-ch:
	default:
		t.Fatal("admission notification missing")
	}
	sessiontest.NotifyAdmissionRefusal(ch)
	select {
	case <-ch:
	default:
		t.Fatal("repeated admission notification missing")
	}
}

func waitForBackgroundAdmissionRelease(t *testing.T, pump *RunQueuePump, sessionID string) {
	t.Helper()
	sessiontest.AwaitPump(t, func() bool {
		release, admitted := pump.AdmitSessionForTest(sessionID)
		if !admitted {
			return false
		}
		release(nil)
		return true
	}, "background admission must be released")
}

func TestDrainSessionNow_LateStartDoesNotInheritCompletedIdentities(t *testing.T) {
	tl := sessiontest.NewTestTimeline()
	gate := newGatedCoordinator()
	gate.identities = []string{"assistant-tool-step", "assistant-final"}
	pump, sessionID := oneEntrySetupReported(t, "late-identity", gate, tl)

	tl.Addf("releasing background before drain starts")
	gate.release <- nil
	waitForBackgroundAdmissionRelease(t, pump, sessionID)
	tl.Addf("background admission released; starting late drain")

	called := false
	ctx := WithDrainAssistantIdentity(context.Background(), func(map[string]struct{}, string) {
		called = true
	})
	result, err := pump.DrainSessionNow(ctx, sessionID)
	tl.Addf("late drain returned result=%v err=%v callback=%v", result, err, called)
	require.NoError(t, err)
	require.Equal(t, DrainNoWork, result)
	require.False(t, called, "a late drain must not inherit an execution it never observed")
	require.Equal(t, int64(1), gate.calls.Load())
}
