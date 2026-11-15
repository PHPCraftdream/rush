package nettransport

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResponsesWSContextCancellationClosesConnectionAndWaitsForCallback(t *testing.T) {
	wire := &responsesWSCancelTestWire{readStarted: make(chan struct{}), unblock: make(chan struct{})}
	conn := newResponsesWSTestConn(wire, 64, 128)
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(context.Background(), []byte("request")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.ReceiveRaw(ctx)
		done <- err
	}()
	select {
	case <-wire.readStarted:
	case <-time.After(time.Second):
		t.Fatal("receive did not start")
	}
	cancel()
	select {
	case err := <-done:
		var receiveErr *ResponsesWSError
		require.ErrorAs(t, err, &receiveErr)
		require.True(t, receiveErr.NoReplay())
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock read")
	}
	require.True(t, wire.isClosed())
}

type responsesWSCancelTestWire struct {
	readStarted chan struct{}
	unblock     chan struct{}
	closeOnce   sync.Once
	closed      chan struct{}
}

func (*responsesWSCancelTestWire) WriteMessage(int, []byte) error { return nil }
func (w *responsesWSCancelTestWire) ReadMessage() (int, []byte, error) {
	close(w.readStarted)
	<-w.unblock
	return 0, nil, net.ErrClosed
}
func (*responsesWSCancelTestWire) SetWriteDeadline(time.Time) error { return nil }
func (*responsesWSCancelTestWire) SetReadDeadline(time.Time) error  { return nil }
func (*responsesWSCancelTestWire) SetReadLimit(int64)               {}
func (w *responsesWSCancelTestWire) Close() error {
	w.closeOnce.Do(func() {
		w.closed = make(chan struct{})
		close(w.unblock)
		close(w.closed)
	})
	return nil
}

func (w *responsesWSCancelTestWire) isClosed() bool {
	return w.closed != nil
}
