package server

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type apiEventsSubscriberCounter interface{ GetSubscriberCount() int }

func TestServeAPIStopReleasesEventFeederSubscriptions(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	messages, ok := app.Messages.(apiEventsSubscriberCounter)
	if !ok {
		t.Fatal("message service does not expose a subscriber count")
	}
	sessions, ok := app.Sessions.(apiEventsSubscriberCounter)
	if !ok {
		t.Fatal("session service does not expose a subscriber count")
	}
	baseMessages, baseSessions := messages.GetSubscriberCount(), sessions.GetSubscriberCount()

	api, err := NewServeAPI(ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}, app, config.GlobalAPITokenStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for messages.GetSubscriberCount() <= baseMessages || sessions.GetSubscriberCount() <= baseSessions {
		if time.Now().After(deadline) {
			t.Fatal("event feeder never subscribed to the App pubsub")
		}
		time.Sleep(time.Millisecond)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := api.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := messages.GetSubscriberCount(); got != baseMessages {
		t.Errorf("message subscribers after Stop = %d, want %d", got, baseMessages)
	}
	if got := sessions.GetSubscriberCount(); got != baseSessions {
		t.Errorf("session subscribers after Stop = %d, want %d", got, baseSessions)
	}
}
