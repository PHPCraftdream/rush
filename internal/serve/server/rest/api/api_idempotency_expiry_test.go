package api

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
	store := NewIdempotencyStore(func() time.Time { return now })
	for i := range IdempotencyMaxEntries {
		key := "k" + strconv.Itoa(i)
		_, _, generation := store.Begin("token", key, "hash")
		store.Complete("token", key, "hash", generation, http.StatusOK, nil, nil)
	}
	if got := len(store.Entries); got != IdempotencyMaxEntries {
		t.Fatalf("entries before expiry = %d, want %d", got, IdempotencyMaxEntries)
	}
	now = now.Add(IdempotencyTTL)
	if outcome, _, _ := store.Begin("token", "fresh", "hash"); outcome != IdempotencyStarted {
		t.Fatalf("insertion outcome = %q", outcome)
	}
	if got := len(store.Entries); got != 1 {
		t.Fatalf("entries after expiry sweep = %d, want 1 (only the new reservation)", got)
	}
}
