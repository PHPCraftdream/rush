// Web propagation for the session title.
//
// The agent's background title generation (internal/agent's generateTitle)
// has no access to this package's Hub — it writes the session row through
// session.Service.Rename. The ONLY path that write has to a browser tab is
// the pubsub bridge in events.go, which forwards every session-service
// UpdatedEvent as a session_updated broadcast (which useWS.ts applies via
// upsertSession, so the sidebar row and the tab both re-render the title
// without a reload). Rename used to publish nothing, which is why a
// generated title was invisible to every open tab until the next
// sessions_list poll.
//
// This test drives the REAL bridge, not a re-implementation of it.
package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTitleSaveIsBroadcastToEveryTab(t *testing.T) {
	// Cannot use t.Parallel(): newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "New Session")
	require.NoError(t, err)

	// Capture hub broadcasts, following handlers_sessions_test.go's
	// established hub.Run + register pattern.
	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	// The production bridge itself.
	subscribeAndBroadcast(ctx, a, hub)

	// Stand in for what the agent's title goroutine does when its fast-model
	// call returns: one Rename on the session row.
	//
	// Retried: pubsub.Broker drops events published BEFORE Subscribe
	// registers the subscriber (broker.go registers subs inside Subscribe,
	// no replay buffer), and subscribeAndBroadcast runs asynchronously, so
	// under full-package load the bridge goroutine can lose the first
	// Rename's UpdatedEvent. This race is a fixture property, not a
	// production one: the real server subscribes at startup, long before
	// any title write. Each retry re-publishes via the same Rename.
	var updated *session.Session
	for attempt := 0; attempt < 5 && updated == nil; attempt++ {
		require.NoError(t, a.Sessions.Rename(ctx, sess.ID, "A Generated Title"))
		deadline := time.Now().Add(1 * time.Second)
		for updated == nil && time.Now().Before(deadline) {
			select {
			case raw := <-client.send:
				var env WSMessage
				require.NoError(t, json.Unmarshal(raw, &env))
				if env.Type != EventSessionUpdated {
					continue
				}
				var s session.Session
				require.NoError(t, json.Unmarshal(env.Payload, &s))
				if s.ID == sess.ID {
					updated = &s
				}
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	require.NotNil(t, updated,
		"a title save must be broadcast as session_updated — that broadcast is the only thing that updates the tab without a reload")
	require.Equal(t, "A Generated Title", updated.Title,
		"the broadcast must carry the renamed row so every open tab renders the new title")
}
