package server

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAPIEventLogIDsRingAndReplayWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	log := NewAPIEventLog(func() time.Time { return now })
	log.maxPerSession = 3
	first, ok := log.Publish("s", "message", map[string]int{"n": 0})
	if !ok {
		t.Fatal("publish failed")
	}
	for i := 1; i <= 4; i++ {
		now = now.Add(time.Second)
		if _, ok := log.Publish("s", "message", map[string]int{"n": i}); !ok {
			t.Fatal("publish failed")
		}
	}
	events, oldest, retained := log.Replay("s", first.ID)
	if !retained || oldest != 3 || len(events) != 3 {
		t.Fatalf("replay retained=%v oldest=%d events=%d", retained, oldest, len(events))
	}
	wantIDs := []uint64{3, 4, 5}
	for i := range events {
		if events[i].ID != wantIDs[i] {
			t.Fatalf("event %d id=%d, want %d", i, events[i].ID, wantIDs[i])
		}
	}
	later, _, _ := log.Replay("s", events[0].ID)
	if len(later) != 2 || later[0].ID != 4 || later[1].ID != 5 {
		t.Fatalf("exclusive replay boundary=%v", later)
	}
	if _, _, retained := log.Replay("missing", 0); retained {
		t.Fatal("missing session unexpectedly retained")
	}
}

func TestAPIEventLogLRUEviction(t *testing.T) {
	log := NewAPIEventLog(nil)
	for i := 0; i < apiEventMaxSessions; i++ {
		log.Publish(fmt.Sprintf("session-%03d", i), "session", i)
	}
	log.Replay("session-000", 0)
	log.Publish("new", "session", 1)
	if log.SessionCount() != apiEventMaxSessions {
		t.Fatalf("session count=%d", log.SessionCount())
	}
	if _, _, retained := log.Replay("session-001", 0); retained {
		t.Fatal("least recently used session was retained")
	}
	if _, _, retained := log.Replay("session-000", 0); !retained {
		t.Fatal("recently used session was evicted")
	}
}

func TestAPIEventLogSubscriberOverflowAndRelease(t *testing.T) {
	log := NewAPIEventLog(nil)
	ch, overflow, unsubscribe := log.Subscribe("s")
	for i := 0; i < apiEventSubscriberCap; i++ {
		if _, ok := log.Publish("s", "message", i); !ok {
			t.Fatal("publish failed")
		}
	}
	if _, ok := log.Publish("s", "message", "overflow"); !ok {
		t.Fatal("overflow publish failed")
	}
	select {
	case <-overflow:
	default:
		t.Fatal("overflow signal not delivered")
	}
	if log.SubscriberCount("s") != 0 {
		t.Fatal("overflowed subscriber remains registered")
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("subscriber event channel closed unexpectedly")
		}
	default:
		t.Fatal("buffer unexpectedly empty")
	}
	unsubscribe()
	ch2, _, unsubscribe2 := log.Subscribe("s")
	if log.SubscriberCount("s") != 1 {
		t.Fatal("subscriber not registered")
	}
	unsubscribe2()
	if log.SubscriberCount("s") != 0 {
		t.Fatal("subscriber not released")
	}
	_ = ch2
}

func TestAPIEventLogConcurrentPublishAndSubscribe(t *testing.T) {
	log := NewAPIEventLog(nil)
	const workers = 8
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ch, _, unsubscribe := log.Subscribe("shared")
			defer unsubscribe()
			for i := 0; i < 100; i++ {
				log.Publish("shared", "run", map[string]int{"worker": worker, "index": i})
				select {
				case <-ch:
				default:
				}
			}
		}(worker)
	}
	wg.Wait()
	if log.SubscriberCount("shared") != 0 {
		t.Fatal("concurrent subscribers leaked")
	}
}

func TestAPIRunRegistryObserverFiresOnlyOnSuccessfulTransition(t *testing.T) {
	registry := NewAPIRunRegistry(nil)
	observed := make(chan APIRun, 2)
	registry.setObserver(func(run APIRun) { observed <- run })
	run, err := registry.Create("session-one", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Transition(run.ID, APIRunSucceeded, "", "", ""); err == nil {
		t.Fatal("illegal transition succeeded")
	}
	select {
	case got := <-observed:
		t.Fatalf("failed transition observed: %+v", got)
	default:
	}
	if _, err := registry.Transition(run.ID, APIRunRunning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-observed:
		if got.SessionID != "session-one" || got.State != APIRunRunning {
			t.Fatalf("observer received %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observer did not receive transition")
	}
}
