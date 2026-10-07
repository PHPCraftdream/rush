// Revert check: TestSetRateLimitedUntilFor_RoundTrip -> deleting the
// RateLimitedUntil field on record/Entry or the SetRateLimitedUntilFor
// stamp/clear loop in rate_limit.go.
package heartbeat

import (
	"testing"
	"time"
)

func TestSetRateLimitedUntilFor_RoundTrip(t *testing.T) {
	setup(t)
	RecordRequest(withCtx(Context{SessionID: "s1", RootSessionID: "s1", Purpose: PurposeTurn, Role: "smart"}), "p", "m", nil)
	future := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	SetRateLimitedUntilFor("s1", future)
	requestFlush(true)
	entries, err := ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var found *Entry
	for i := range entries {
		if entries[i].Session == "s1" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("no entry for s1 after flush")
	}
	stamp := future.UTC().Format(time.RFC3339)
	if found.RateLimitedUntil != stamp {
		t.Fatalf("got RateLimitedUntil %q, want %q", found.RateLimitedUntil, stamp)
	}
	// Zero time clears the stamp; unknown sessions are a silent no-op.
	SetRateLimitedUntilFor("s1", time.Time{})
	SetRateLimitedUntilFor("missing", future)
	requestFlush(true)
	entries, err = ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after clear: %v", err)
	}
	for i := range entries {
		if entries[i].Session == "s1" && entries[i].RateLimitedUntil != "" {
			t.Fatalf("stamp not cleared: %q", entries[i].RateLimitedUntil)
		}
	}
}
