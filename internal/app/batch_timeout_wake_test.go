package app

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchTimeoutWakeDispatcherBoundsAndJoins(t *testing.T) {
	d := newBatchTimeoutWakeDispatcher()
	t.Cleanup(d.Stop)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	d.Attach(wakeDispatchTestCoordinator{wake: func(ctx context.Context, _ string, _ bool) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}})
	d.Wake(context.Background(), "owner")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not start")
	}
	queued := make(chan struct{})
	go func() {
		for i := 0; i < batchTimeoutWakeQueueSize*4; i++ {
			d.Wake(context.Background(), "owner")
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("wake enqueue blocked at queue capacity")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); d.Stop() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight dispatch was not canceled")
	}
	wg.Wait()
	select {
	case <-d.done:
	default:
		t.Fatal("dispatcher did not join")
	}
	var _ agent.SessionWakeDispatcher = wakeDispatchTestCoordinator{}
}

func TestBatchTimeoutCheckinFutureAndMalformedPayloadsDoNotFire(t *testing.T) {
	a, conn, _ := newBatchMaintenanceApp(t)
	clock := time.Unix(2000000000, 0)
	var scanner *session.BatchTimeoutCheckin
	batchRuntimeConfigHook = func(cfg *BatchRuntimeConfig) {
		scanner = cfg.TimeoutCheckin.(*session.BatchTimeoutCheckin)
		scanner.SetClock(func() time.Time { return clock })
	}
	t.Cleanup(func() { batchRuntimeConfigHook = nil })
	require.NoError(t, a.installBatchRuntime(context.Background()))
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	defer func() { require.False(t, a.ShutdownWithResult().Forced) }()
	owner, err := a.Sessions.Create(context.Background(), "future timeout owner")
	require.NoError(t, err)
	store := session.NewBatchStore(conn)
	payloads := []sql.NullString{
		{String: `{"timeout":`, Valid: true},
		{String: `{"timeout":{"kind":"wake_only","seconds":2000000000}}`, Valid: true},
	}
	for i, payload := range payloads {
		callID := []string{"malformed-timeout", "future-timeout"}[i]
		_, err := store.Reserve(context.Background(), session.BatchReservation{
			Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "future-runner", ToolCallID: callID, InputHash: "hash-" + callID, Sequence: 1,
			Root: session.BatchReservationNode{
				Values:   db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"},
				Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none", TaskPayload: payload}}},
			},
		})
		require.NoError(t, err)
	}
	n, err := scanner.ScanOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	var notices, receipts int
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM session_notices WHERE kind='timeout_wake_only'").Scan(&notices))
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Zero(t, notices)
	require.Zero(t, receipts)
}

func TestBatchTimeoutCheckinShutdownStopsDueScanner(t *testing.T) {
	a, conn, _ := newBatchMaintenanceApp(t)
	clock := time.Unix(4102444800, 0)
	var spy *timeoutCheckinSpy
	batchRuntimeConfigHook = func(cfg *BatchRuntimeConfig) {
		scanner := cfg.TimeoutCheckin.(*session.BatchTimeoutCheckin)
		scanner.SetClock(func() time.Time { return clock })
		spy = &timeoutCheckinSpy{scanner: scanner}
		cfg.TimeoutCheckin = spy
	}
	t.Cleanup(func() { batchRuntimeConfigHook = nil })
	owner, err := a.Sessions.Create(context.Background(), "shutdown timeout owner")
	require.NoError(t, err)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error {
		require.Greater(t, spy.stops.Load(), int32(0))
		select {
		case <-a.batchTimeoutWake.done:
		default:
			t.Fatal("dispatcher not joined before writer release")
		}
		return nil
	}
	defer a.ShutdownWithResult()
	callID := "shutdown-timeout-call"
	payload := `{"tool":"bash","tool_call_id":"` + callID + `","session_id":"` + owner.ID + `","timeout":{"kind":"wake_only","seconds":5}}`
	_, err = session.NewBatchStore(conn).Reserve(context.Background(), session.BatchReservation{
		Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "shutdown-runner", ToolCallID: callID, InputHash: "shutdown-hash", Sequence: 1,
		Root: session.BatchReservationNode{
			Values:   db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none", TaskPayload: sql.NullString{String: payload, Valid: true}}}},
		},
	})
	require.NoError(t, err)
	result := a.ShutdownWithResult()
	require.False(t, result.Forced)
	require.EqualValues(t, 1, spy.starts.Load())
	require.Greater(t, spy.stops.Load(), int32(0))
	n, err := spy.scanner.ScanOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	var notices, receipts int
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM session_notices WHERE kind='timeout_wake_only'").Scan(&notices))
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Zero(t, notices)
	require.Zero(t, receipts)
}
