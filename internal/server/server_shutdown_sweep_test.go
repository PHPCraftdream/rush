package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// shutdownStacks returns the stacks of the goroutines inside the HTTP server
// and the Server's shutdown path, for the "server did not shut down" report.
func shutdownStacks() string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "net/http.(*Server)") || strings.Contains(g, "internal/server.(*Server)") || strings.Contains(g, "net/http.(*conn)") {
			keep = append(keep, g)
		}
	}
	out := fmt.Sprintf("goroutines in the HTTP server / Server shutdown path: %d\n%s", len(keep), strings.Join(keep, "\n\n"))
	if len(out) > 12000 {
		out = out[:12000] + "\n...(truncated)"
	}
	return out
}

// TestShutdownHTTPClosesAConnectionRegisteredAfterTheFirstSweep pins the
// shutdown bound. A connection accepted just before the listener closed is
// registered (StateNew) only after Start's one sweep of quiescent connections;
// net/http treats a StateNew connection as active for its first 5 s, so a
// client's speculative dial that never sends a request would hold Shutdown for
// that long and a caller waiting 5 s for Start to return (startPolicyServer's
// cleanup) would give up. The hook below delays the registration exactly so.
//
// Revert-check: make shutdownHTTP call srv.Shutdown(ctx) alone: the test fails
// with "Shutdown did not return" (it takes the full 5 s).
func TestShutdownHTTPClosesAConnectionRegisteredAfterTheFirstSweep(t *testing.T) {
	t.Parallel()

	s := &Server{}
	entered := make(chan struct{})
	gate := make(chan struct{})
	srv := &http.Server{
		Handler: http.NotFoundHandler(),
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateNew {
				close(entered)
				<-gate // the connection is registered late
			}
			s.trackConnection(c, state)
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never accepted the connection")
	}

	// What Start does first: close the listener, then sweep. The accepted
	// connection is not registered yet, so the sweep finds nothing.
	_ = ln.Close()
	s.closeQuiescentConnections()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.shutdownHTTP(shutdownCtx, srv) }()
	close(gate) // now the connection is registered, as StateNew

	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown did not return: a late-registered StateNew connection holds it for net/http's 5 s\n%s", shutdownStacks())
	}
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestShutdownStacksListsServerGoroutines(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	require.Eventually(t, func() bool { return strings.Contains(shutdownStacks(), "net/http.(*Server).Serve") }, 5*time.Second, 5*time.Millisecond)
}
