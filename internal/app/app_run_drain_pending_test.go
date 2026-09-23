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
