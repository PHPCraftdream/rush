package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBatchTimeoutCheckinStopLifecycle(t *testing.T) {
	store, conn, ctx := batchTimeoutCheckinStores(t)
	batchTimeoutCheckinReserve(t, store, ctx, "call-stop")
	svc := NewBatchTimeoutCheckin(conn)
	fixed := time.Now().Add(time.Minute)
	svc.SetClock(func() time.Time { return fixed })
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop-before-Start blocked")
	}
	n, err := svc.ScanOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, batchTimeoutCheckinNoticeCount(t, conn))
	require.Zero(t, batchTimeoutCheckinReceiptCount(t, conn))
}
