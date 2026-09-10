package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestGetOrRenewClientWaitsForDetachedRenewalWinner(t *testing.T) {
	for _, test := range []struct {
		name       string
		cancelWait bool
	}{
		{name: "winner", cancelWait: false},
		{name: "canceled waiter", cancelWait: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const serverName = "renewal-waiter"
			oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
			oldServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
					if method == "ping" {
						return nil, errors.New("force renewal")
					}
					return next(ctx, method, request)
				}
			})
			oldTransport, oldClientTransport := mcp.NewInMemoryTransports()
			oldServerSession, err := oldServer.Connect(context.Background(), oldTransport, nil)
			require.NoError(t, err)
			defer oldServerSession.Close()
			oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
				Connect(context.Background(), oldClientTransport, nil)
			require.NoError(t, err)

			newServer := mcp.NewServer(&mcp.Implementation{Name: "new-server"}, nil)
			mcp.AddTool(newServer, &mcp.Tool{Name: "winner-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
			newHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newServer }, nil)
			requestStarted := make(chan struct{})
			releaseRequest := make(chan struct{})
			var requestOnce sync.Once
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestOnce.Do(func() { close(requestStarted) })
				select {
				case <-releaseRequest:
				case <-r.Context().Done():
					return
				}
				newHandler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()

			store := persistedMCPStore(t, serverName, httpServer.URL, false)
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close(context.Background())) }()
			oldSession := &ClientSession{ClientSession: oldClientSession}
			sessions.Set(serverName, oldSession)
			setState(serverName, StateConnected, nil, oldSession, Counts{})

			lease := serverLeaseFor(serverName)
			waitStarted := make(chan struct{})
			var waitOnce sync.Once
			lease.mu.Lock()
			lease.renewalWaitHook = func() { waitOnce.Do(func() { close(waitStarted) }) }
			lease.mu.Unlock()
			defer func() {
				lease.mu.Lock()
				lease.renewalWaitHook = nil
				lease.mu.Unlock()
			}()

			winnerDone := make(chan *clientLease, 1)
			winnerErr := make(chan error, 1)
			go func() {
				client, renewErr := getOrRenewClient(context.Background(), store, serverName)
				if renewErr != nil {
					winnerErr <- renewErr
					return
				}
				winnerDone <- client
			}()
			select {
			case <-requestStarted:
			case err := <-winnerErr:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("renewal winner did not reach blocked replacement initialization")
			}
			lease.mu.Lock()
			renewing := lease.renewing
			lease.mu.Unlock()
			_, sessionExists := sessions.Get(serverName)
			require.True(t, renewing)
			require.False(t, sessionExists, "renewal must detach the old session before replacement publication")

			waiterCtx := context.Background()
			cancelWaiter := func() {}
			if test.cancelWait {
				waiterCtx, cancelWaiter = context.WithCancel(context.Background())
			}
			// Every t.Fatal below returns without reaching the explicit
			// cancelWaiter() call in the cancelWait branch, which leaked the
			// context on each failure path. Cancelling twice is a no-op, and
			// so is cancelling the func(){} default.
			defer cancelWaiter()
			waiterDone := make(chan *clientLease, 1)
			waiterErr := make(chan error, 1)
			go func() {
				client, waitErr := getOrRenewClient(waiterCtx, store, serverName)
				if waitErr != nil {
					waiterErr <- waitErr
					return
				}
				waiterDone <- client
			}()
			select {
			case <-waitStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("renewal waiter did not enter the event-driven renewDone wait")
			}
			select {
			case err := <-waiterErr:
				t.Fatalf("renewal waiter returned before renewDone: %v", err)
			case client := <-waiterDone:
				client.close()
				t.Fatal("renewal waiter returned before renewDone")
			default:
			}

			if test.cancelWait {
				cancelWaiter()
				select {
				case err := <-waiterErr:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(2 * time.Second):
					t.Fatal("canceled renewal waiter did not exit")
				}
			} else {
				close(releaseRequest)
				var winner *clientLease
				select {
				case winner = <-winnerDone:
				case err := <-winnerErr:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("renewal winner did not publish")
				}
				defer winner.close()
				select {
				case waiter := <-waiterDone:
					defer waiter.close()
					require.Same(t, winner.session, waiter.session)
				case err := <-waiterErr:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("renewal waiter did not use the published winner")
				}
			}
			if test.cancelWait {
				close(releaseRequest)
				select {
				case err := <-winnerErr:
					require.NoError(t, err)
				case winner := <-winnerDone:
					winner.close()
				case <-time.After(2 * time.Second):
					t.Fatal("renewal winner did not finish after waiter cancellation")
				}
			}
		})
	}
}
