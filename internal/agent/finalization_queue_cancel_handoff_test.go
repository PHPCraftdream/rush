package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	var startOnce, cancelOnce sync.Once
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		startOnce.Do(func() { close(started) })
		<-request.Context().Done()
		cancelOnce.Do(func() { close(cancelled) })
	}))
	t.Cleanup(server.Close)
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
	defer func() {
		stop()
		server.CloseClientConnections()
		<-finished
	}()
	wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-wait.Done():
		t.Fatal("reserved Run never reached the local provider")
	}
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
