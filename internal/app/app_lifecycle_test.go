package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type shutdownPumpCoordinator struct {
	broker   *pubsub.Broker[any]
	entered  chan struct{}
	terminal chan struct{}
	finished chan struct{}
	release  <-chan struct{}
}

func (c *shutdownPumpCoordinator) Run(ctx context.Context, _ session.SessionAgentCallData) (*any, error) {
	close(c.entered)
	<-ctx.Done()
	c.broker.Publish(pubsub.CreatedEvent, "pump terminal event")
	close(c.terminal)
	if c.release != nil {
		<-c.release
	}
	close(c.finished)
	return nil, nil
}

// TestAppShutdown_PumpStopPrecedesBrokerEOF proves both shutdown paths leave
// App brokers open for the pump's bounded Stop window. The coordinator emits
// its terminal event only after Stop cancels the worker context, so closing a
// broker first would make the event disappear.
func TestAppShutdown_PumpStopPrecedesBrokerEOF(t *testing.T) {
	for _, test := range []struct {
		name   string
		forced bool
	}{
		{name: "graceful"},
		{name: "forced", forced: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateAppNewTestEnv(t)

			dataDir := t.TempDir()
			store, err := config.Init(dataDir, dataDir, false)
			require.NoError(t, err)
			conn, err := db.Connect(context.Background(), dataDir)
			require.NoError(t, err)
			application, err := New(context.Background(), conn, store, SkipAgentSetup())
			require.NoError(t, err)
			t.Cleanup(application.Shutdown)

			events := application.Events(context.Background())
			release := make(chan struct{})
			coordinator := &shutdownPumpCoordinator{
				broker:   application.events,
				entered:  make(chan struct{}),
				terminal: make(chan struct{}),
				finished: make(chan struct{}),
				release:  release,
			}
			if !test.forced {
				close(release)
			}
			pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
				Sessions:    application.Sessions,
				Coordinator: coordinator,
				TestTick:    func() time.Duration { return time.Millisecond },
			})
			application.RunQueuePump = pump
			pump.Start()

			sess, err := application.Sessions.Create(context.Background(), "shutdown-order")
			require.NoError(t, err)
			callData, err := json.Marshal(map[string]any{
				"SessionID": sess.ID,
				"Prompt":    "shutdown-order",
			})
			require.NoError(t, err)
			require.NoError(t, application.Sessions.EnqueueRunQueueEntry(
				context.Background(), "shutdown-order", sess.ID, callData))

			select {
			case <-coordinator.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("pump did not start the test call")
			}

			result := application.ShutdownWithResult()
			require.Equal(t, test.forced, result.Forced)

			select {
			case event, ok := <-events:
				require.True(t, ok, "terminal event must arrive before broker EOF")
				require.Equal(t, "pump terminal event", event.Payload)
			case <-time.After(time.Second):
				t.Fatal("pump terminal event was lost before broker EOF")
			}
			requireClosed(t, events)

			if test.forced {
				close(release)
				select {
				case <-coordinator.finished:
				case <-time.After(time.Second):
					t.Fatal("forced pump worker did not finish after release")
				}
				// Join the worker before forcibly reclaiming the pooled DB handle.
				// The first Stop returned forced while this worker was intentionally
				// blocked; the second Stop is now a bounded, already-unblocked join.
				require.False(t, pump.Stop())
				require.NoError(t, db.ReleaseAll(dataDir))
			}
		})
	}
}
