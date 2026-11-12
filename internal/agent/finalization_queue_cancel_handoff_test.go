package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationReservationOldCancelHandleCancelsNewRun(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	sessions := fixture.api.(session.Service)
	parent, err := fixture.api.BindAgentFinalization(fixture.ctx, fixture.parent)
	require.NoError(t, err)
	trusted, err := session.WithAgentFinalizationContext(context.Background(), parent)
	require.NoError(t, err)
	data := reservationQueueCall(t, fixture, fixture.parent, session.CaptureAgentFinalizationContext(trusted))
	started, cancelled := make(chan struct{}), make(chan struct{})
	handlerRelease := make(chan struct{})
	var handlerReleaseOnce sync.Once
	releaseHandler := func() { handlerReleaseOnce.Do(func() { close(handlerRelease) }) }
	var startOnce, cancelOnce sync.Once
	var requests atomic.Int64
	requestDetails := make(chan struct {
		method    string
		bodyBytes int64
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		bodyBytes, readErr := io.Copy(io.Discard, request.Body)
		closeErr := request.Body.Close()
		if readErr != nil {
			t.Errorf("read provider request body: %v", readErr)
		}
		if closeErr != nil {
			t.Errorf("close provider request body: %v", closeErr)
		}
		if readErr != nil || closeErr != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush provider response headers: %v", err)
			return
		}
		select {
		case requestDetails <- struct {
			method    string
			bodyBytes int64
		}{method: request.Method, bodyBytes: bodyBytes}:
		default:
		}
		startOnce.Do(func() { close(started) })
		select {
		case <-request.Context().Done():
			cancelOnce.Do(func() { close(cancelled) })
		case <-handlerRelease:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseHandler)
	env := fakeEnv{sessions: sessions, messages: message.NewService(db.New(fixture.conn)), conn: fixture.conn}
	real := newControlRealAgent(t, env, server.URL)
	home := true
	real.(*sessionAgent).wsHomeOverride = &home
	captured := make(chan context.CancelFunc, 1)
	driver := &reservationBarrierAgent{SessionAgent: real, capture: func(_ context.Context, cancel context.CancelFunc) { captured <- cancel }}
	coordinator := &coordinator{sessions: sessions, currentAgent: driver}
	var observedDeadline time.Time
	var hasDeadline bool
	probe := &reservationQueueProbe{coordinator: coordinator, execute: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observedDeadline, hasDeadline = ctx.Deadline()
		return driver.Run(ctx, call)
	}}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: probe, PumpInstanceID: "pump"})
	defer pump.Stop()
	deadline := time.Now().Add(5 * time.Minute)
	operation, stop := context.WithDeadline(context.Background(), deadline)
	finished := make(chan struct{})
	var outcome session.DrainResult
	go func() {
		defer close(finished)
		outcome, _ = pump.DrainSessionNow(operation, data.SessionID)
	}()
	t.Cleanup(func() {
		stop()
		server.CloseClientConnections()
		releaseHandler()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		joined := false
		timedOut := false
		select {
		case <-finished:
			joined = true
		case <-timer.C:
			select {
			case <-finished:
				joined = true
			default:
				timedOut = true
			}
		}
		if timedOut {
			t.Errorf("reserved Run cleanup timed out")
		}
		if t.Failed() {
			stacks := make([]byte, 1<<20)
			n := runtime.Stack(stacks, true)
			t.Logf("reserved Run failure state: timed_out=%t finished=%t requests=%d builds=%d runs=%d releases=%d mailbox_busy=%t\n%s", timedOut, joined, requests.Load(), probe.builds.Load(), probe.runs.Load(), probe.releases.Load(), real.IsSessionBusy(data.SessionID), stacks[:n])
		}
	})
	wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-wait.Done():
		t.Fatal("reserved Run never reached the local provider")
	}
	details := <-requestDetails
	require.Equal(t, http.MethodPost, details.method)
	require.Greater(t, details.bodyBytes, int64(0), "provider request body must be nonempty")
	var oldCancel context.CancelFunc
	select {
	case oldCancel = <-captured:
	case <-wait.Done():
		t.Fatal("reservation cancel handle was not captured")
	}
	// This is the OLD beginCompact handle, captured before Run rebinds its
	// dispatcher cancel. It must remain effective after the actual handoff.
	oldCancel()
	select {
	case <-cancelled:
	case <-wait.Done():
		t.Fatal("the old cancel handle did not cancel the in-flight new Run")
	}
	select {
	case <-finished:
	case <-wait.Done():
		t.Fatal("cancelled reserved Run did not release its era")
	}
	require.NotEqual(t, session.DrainComplete, outcome)
	require.EqualValues(t, 1, requests.Load())
	require.EqualValues(t, 1, probe.builds.Load())
	require.EqualValues(t, 1, probe.runs.Load())
	require.EqualValues(t, 1, probe.releases.Load())
	require.True(t, hasDeadline)
	require.Equal(t, deadline, observedDeadline, "handoff must not reset the parent time budget")
	require.False(t, real.IsSessionBusy(data.SessionID))
	require.EqualValues(t, 1, reservationConsumed(t, fixture))
}
