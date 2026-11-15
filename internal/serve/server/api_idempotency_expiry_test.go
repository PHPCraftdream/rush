package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// An insert sweeps every expired completed entry, not only the single oldest
// one that capacity pressure would evict.
func TestIdempotencyInsertSweepsAllExpiredEntries(t *testing.T) {
	now := time.Unix(1, 0)
	store := newIdempotencyStore(func() time.Time { return now })
	for i := range idempotencyMaxEntries {
		key := "k" + strconv.Itoa(i)
		_, _, generation := store.Begin("token", key, "hash")
		store.Complete("token", key, "hash", generation, http.StatusOK, nil, nil)
	}
	if got := len(store.entries); got != idempotencyMaxEntries {
		t.Fatalf("entries before expiry = %d, want %d", got, idempotencyMaxEntries)
	}
	now = now.Add(idempotencyTTL)
	if outcome, _, _ := store.Begin("token", "fresh", "hash"); outcome != idempotencyStarted {
		t.Fatalf("insertion outcome = %q", outcome)
	}
	if got := len(store.entries); got != 1 {
		t.Fatalf("entries after expiry sweep = %d, want 1 (only the new reservation)", got)
	}
}
