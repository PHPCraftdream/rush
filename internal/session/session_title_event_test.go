// The title-change event: session.Service.Rename must publish an
// UpdatedEvent, because that publish is the ONLY path a title written by the
// agent's background title generation has to the browser — the agent has no
// access to the web server's Hub, and internal/server/events.go forwards
// every UpdatedEvent as a session_updated broadcast. Rename used to be the
// one session mutation that published nothing, so a generated title sat
// invisible in the DB until the next 5s sessions_list poll while a
// hand-rename updated the tab instantly.
package session

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRename_PublishesUpdatedEventWithNewTitle(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "New Session")
	require.NoError(t, err)

	// Subscribe BEFORE the rename so the event cannot be missed.
	events := svc.Subscribe(ctx)

	require.NoError(t, svc.Rename(ctx, sess.ID, "A Generated Title"))

	var got *pubsub.Event[Session]
	deadline := time.Now().Add(2 * time.Second)
	for got == nil && time.Now().Before(deadline) {
		select {
		case ev := <-events:
			if ev.Type == pubsub.UpdatedEvent && ev.Payload.ID == sess.ID {
				got = &ev
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.NotNil(t, got, "Rename must publish an UpdatedEvent so the web bridge can broadcast the new title")
	assert.Equal(t, pubsub.UpdatedEvent, got.Type)
	assert.Equal(t, "A Generated Title", got.Payload.Title,
		"the published event must carry the renamed row, not a stale snapshot")

	// And the column itself must of course carry it.
	stored, err := svc.Get(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "A Generated Title", stored.Title)
}
