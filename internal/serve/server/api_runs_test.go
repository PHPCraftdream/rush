package server

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAPIRunRegistryTransitions(t *testing.T) {
	states := []APIRunState{APIRunQueued, APIRunRunning, APIRunSucceeded, APIRunFailed, APIRunCancelled, APIRunAwaitingAnswer}
	cases := []struct {
		from, to APIRunState
		legal    bool
	}{
		{APIRunQueued, APIRunQueued, false},
		{APIRunQueued, APIRunRunning, true},
		{APIRunQueued, APIRunSucceeded, false},
		{APIRunQueued, APIRunFailed, true},
		{APIRunQueued, APIRunCancelled, true},
		{APIRunQueued, APIRunAwaitingAnswer, false},
		{APIRunRunning, APIRunQueued, false},
		{APIRunRunning, APIRunRunning, false},
		{APIRunRunning, APIRunSucceeded, true},
		{APIRunRunning, APIRunFailed, true},
		{APIRunRunning, APIRunCancelled, true},
		{APIRunRunning, APIRunAwaitingAnswer, true},
		{APIRunSucceeded, APIRunQueued, false},
		{APIRunSucceeded, APIRunRunning, false},
		{APIRunSucceeded, APIRunSucceeded, false},
		{APIRunSucceeded, APIRunFailed, false},
		{APIRunSucceeded, APIRunCancelled, false},
		{APIRunSucceeded, APIRunAwaitingAnswer, false},
		{APIRunFailed, APIRunQueued, false},
		{APIRunFailed, APIRunRunning, false},
		{APIRunFailed, APIRunSucceeded, false},
		{APIRunFailed, APIRunFailed, false},
		{APIRunFailed, APIRunCancelled, false},
		{APIRunFailed, APIRunAwaitingAnswer, false},
		{APIRunCancelled, APIRunQueued, false},
		{APIRunCancelled, APIRunRunning, false},
		{APIRunCancelled, APIRunSucceeded, false},
		{APIRunCancelled, APIRunFailed, false},
		{APIRunCancelled, APIRunCancelled, false},
		{APIRunCancelled, APIRunAwaitingAnswer, false},
		{APIRunAwaitingAnswer, APIRunQueued, false},
		{APIRunAwaitingAnswer, APIRunRunning, true},
		{APIRunAwaitingAnswer, APIRunSucceeded, false},
		{APIRunAwaitingAnswer, APIRunFailed, true},
		{APIRunAwaitingAnswer, APIRunCancelled, true},
		{APIRunAwaitingAnswer, APIRunAwaitingAnswer, false},
	}
	if len(cases) != len(states)*len(states) {
		t.Fatalf("transition matrix has %d cases", len(cases))
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_to_%s", tc.from, tc.to), func(t *testing.T) {
			if got := validAPIRunTransition(tc.from, tc.to); got != tc.legal {
				t.Fatalf("validAPIRunTransition(%q,%q)=%v want %v", tc.from, tc.to, got, tc.legal)
			}
		})
	}
}

func TestAPIRunRegistryBoundsEvictionAndTTL(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewAPIRunRegistry(func() time.Time { return now })
	for i := 0; i < apiRunMaxRetained; i++ {
		run, err := r.Create("s", "t")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Transition(run.ID, APIRunFailed, "", "", ""); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Nanosecond)
	}
	oldest := ""
	for id, entry := range r.runs {
		if oldest == "" || entry.sequence < r.runs[oldest].sequence {
			oldest = id
		}
	}
	added, err := r.Create("s", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(oldest); ok {
		t.Fatal("oldest finished run was not evicted")
	}
	fullRegistry := NewAPIRunRegistry(nil)
	for i := 0; i < apiRunMaxRetained; i++ {
		if _, err := fullRegistry.Create("active", "t"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fullRegistry.Create("overflow", "t"); !errors.Is(err, ErrAPIRunRegistryFull) {
		t.Fatalf("full registry error=%v", err)
	}
	if _, err := r.Transition(added.ID, APIRunFailed, "", "", ""); err != nil {
		t.Fatal(err)
	}
	ttlNow := now
	ttlRegistry := NewAPIRunRegistry(func() time.Time { return ttlNow })
	ttlRun, err := ttlRegistry.Create("ttl", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ttlRegistry.Transition(ttlRun.ID, APIRunFailed, "", "", ""); err != nil {
		t.Fatal(err)
	}
	ttlNow = ttlNow.Add(apiRunTTL - time.Nanosecond)
	if _, ok := ttlRegistry.Get(ttlRun.ID); !ok {
		t.Fatal("run expired before TTL")
	}
	ttlNow = ttlNow.Add(time.Nanosecond)
	if _, ok := ttlRegistry.Get(ttlRun.ID); ok {
		t.Fatal("run retained at TTL boundary")
	}
}

func TestAPIRunRegistryOwnershipAndSessionLookups(t *testing.T) {
	r := NewAPIRunRegistry(nil)
	first, err := r.Create("session-b", "token-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Create("session-a", "token-b")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.GetOwned(first.ID, "token-a"); !ok || got.ID != first.ID {
		t.Fatalf("owner lookup=%+v,%v", got, ok)
	}
	if _, ok := r.GetOwned(first.ID, "token-b"); ok {
		t.Fatal("foreign token owns run")
	}
	if _, ok := r.GetOwned("absent", "token-a"); ok {
		t.Fatal("missing run found")
	}
	if !r.IsSessionActive("session-a") || !r.IsSessionActive("session-b") {
		t.Fatal("queued session not active")
	}
	if got := r.ActiveSessionIDs(); fmt.Sprint(got) != "[session-a session-b]" {
		t.Fatalf("active sessions=%v", got)
	}
	if _, err := r.Transition(first.ID, APIRunSucceeded, "", "", ""); err == nil {
		t.Fatal("queued run succeeded directly")
	}
	if _, err := r.Transition(first.ID, APIRunCancelled, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if r.IsSessionActive("session-b") {
		t.Fatal("cancelled session remains active")
	}
	if second.TokenID != "token-b" {
		t.Fatal("token ownership was not retained")
	}
}

func TestAPIRunRegistryConcurrentAccess(t *testing.T) {
	r := NewAPIRunRegistry(nil)
	const workers = 24
	var wg sync.WaitGroup
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, err := r.Create(fmt.Sprintf("session-%d", i), fmt.Sprintf("token-%d", i))
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			ids <- run.ID
			if _, err := r.Transition(run.ID, APIRunRunning, "", "", ""); err != nil {
				t.Errorf("running transition: %v", err)
				return
			}
			if _, ok := r.GetOwned(run.ID, fmt.Sprintf("token-%d", i)); !ok {
				t.Errorf("owned run %s missing", run.ID)
			}
			if _, err := r.Transition(run.ID, APIRunSucceeded, "", "", "message"); err != nil {
				t.Errorf("terminal transition: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("runs=%d want %d", len(seen), workers)
	}
}
