// Revert check: TestWhyRateLimitLineFuture -> deleting the rate-limit branch
// in printStallHeader/rateLimitLineForWhy; TestWhyRateLimitLinePastOrMissing
// -> deleting the future-time check in rateLimitLineForWhy.
package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

func seedRateLimitEntry(t *testing.T, sessionID string, until time.Time) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", d)
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.ResetForTest)
	heartbeat.RecordRequest(
		heartbeat.WithContext(context.Background(), heartbeat.Context{
			SessionID: sessionID, RootSessionID: sessionID, Purpose: heartbeat.PurposeTurn, Role: "smart",
		}),
		"p", "m", nil,
	)
	heartbeat.SetRateLimitedUntilFor(sessionID, until)
	heartbeat.Shutdown(time.Second)
}

func TestWhyRateLimitLineFuture(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	seedRateLimitEntry(t, "s-live", future)
	line := rateLimitLineForWhy("s-live", time.Now())
	// The stamp is stored UTC; the operator reads the local clock.
	want := "rate limit: waiting for provider rate limit until " + future.Local().Format("15:04") + "\n"
	if line != want {
		t.Fatalf("got %q, want %q", line, want)
	}
}

func TestWhyRateLimitLinePastOrMissing(t *testing.T) {
	seedRateLimitEntry(t, "s-past", time.Now().Add(-time.Minute))
	if line := rateLimitLineForWhy("s-past", time.Now()); line != "" {
		t.Fatalf("past stamp: got %q, want \"\"", line)
	}
	if line := rateLimitLineForWhy("other", time.Now()); line != "" {
		t.Fatalf("missing session: got %q, want \"\"", line)
	}
}

// TestWhyRateLimitLineInOutput: `sessions why` on a running session prints
// the rate-limit line. Revert check: deleting the fmt.Fprint(out, line) in
// printStallHeader.
func TestWhyRateLimitLineInOutput(t *testing.T) {
	a, dataDir, id := seedStallFixture(t, 0)
	future := time.Now().Add(10 * time.Minute)
	seedRateLimitEntry(t, id, future)
	var buf bytes.Buffer
	if err := explainWhy(a, dataDir, id, &buf); err != nil {
		t.Fatal(err)
	}
	want := "rate limit: waiting for provider rate limit until " + future.Local().Format("15:04")
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, buf.String())
	}
}
