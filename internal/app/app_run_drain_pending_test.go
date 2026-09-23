package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression (2026-09-23, five hung sessions): a durable row left by a
// previous process is admitted by THIS process's pump at startup. A
// `rush run` on the same session must wait for it and then run its own
// turn — never queue behind it, exit "queued" and cancel it on the way out.
func TestExecuteRun_WaitsForLocalPumpTurnInsteadOfQueueing(t *testing.T) {
	staleStarted := make(chan struct{})
	var startOnce sync.Once
	var staleDone, freshDone atomic.Bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "FRESH_TURN_MARKER"):
			freshDone.Store(true)
			admissionWriteSSE(w, []string{admissionSSEText("c1", "fresh ok"), admissionSSEStop("c1", "stop")})
		case strings.Contains(string(body), "STALE_ROW_MARKER"):
			startOnce.Do(func() { close(staleStarted) })
			time.Sleep(1500 * time.Millisecond) // pump turn still running when the run arrives
			staleDone.Store(true)
			admissionWriteSSE(w, []string{admissionSSEText("c0", "stale ok"), admissionSSEStop("c0", "stop")})
		default:
			admissionWriteSSE(w, []string{admissionSSEText("c2", "title"), admissionSSEStop("c2", "stop")})
		}
	}
	application, sessionID := newAdmissionRaceApp(t, handler)
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(t.Context(), "stale-row", sessionID,
		[]byte(`{"SessionID":"`+sessionID+`","Prompt":"STALE_ROW_MARKER previous process task"}`)))

	select {
	case <-staleStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the pump never picked up the stale durable row")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := application.ExecuteRun(ctx, RunRequest{
		Prompt: "FRESH_TURN_MARKER", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err, "the run must not report queued behind its own process's pump turn")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.True(t, staleDone.Load(), "the stale durable task must complete, not be cancelled")
	require.True(t, freshDone.Load(), "the run's own turn must execute")

	outstanding, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.False(t, outstanding, "no durable row may be left behind")
}

// Regression (2026-09-23, r2-15 on alpha.3): the drain can return while a
// turn in this process still owns the session (there: the pump's turn lost
// its lease and was still unwinding). The run must wait for that owner to
// release instead of queueing behind it and exiting "queued".
func TestExecuteRun_WaitsForLocalOwnerAfterDrainReturns(t *testing.T) {
	ownerStarted := make(chan struct{})
	releaseOwner := make(chan struct{})
	var ownerOnce sync.Once
	var freshDone atomic.Bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "FRESH_TURN_MARKER"):
			freshDone.Store(true)
			admissionWriteSSE(w, []string{admissionSSEText("c1", "fresh ok"), admissionSSEStop("c1", "stop")})
		case strings.Contains(string(body), "OWNER_TURN_MARKER"):
			ownerOnce.Do(func() { close(ownerStarted) })
			<-releaseOwner
			admissionWriteSSE(w, []string{admissionSSEText("c0", "owner ok"), admissionSSEStop("c0", "stop")})
		default:
			admissionWriteSSE(w, []string{admissionSSEText("c2", "other ok"), admissionSSEStop("c2", "stop")})
		}
	}
	application, sessionID := newAdmissionRaceApp(t, handler)

	outcomes := make(chan admissionOutcome, 2)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "OWNER_TURN_MARKER", RunOverrides{}, false)
	close(start)
	select {
	case <-ownerStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("owner turn never reached the provider")
	}
	// Pending durable work exists while the local owner is mid-turn: the
	// drain finds the session busy and returns without running anything.
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(t.Context(), "pending-row", sessionID,
		[]byte(`{"SessionID":"`+sessionID+`","Prompt":"PENDING_ROW_MARKER"}`)))
	time.AfterFunc(1500*time.Millisecond, func() { close(releaseOwner) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := application.ExecuteRun(ctx, RunRequest{
		Prompt: "FRESH_TURN_MARKER", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err, "the run must wait for the local owner, not report queued")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.True(t, freshDone.Load(), "the run's own turn must execute")

	select {
	case o := <-outcomes:
		require.NoError(t, o.err, "the owner turn must complete, not be cancelled")
	case <-time.After(30 * time.Second):
		t.Fatal("owner turn did not finish")
	}
}
