// R8B-5: a Drain that fails because the history no longer fits the model's
// window closes its debt at once (terminal) and the marker must say why and
// what to do: the events stay in history, so the next turn fails the same way
// until the session is compacted or replaced. "Continuation at the next turn"
// is false for that failure.
package agent

import (
	"context"
	"net/http"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// wakeFailedMarkerText is the text of the session's wake_failed marker.
func (f *attemptFixture) wakeFailedMarkerText(ctx context.Context) string {
	f.t.Helper()
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(f.t, err)
	for _, no := range notices {
		if no.Kind == session.NoticeKindWakeFailed {
			return no.Text
		}
	}
	f.t.Fatal("no wake_failed marker was written")
	return ""
}

// accountFailedDrain settles a debt of "call-1" through the one accounting
// function as a Drain leg that ended in turnErr.
func (f *attemptFixture) accountFailedDrain(ctx context.Context, turnErr error) {
	f.t.Helper()
	f.seedDebt(ctx, "call-1", true)
	snap, err := f.store.CaptureDebtSnapshot(ctx, f.sessID)
	require.NoError(f.t, err)
	require.Len(f.t, snap.Jobs, 1)
	f.coord.accountDrainAttempt(ctx, &drainAttempt{
		sessionID: f.sessID, snapshot: snap, outcome: drainAttempted,
	}, turnErr)
}

// TestDrainAttempt_ContextTooLargeMarkerNamesCauseAndRemedy: the marker of a
// context-window failure names the cause and the remedy and does not promise a
// continuation; every other terminal failure keeps the ordinary text.
//
// Revert-check: writing the ordinary "продолжение — при следующем ходе" tail for
// every failure turns the three too-large cases red.
func TestDrainAttempt_ContextTooLargeMarkerNamesCauseAndRemedy(t *testing.T) {
	t.Parallel()
	const remedy = "Сожмите"
	cases := []struct {
		name    string
		err     error
		tooLong bool
	}{
		{"flagged provider error", &fantasy.ProviderError{Title: "bad request", Message: "prompt is too long", StatusCode: 400, ContextTooLargeErr: true}, true},
		{"token counts parsed", &fantasy.ProviderError{Title: "bad request", Message: "x", StatusCode: 400, ContextUsedTokens: 220000, ContextMaxTokens: 200000}, true},
		{"openai-style 400 message", &fantasy.ProviderError{Title: "bad request", Message: "This model's maximum context length is 200000 tokens. However, your messages resulted in 220000 tokens.", StatusCode: 400}, true},
		{"413", &fantasy.ProviderError{Title: "request too large", Message: "payload", StatusCode: 413}, true},
		{"unrelated 400", &fantasy.ProviderError{Title: "bad request", Message: "invalid tool schema", StatusCode: 400}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newAttemptFixture(t, "attempt-too-large", attemptFixtureOpts{noIdle: true})
			f.accountFailedDrain(ctx, tc.err)

			row := f.row(ctx, "call-1")
			require.EqualValues(t, 1, row.ReactedFailed, "a terminal failure closes the row at once")
			text := f.wakeFailedMarkerText(ctx)
			require.Contains(t, text, "call-1")
			if tc.tooLong {
				require.Contains(t, text, remedy, "the marker names the remedy")
				require.Contains(t, text, "окн", "and the cause: the context window")
				require.NotContains(t, text, "продолжение — при следующем ходе", "the next turn cannot continue")
			} else {
				require.Contains(t, text, "продолжение — при следующем ходе")
				require.NotContains(t, text, remedy)
			}
		})
	}
}

// TestDrainAttempt_ContextTooLargeThroughTheProvider: the same through a real
// Drain against a provider answering 400 "maximum context length" (the openai
// shape, which fantasy does not flag itself).
//
// Revert-check: as the table test.
func TestDrainAttempt_ContextTooLargeThroughTheProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-too-large-e2e", attemptFixtureOpts{noIdle: true, handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 200000 tokens. However, your messages resulted in 220000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}})
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)
	require.EqualValues(t, 1, f.row(ctx, "call-1").ReactedFailed)
	text := f.wakeFailedMarkerText(ctx)
	require.Contains(t, text, "Сожмите")
	require.NotContains(t, text, "продолжение — при следующем ходе")
}
