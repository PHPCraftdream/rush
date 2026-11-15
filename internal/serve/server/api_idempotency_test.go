package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdempotencyBeginReplayConflictAndExpiry(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newIdempotencyStore(func() time.Time { return now })
	if outcome, replay, _ := store.Begin("token", "key", "hash"); outcome != idempotencyStarted || replay != nil {
		t.Fatalf("first Begin = %q, %v", outcome, replay)
	}
	if outcome, _, _ := store.Begin("token", "key", "hash"); outcome != idempotencyInProgress {
		t.Fatalf("in-flight Begin = %q", outcome)
	}
	if outcome, _, _ := store.Begin("token", "key", "other"); outcome != idempotencyConflict {
		t.Fatalf("different-body Begin = %q", outcome)
	}
	store.Complete("token", "key", "hash", 1, http.StatusCreated, http.Header{"Content-Type": {"application/json"}, "Location": {"/sessions/1"}}, []byte("created"))
	outcome, replay, _ := store.Begin("token", "key", "hash")
	if outcome != idempotencyReplay || replay.Status != http.StatusCreated || !bytes.Equal(replay.Body, []byte("created")) || replay.Headers.Get("Location") != "/sessions/1" || replay.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("replay = %q, %#v", outcome, replay)
	}
	now = now.Add(idempotencyTTL)
	if outcome, _, _ := store.Begin("token", "key", "hash"); outcome != idempotencyStarted {
		t.Fatalf("expired Begin = %q", outcome)
	}
}

func TestIdempotencyServerErrorAndRedirectNotCached(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusFound} {
		store := newIdempotencyStore(nil)
		_, _, generation := store.Begin("token", "key", "hash")
		store.Complete("token", "key", "hash", generation, status, nil, []byte("response"))
		if outcome, replay, _ := store.Begin("token", "key", "hash"); outcome != idempotencyStarted || replay != nil {
			t.Fatalf("Begin after status %d = %q, %v", status, outcome, replay)
		}
	}
}

func TestIdempotencyMaxEntriesEvictsOldestCompleted(t *testing.T) {
	now := time.Unix(1, 0)
	store := newIdempotencyStore(func() time.Time { return now })
	for i := range idempotencyMaxEntries {
		key := string(rune(i + 1))
		_, _, generation := store.Begin("token", key, "hash")
		store.Complete("token", key, "hash", generation, http.StatusOK, nil, nil)
		now = now.Add(time.Nanosecond)
	}
	if outcome, _, _ := store.Begin("token", "last", "hash"); outcome != idempotencyStarted {
		t.Fatalf("insertion outcome = %q", outcome)
	}
	if outcome, _, _ := store.Begin("token", string(rune(1)), "hash"); outcome != idempotencyStarted {
		t.Fatalf("oldest entry outcome = %q", outcome)
	}
	if got := len(store.entries); got != idempotencyMaxEntries {
		t.Fatalf("entries = %d, want %d", got, idempotencyMaxEntries)
	}
}

func TestIdempotencyInFlightEntriesCannotExpireOrEvict(t *testing.T) {
	now := time.Unix(1, 0)
	store := newIdempotencyStore(func() time.Time { return now })
	_, _, generation := store.Begin("token", "reserved", "hash")
	now = now.Add(2 * idempotencyTTL)
	for i := 0; i < idempotencyMaxEntries-1; i++ {
		if outcome, _, _ := store.Begin("token", "fill"+string(rune(i+1)), "hash"); outcome != idempotencyStarted {
			t.Fatalf("fill %d outcome = %q", i, outcome)
		}
	}
	if outcome, _, _ := store.Begin("token", "overflow", "hash"); outcome != idempotencyFull {
		t.Fatalf("full store outcome = %q, want %q", outcome, idempotencyFull)
	}
	if outcome, _, _ := store.Begin("token", "reserved", "hash"); outcome != idempotencyInProgress {
		t.Fatalf("reservation outcome = %q", outcome)
	}
	store.Release("token", "reserved", generation)
}

