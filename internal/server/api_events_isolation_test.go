package server

import (
	"fmt"
	"testing"
	"time"
)

func TestAPIEventLogSubscribersAreIsolatedAcrossSessionsAndEventTypes(t *testing.T) {
	log := NewAPIEventLog(nil)
	aEvents, _, unsubscribeA := log.Subscribe("session-a")
	defer unsubscribeA()
	bEvents, _, unsubscribeB := log.Subscribe("session-b")
	defer unsubscribeB()

	for _, eventType := range []string{"message", "session", "run"} {
		if _, ok := log.Publish("session-b", eventType, map[string]string{"session": "session-b"}); !ok {
			t.Fatalf("publish %s event for B failed", eventType)
		}
	}
	for _, eventType := range []string{"message", "session", "run"} {
		if _, ok := log.Publish("session-a", eventType, map[string]string{"session": "session-a"}); !ok {
			t.Fatalf("publish %s event for A failed", eventType)
		}
	}

	for _, wantType := range []string{"message", "session", "run"} {
		select {
		case event := <-aEvents:
			if event.Type != wantType || string(event.Data) != `{"session":"session-a"}` {
				t.Fatalf("A received event %+v, want %s for A", event, wantType)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for A %s event", wantType)
		}
	}
	for _, wantType := range []string{"message", "session", "run"} {
		select {
		case event := <-bEvents:
			if event.Type != wantType || string(event.Data) != `{"session":"session-b"}` {
				t.Fatalf("B received event %+v, want %s for B", event, wantType)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for B %s event", wantType)
		}
	}
	select {
	case event := <-aEvents:
		t.Fatalf("A received unexpected cross-session event %+v", event)
	default:
	}
	select {
	case event := <-bEvents:
		t.Fatalf("B received unexpected cross-session event %+v", event)
	default:
	}
}

func TestAPIEventLogSlowSubscriberOverflowDoesNotBlockOtherSubscribers(t *testing.T) {
	log := NewAPIEventLog(nil)
	_, overflow, unsubscribeSlow := log.Subscribe("session")
	defer unsubscribeSlow()
	fastEvents, _, unsubscribeFast := log.Subscribe("session")
	defer unsubscribeFast()

	consumed := make(chan struct{})
	ack := make(chan struct{})
	go func() {
		defer close(consumed)
		for i := 0; i < apiEventSubscriberCap+1; i++ {
			select {
			case <-fastEvents:
				ack <- struct{}{}
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()

	published := make(chan error, 1)
	go func() {
		for i := 0; i < apiEventSubscriberCap+1; i++ {
			if _, ok := log.Publish("session", "message", map[string]int{"n": i}); !ok {
				published <- fmt.Errorf("publish %d failed", i)
				return
			}
			select {
			case <-ack:
			case <-time.After(10 * time.Second):
				published <- fmt.Errorf("timed out waiting for fast subscriber to consume event %d", i)
				return
			}
		}
		published <- nil
	}()

	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publisher blocked on the full subscriber")
	}
	select {
	case <-consumed:
	case <-time.After(10 * time.Second):
		t.Fatal("second subscriber did not consume every published event")
	}
	select {
	case <-overflow:
	case <-time.After(10 * time.Second):
		t.Fatal("full subscriber was not dropped")
	}
	if got := log.SubscriberCount("session"); got != 1 {
		t.Fatalf("subscriber count after overflow = %d, want 1", got)
	}
}

// When every retained session has a live stream the cap is soft: a new
// session is still retained and delivered (no silent loss, no nil channel),
// and sessions without streams become evictable again afterwards.
func TestAPIEventLogCapIsSoftWhenEverySessionHasAStream(t *testing.T) {
	log := NewAPIEventLog(nil)
	var unsubscribers []func()
	for i := 0; i < apiEventMaxSessions; i++ {
		_, _, unsubscribe := log.Subscribe(fmt.Sprintf("session-%d", i))
		unsubscribers = append(unsubscribers, unsubscribe)
	}
	if _, ok := log.Publish("another-session", "message", "kept"); !ok {
		t.Fatal("publish was dropped although every retained session has a stream")
	}
	ch, overflow, unsubscribeExtra := log.Subscribe("yet-another-session")
	if ch == nil || overflow == nil {
		t.Fatal("subscribe returned nil channels for a new session at the cap")
	}
	unsubscribeExtra()
	// "another-session" has no stream, so it is evicted for the next newcomer; every
	// session with a stream is kept: the overshoot is exactly one over the cap.
	if got := log.SessionCount(); got != apiEventMaxSessions+1 {
		t.Fatalf("session count = %d, want %d (soft cap)", got, apiEventMaxSessions+1)
	}
	for _, unsubscribe := range unsubscribers {
		unsubscribe()
	}
	if _, ok := log.Publish("after-release", "message", "x"); !ok {
		t.Fatal("publish failed after the streams were released")
	}
	if got := log.SessionCount(); got > apiEventMaxSessions+1 {
		t.Fatalf("session count grew to %d after release", got)
	}
}
