package app

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type timeoutCheckinSpy struct {
	scanner *session.BatchTimeoutCheckin
	starts  atomic.Int32
	stops   atomic.Int32
}

func (s *timeoutCheckinSpy) Start() { s.starts.Add(1); s.scanner.Start() }
func (s *timeoutCheckinSpy) Stop()  { s.stops.Add(1); s.scanner.Stop() }

func TestBatchTimeoutCheckinInstalledProductionPath(t *testing.T) {
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
	owner, err := a.Sessions.Create(context.Background(), "timeout check-in owner")
	require.NoError(t, err)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	runtime := a.batchRuntime
	require.NotNil(t, runtime)
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	defer a.ShutdownWithResult()
	require.EqualValues(t, 1, spy.starts.Load(), "runtime Start arms installed scanner")
	require.NoError(t, a.installBatchRuntime(context.Background()))
	require.Same(t, runtime, a.batchRuntime)
	require.EqualValues(t, 1, spy.starts.Load())

	store := session.NewBatchStore(conn)
	callID := "timeout-install-call"
	payload := `{"tool":"bash","tool_call_id":"` + callID + `","session_id":"` + owner.ID + `","timeout":{"kind":"wake_only","seconds":5}}`
	_, err = store.Reserve(context.Background(), session.BatchReservation{
		Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "timeout-install-runner", ToolCallID: callID,
		InputHash: "timeout-install-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none", TaskPayload: sql.NullString{String: payload, Valid: true}}}}},
	})
	require.NoError(t, err)
	woke := make(chan string, 2)
	var wakeCount atomic.Int32
	a.batchTimeoutWake.Attach(wakeDispatchTestCoordinator{wake: func(ctx context.Context, got string, fact bool) error {
		if got == "barrier" {
			select {
			case woke <- got:
			default:
			}
			return nil
		}
		if fact {
			wakeCount.Add(1)
		}
		select {
		case woke <- got:
		default:
		}
		return nil
	}})
	n, err := spy.scanner.ScanOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	select {
	case got := <-woke:
		require.Equal(t, owner.ID, got)
	case <-time.After(time.Second):
		t.Fatal("owner wake not dispatched")
	}
	var kind, delivery, noticeOwner, noticeCall string
	var wake int
	var notices, receipts int
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*), kind, wake, delivery, owner, job_tool_call_id FROM session_notices WHERE kind='timeout_wake_only'").Scan(&notices, &kind, &wake, &delivery, &noticeOwner, &noticeCall))
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Equal(t, 1, notices)
	require.Equal(t, "timeout_wake_only", kind)
	require.Equal(t, 1, wake)
	require.Equal(t, "pending", delivery)
	require.Equal(t, owner.ID, noticeOwner)
	require.Equal(t, callID, noticeCall)
	require.Equal(t, 1, receipts)
	n, err = spy.scanner.ScanOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM session_notices WHERE kind='timeout_wake_only'").Scan(&notices))
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Equal(t, 1, notices)
	require.Equal(t, 1, receipts)
	a.batchTimeoutWake.Wake(context.Background(), "barrier")
	select {
	case got := <-woke:
		require.Equal(t, "barrier", got)
	case <-time.After(time.Second):
		t.Fatal("dispatch barrier did not complete")
	}
	require.EqualValues(t, 1, wakeCount.Load())
	shutdown := a.ShutdownWithResult()
	require.False(t, shutdown.Forced)
	select {
	case <-a.batchTimeoutWake.done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not join")
	}
	require.Greater(t, spy.stops.Load(), int32(0))
	require.EqualValues(t, 1, wakeCount.Load())
}

func TestBatchTimeoutCheckinAttachedToRealCoordinator(t *testing.T) {
	a := newFullNewApp(t)
	require.NotNil(t, a.batchTimeoutWake)
	require.NotNil(t, a.AgentCoordinator)
	a.batchTimeoutWake.mu.Lock()
	coord := a.batchTimeoutWake.coord
	a.batchTimeoutWake.mu.Unlock()
	require.Same(t, a.AgentCoordinator, coord)
	_, ok := a.AgentCoordinator.(agent.SessionWakeDispatcher)
	require.True(t, ok)
	require.False(t, a.ShutdownWithResult().Forced)
}

func TestBatchTimeoutCheckinShutdownLeavesDebtWithoutCoordinator(t *testing.T) {
	a, conn, _ := newBatchMaintenanceApp(t)
	clock := time.Unix(4102444800, 0)
	var scanner *session.BatchTimeoutCheckin
	batchRuntimeConfigHook = func(cfg *BatchRuntimeConfig) {
		scanner = cfg.TimeoutCheckin.(*session.BatchTimeoutCheckin)
		scanner.SetClock(func() time.Time { return clock })
	}
	t.Cleanup(func() { batchRuntimeConfigHook = nil })
	require.NoError(t, a.installBatchRuntime(context.Background()))
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	defer a.ShutdownWithResult()
	owner, err := a.Sessions.Create(context.Background(), "unattached timeout owner")
	require.NoError(t, err)
	callID := "unattached-timeout-call"
	payload := `{"tool":"bash","tool_call_id":"` + callID + `","session_id":"` + owner.ID + `","timeout":{"kind":"wake_only","seconds":5}}`
	_, err = session.NewBatchStore(conn).Reserve(context.Background(), session.BatchReservation{
		Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "unattached-runner", ToolCallID: callID, InputHash: "unattached-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none", TaskPayload: sql.NullString{String: payload, Valid: true}}}}},
	})
	require.NoError(t, err)
	n, err := scanner.ScanOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	result := a.ShutdownWithResult()
	require.False(t, result.Forced)
	select {
	case <-a.batchTimeoutWake.done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not join")
	}
	var notices, receipts int
	var wake int
	var delivery, noticeOwner, noticeCall string
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*), wake, delivery, owner, job_tool_call_id FROM session_notices WHERE kind='timeout_wake_only'").Scan(&notices, &wake, &delivery, &noticeOwner, &noticeCall))
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT count(*) FROM batch_timeout_checkins").Scan(&receipts))
	require.Equal(t, 1, notices)
	require.Equal(t, 1, wake)
	require.Equal(t, "pending", delivery)
	require.Equal(t, owner.ID, noticeOwner)
	require.Equal(t, callID, noticeCall)
	require.Equal(t, 1, receipts)
}

// TestBatchTimeoutCheckinLibraryModeConstructsNothing: library-mode New
// (SkipAgentSetup) constructs no batch runtime, no wake dispatcher and no
// check-in scanner. Obsolete flag-OFF contract translated to the
// library-mode shape.
func TestBatchTimeoutCheckinLibraryModeConstructsNothing(t *testing.T) {
	a := newFullNewAppSkipAgentSetup(t)
	require.Nil(t, a.batchRuntime)
	require.Nil(t, a.batchTimeoutWake)
}