func TestIdempotencyGenerationPreventsStaleCompletion(t *testing.T) {
	store := newIdempotencyStore(nil)
	_, _, oldGeneration := store.Begin("token", "key", "hash")
	store.Release("token", "key", oldGeneration)
	_, _, newGeneration := store.Begin("token", "key", "hash")
	store.Complete("token", "key", "hash", oldGeneration, http.StatusOK, nil, []byte("stale"))
	if outcome, _, _ := store.Begin("token", "key", "hash"); outcome != idempotencyInProgress {
		t.Fatalf("after stale completion = %q", outcome)
	}
	store.Complete("token", "key", "hash", newGeneration, http.StatusOK, nil, []byte("current"))
	outcome, replay, _ := store.Begin("token", "key", "hash")
	if outcome != idempotencyReplay || string(replay.Body) != "current" {
		t.Fatalf("replay = %q, %#v", outcome, replay)
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	for _, test := range []struct {
		key   string
		valid bool
	}{
		{"a", true},
		{"A0._:-", true},
		{string(bytes.Repeat([]byte("a"), 128)), true},
		{"", false},
		{string(bytes.Repeat([]byte("a"), 129)), false},
		{"has space", false},
		{"bad/char", false},
		{"ümlaut", false},
	} {
		t.Run(test.key, func(t *testing.T) {
			if got := validIdempotencyKey(test.key); got != test.valid {
				t.Fatalf("validIdempotencyKey(%q) = %t, want %t", test.key, got, test.valid)
			}
		})
	}
}

type idempotencyTestResponse struct {
	status  int
	headers http.Header
	body    []byte
}

func TestWithIdempotencyReplayResponse(t *testing.T) {
	store := newIdempotencyStore(nil)
	var calls atomic.Int32
	handler := withIdempotency(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/sessions/one")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("response\n"))
	}))
	first := runIdempotencyRequest(t, handler, "same", "same")
	second := runIdempotencyRequest(t, handler, "same", "same")
	if first.status != http.StatusCreated || second.status != first.status || !bytes.Equal(first.body, second.body) || second.headers.Get("Location") != "/sessions/one" || second.headers.Get("Content-Type") != "application/json" || second.headers.Get("Idempotent-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("first=%#v second=%#v calls=%d", first, second, calls.Load())
	}
}

func TestWithIdempotencyConcurrentSameKey(t *testing.T) {
	store := newIdempotencyStore(nil)
	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	var calls atomic.Int32
	handler := withIdempotency(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			t.Error("handler release timed out")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	const count = 20
	responses := make(chan idempotencyTestResponse, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			responses <- runIdempotencyRequest(t, handler, "race", "same")
		}()
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not start")
	}
	close(release)
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("requests did not finish")
	}
	close(responses)
	if calls.Load() != 1 {
		t.Fatalf("handler executions = %d, want 1", calls.Load())
	}
	for response := range responses {
		if response.status != http.StatusOK && response.status != http.StatusConflict {
			t.Fatalf("concurrent response status = %d", response.status)
		}
	}
}

func TestWithIdempotencyRejectsEmptyAndInvalidKey(t *testing.T) {
	handler := withIdempotency(newIdempotencyStore(nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }))
	for _, key := range []string{"", "bad key"} {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "/sessions", bytes.NewBufferString("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header[http.CanonicalHeaderKey("Idempotency-Key")] = []string{key}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("key %q status = %d, want 400", key, response.Code)
		}
	}
}

func TestWithIdempotencyFullReturnsServiceUnavailable(t *testing.T) {
	store := newIdempotencyStore(nil)
	for i := 0; i < idempotencyMaxEntries; i++ {
		store.Begin("token", "occupied"+string(rune(i+1)), "hash")
	}
	handler := withIdempotency(store, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }))
	response := runIdempotencyRequest(t, handler, "new", "body")
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.status)
	}
}

func TestWithIdempotencyPanicReleasesReservation(t *testing.T) {
	store := newIdempotencyStore(nil)
	handler := withIdempotency(store, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		runIdempotencyRequest(t, handler, "panic", "body")
	}()
	if len(store.entries) != 0 {
		t.Fatalf("entries after panic = %d, want 0", len(store.entries))
	}
}

func TestWithIdempotencyOversizedBodyReturns413(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "/sessions", bytes.NewBufferString("oversized"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Idempotency-Key", "oversized")
	request.Body = http.MaxBytesReader(httptest.NewRecorder(), request.Body, 2)
	handler := withIdempotency(newIdempotencyStore(nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
}

func TestWithIdempotencyRejectsMultipleKeys(t *testing.T) {
	handler := withIdempotency(newIdempotencyStore(nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }))
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "/sessions", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header[http.CanonicalHeaderKey("Idempotency-Key")] = []string{"first", "second"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestIdempotencyHashPartsAreLengthDelimited(t *testing.T) {
	var first, second bytes.Buffer
	writeIdempotencyHashPart(&first, []byte("a"))
	writeIdempotencyHashPart(&first, []byte("bc"))
	writeIdempotencyHashPart(&second, []byte("ab"))
	writeIdempotencyHashPart(&second, []byte("c"))
	if bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("different parts encoded identically")
	}
}

func runIdempotencyRequest(t *testing.T, handler http.Handler, key, body string) idempotencyTestResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "/sessions", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	payload, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	return idempotencyTestResponse{status: result.StatusCode, headers: result.Header.Clone(), body: payload}
}
