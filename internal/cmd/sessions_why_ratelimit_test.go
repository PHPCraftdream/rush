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
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func seedRateLimitEntry(t *testing.T, sessionID string, until time.Time) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", d)
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.QuiesceForTest)
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
	future := time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC)
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

func TestWhyRateLimitLineNegativeRenderer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  bool
		until time.Time
	}{
		{"past", true, time.Now().Add(-time.Minute)},
		{"missing", false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "negative-" + tc.name
			if tc.seed {
				seedRateLimitEntry(t, id, tc.until)
			} else {
				dir := t.TempDir()
				t.Setenv("RUSH_HEARTBEAT_DIR", dir)
				heartbeat.ResetForTest()
				t.Cleanup(heartbeat.QuiesceForTest)
				heartbeat.RecordRequest(heartbeat.WithContext(context.Background(), heartbeat.Context{SessionID: id, RootSessionID: id, Purpose: heartbeat.PurposeTurn, Role: "smart"}), "p", "m", nil)
				heartbeat.Shutdown(time.Second)
			}
			var buf bytes.Buffer
			printStallHeader(context.Background(), nil, t.TempDir(), id, "running", session.LockFact{}, &buf)
			require.NotContains(t, buf.String(), "rate limit:")
		})
	}
}

func TestWhyRateLimitLineMissingPulse(t *testing.T) {
	seedRateLimitEntry(t, "no-pulse", time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC))
	var buf bytes.Buffer
	lock := session.LockFact{Kind: session.LockHeld}
	stalled := printStallHeader(context.Background(), nil, t.TempDir(), "no-pulse", "running", lock, &buf)
	require.False(t, stalled)
	require.Contains(t, buf.String(), "rate limit: waiting for provider rate limit until")
}

func TestWhyRateLimitLineRunningStaleLock(t *testing.T) {
	a, dataDir, id := seedStallFixture(t, 11*time.Minute)
	seedRateLimitEntry(t, id, time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC))
	var buf bytes.Buffer
	stalled := printStallHeader(context.Background(), a, dataDir, id, "running (stale lock)", session.LockFact{Kind: session.LockHeld}, &buf)
	require.False(t, stalled)
	require.Contains(t, buf.String(), "status: running (stale lock)")
	require.Contains(t, buf.String(), "rate limit: waiting for provider rate limit until")
}

func TestWhyRateLimitLineStalledHeldPulse(t *testing.T) {
	a, dataDir, id := seedStallFixture(t, 11*time.Minute)
	seedRateLimitEntry(t, id, time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC))
	var buf bytes.Buffer
	stalled := printStallHeader(context.Background(), a, dataDir, id, "running", session.LockFact{Kind: session.LockHeld}, &buf)
	require.True(t, stalled)
	require.Contains(t, buf.String(), "STALLED")
	require.Contains(t, buf.String(), "rate limit: waiting for provider rate limit until")
}

// TestWhyRateLimitLineInOutput: `sessions why` surfaces a future provider
// rate-limit wait on a normally running session with a fresh pulse.
func TestWhyRateLimitLineInOutput(t *testing.T) {
	a, dataDir, id := seedStallFixture(t, 0)
	seedRateLimitEntry(t, id, time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC))
	var buf bytes.Buffer
	if err := explainWhy(a, dataDir, id, &buf); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC)
	want := "rate limit: waiting for provider rate limit until " + future.Local().Format("15:04")
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, buf.String())
	}
}
