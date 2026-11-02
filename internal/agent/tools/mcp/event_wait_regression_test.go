package mcp

import (
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestTransactionalEventWaitAcceptsDelayedEvent(t *testing.T) {
	t.Run("delayed event", func(t *testing.T) {
		events := make(chan pubsub.Event[Event], 1)
		time.AfterFunc(1500*time.Millisecond, func() {
			events <- pubsub.Event[Event]{
				Type: pubsub.UpdatedEvent,
				Payload: Event{
					Type:  EventStateChanged,
					Name:  "delayed",
					State: StateConnected,
				},
			}
		})

		requireTransactionalEvent(t, events, pubsub.UpdatedEvent, "delayed", StateConnected)
	})
}

func TestTransactionalEventWaitReportsMissingEvent(t *testing.T) {
	previousTimeout := transactionalEventTimeout
	transactionalEventTimeout = time.Millisecond
	t.Cleanup(func() { transactionalEventTimeout = previousTimeout })

	_, err := waitForTransactionalEvent(make(chan pubsub.Event[Event]), pubsub.DeletedEvent, "missing")
	require.EqualError(t, err, fmt.Sprintf("missing %s event for MCP server %q", pubsub.DeletedEvent, "missing"))
}
