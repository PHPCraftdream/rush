package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunDrainsPendingAfterLocalOwnerReleases_Round13_1(t *testing.T) {
	ownerStarted := make(chan struct{})
	releaseOwner := make(chan struct{})
	firstAttemptReturned := make(chan struct{})
	order := make(chan string, 4)
	var requests atomic.Uint32
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch requests.Add(1) {
		case 1:
			order <- "owner-start"
			close(ownerStarted)
			<-releaseOwner
			order <- "owner-end"
			admissionWriteSSE(w, []string{admissionSSEText("owner", "owner done"), admissionSSEStop("owner", "stop")})
		case 2:
			order <- "pending"
			admissionWriteSSE(w, []string{admissionSSEText("pending", "pending done"), admissionSSEStop("pending", "stop")})
		case 3:
			order <- "fresh"
			admissionWriteSSE(w, []string{admissionSSEText("fresh", "fresh done"), admissionSSEStop("fresh", "stop")})
		default:
			admissionWriteSSE(w, []string{admissionSSEText("other", "other"), admissionSSEStop("other", "stop")})
		}
	}
	application, sessionID := newAdmissionRaceApp(t, http.HandlerFunc(handler))
	ownerOutcome := make(chan admissionOutcome, 1)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, ownerOutcome, start, 1, "ROUND13_OWNER", RunOverrides{}, false)
	close(start)
	select {
	case <-ownerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("local owner did not start")
	}
	defer func() {
		select {
		case <-releaseOwner:
		default:
			close(releaseOwner)
		}
	}()
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(t.Context(), "round13-pending", sessionID,
		[]byte(`{"SessionID":"`+sessionID+`","Prompt":"ROUND13_PENDING"}`)))

	oldSeam := drainPendingAfterAttemptSeam
	drainPendingAfterAttemptSeam = func(gotSession string) {
		if gotSession == sessionID {
			select {
			case <-firstAttemptReturned:
			default:
				close(firstAttemptReturned)
			}
		}
	}
	t.Cleanup(func() { drainPendingAfterAttemptSeam = oldSeam })

	resultCh := make(chan admissionOutcome, 1)
	go func() {
		result, err := application.ExecuteRun(context.Background(), RunRequest{
			Prompt: "ROUND13_FRESH", Mode: RunModeJSON, ContinueSessionID: sessionID,
			Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		})
		resultCh <- admissionOutcome{res: result, err: err}
	}()
	select {
	case <-firstAttemptReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("initial busy drain attempt did not return")
	}
	outstandingAfterBusyAttempt, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.True(t, outstandingAfterBusyAttempt, "the busy drain must leave Q pending for the post-owner retry")
	close(releaseOwner)
	select {
	case outcome := <-resultCh:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.res)
	case <-time.After(20 * time.Second):
		t.Fatal("ExecuteRun did not finish")
	}
	select {
	case outcome := <-ownerOutcome:
		require.NoError(t, outcome.err)
	case <-time.After(10 * time.Second):
		t.Fatal("local owner did not finish")
	}
	require.Equal(t, []string{"owner-start", "owner-end", "pending", "fresh"}, []string{<-order, <-order, <-order, <-order})
	outstanding, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.False(t, outstanding)
}

type round13PreflightFailureService struct {
	session.Service
	err error
}

func (s round13PreflightFailureService) HasOutstandingRunQueueEntriesForSession(context.Context, string) (bool, error) {
	return false, s.err
}

func TestExecuteRunPreflightErrorBlocksTurn_Round13_1(t *testing.T) {
	providerCalled := make(chan struct{}, 1)
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		providerCalled <- struct{}{}
		admissionWriteSSE(w, []string{admissionSSEText("unexpected", "unexpected"), admissionSSEStop("unexpected", "stop")})
	})
	wantErr := errors.New("injected queue check failure")
	application.Sessions = round13PreflightFailureService{Service: application.Sessions, err: wantErr}

	result, err := application.ExecuteRun(t.Context(), RunRequest{
		Prompt: "ROUND13_MUST_NOT_RUN", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.Nil(t, result)
	require.ErrorIs(t, err, wantErr)
	select {
	case <-providerCalled:
		t.Fatal("provider was called after preflight failed")
	default:
	}
}

func TestExecuteRunDrainWaitCancellationReturnsError_Round13_1(t *testing.T) {
	ownerStarted := make(chan struct{})
	releaseOwner := make(chan struct{})
	firstAttemptReturned := make(chan struct{})
	var requests atomic.Uint32
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			close(ownerStarted)
			<-releaseOwner
		}
		admissionWriteSSE(w, []string{admissionSSEText("done", "done"), admissionSSEStop("done", "stop")})
	}
	application, sessionID := newAdmissionRaceApp(t, handler)
	ownerOutcome := make(chan admissionOutcome, 1)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, ownerOutcome, start, 1, "ROUND13_CANCEL_OWNER", RunOverrides{}, false)
	close(start)
	select {
	case <-ownerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("local owner did not start")
	}
	defer func() {
		select {
		case <-releaseOwner:
		default:
			close(releaseOwner)
		}
	}()
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(t.Context(), "round13-cancel-pending", sessionID,
		[]byte(`{"SessionID":"`+sessionID+`","Prompt":"ROUND13_CANCEL_PENDING"}`)))

	oldSeam := drainPendingAfterAttemptSeam
	drainPendingAfterAttemptSeam = func(gotSession string) {
		if gotSession == sessionID {
			select {
			case <-firstAttemptReturned:
			default:
				close(firstAttemptReturned)
			}
		}
	}
	t.Cleanup(func() { drainPendingAfterAttemptSeam = oldSeam })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan admissionOutcome, 1)
	go func() {
		result, err := application.ExecuteRun(ctx, RunRequest{
			Prompt: "ROUND13_CANCEL_FRESH", Mode: RunModeJSON, ContinueSessionID: sessionID,
			Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		})
		resultCh <- admissionOutcome{res: result, err: err}
	}()
	select {
	case <-firstAttemptReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("initial busy drain attempt did not return")
	}
	cancel()
	close(releaseOwner)
	select {
	case outcome := <-resultCh:
		require.Nil(t, outcome.res)
		require.ErrorIs(t, outcome.err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled queue wait did not return promptly")
	}
	select {
	case outcome := <-ownerOutcome:
		require.NoError(t, outcome.err)
	case <-time.After(10 * time.Second):
		t.Fatal("local owner did not finish")
	}
}
