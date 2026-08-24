// task #611: test oracles that isolate the watchdog-deadline mechanism and
// the lease-loss cancellation cause, instead of only observing an effect
// several mechanisms can produce.
//
// Part 1 (this file): TestP1_1_WatchdogDeadlineStoredIsTruePreRoundingValue
// asserts, as a direct value comparison anchored at the exact instant
// production code samples trueNewExpiresAt (BEFORE the DB round-trip — see
// TestOnWatchdogDeadlineStored's own doc for why that anchor matters and
// TestP1_1_WatchdogCancelsAtTTLMinusMargin's firstRenewalDone doc for the
// prior #609 lesson about anchoring on the wrong side of a DB call), that
// watchdogDeadlineAtomic is seeded from the true, pre-rounding deadline —
// not the whole-Unix-second, ceiling-rounded value RenewRunQueueLease
// actually persists. This closes the gap the seventh review's RC4 found:
// reverting run_queue_entry_exec.go's `watchdogDeadlineAtomic.Store(...)`
// line to the rounded value left `ok internal/session 58.3s` — the existing
// TestP1_1_WatchdogCancelsAtTTLMinusMargin's 6-9s elapsed-time window is too
// broad to notice a donation of well under 1s.
//
// Part 2 lives in p1_2_cancel_cause_oracle_test.go.
package session_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestP1_1_WatchdogDeadlineStoredIsTruePreRoundingValue asserts the value
// stored into the watchdog's own deadline atomic on a successful renewal is
// the true, sub-second-precision deadline (time.Now().Add(TTL), sampled
// BEFORE the DB round-trip) — never the whole-Unix-second, ceiling-rounded
// value actually persisted to the lease_expires_at column.
//
// This is a direct value assertion, not an elapsed-time inference: it does
// not need TTL/margin sized to make a multi-second window observable, and
// it is immune to scheduling jitter on the machine running the test.
//
// REVERT CHECK (task #611): reverting run_queue_entry_exec.go's
//
//	watchdogDeadlineAtomic.Store(trueNewExpiresAt.UnixNano())
//
// to
//
//	watchdogDeadlineAtomic.Store(time.Unix(newExpiresAt, 0).UnixNano())
//
// (the pre-b7d50642 shape) fails this test: `stored` then equals
// `roundedDeadline` rather than `trueDeadline`. Verified — the revert
// produces
//
//	observation 0: stored watchdog deadline (09:05:02) must equal the true
//	pre-rounding deadline (09:05:01.4958775), not something else
//
// while TestP1_1_WatchdogCancelsAtTTLMinusMargin, run in the same
// invocation, still passes: its 6-9s elapsed window cannot see a ~0.5s
// donation.
//
// The one instant this oracle could not distinguish is trueNewExpiresAt
// landing exactly on a whole second, where rounded == true and the two
// spellings agree. time.Now() carries nanosecond precision, so that is not
// a case that occurs in practice; the gap assertion below states the
// invariant rather than assuming it.
func TestP1_1_WatchdogDeadlineStoredIsTruePreRoundingValue(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-deadline-oracle")
	ctx := t.Context()

	idempotencyKey := "p1-1-deadline-oracle-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	type observation struct {
		stored, trueDeadline, roundedDeadline time.Time
	}
	obsCh := make(chan observation, 8)

	// TTL chosen large enough that a renewal tick has time to land, short
	// enough the test stays fast. The rounding gap (roundedDeadline minus
	// trueDeadline) depends only on the sub-second wall-clock phase the
	// renewal happens to land on, not on TTL size, so no particular TTL is
	// required to observe it — but a TTL of a few seconds makes it very
	// likely at least one of several renewal ticks lands with a non-trivial
	// fractional second, and the test only needs ONE observation.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p1-1-deadline-oracle-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   3 * time.Second,
		TestOnWatchdogDeadlineStored: func(stored, trueDeadline, roundedDeadline time.Time) {
			select {
			case obsCh <- observation{stored: stored, trueDeadline: trueDeadline, roundedDeadline: roundedDeadline}:
			default:
			}
		},
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })

	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Collect a handful of observations across several renewal ticks (TTL/3
	// interval => renewals roughly every second) so the assertion below is
	// not betting everything on the very first tick happening to land on a
	// wall-clock instant with a large fractional second.
	var observations []observation
	deadline := time.After(6 * time.Second)
collect:
	for len(observations) < 3 {
		select {
		case obs := <-obsCh:
			observations = append(observations, obs)
		case <-deadline:
			break collect
		}
	}
	close(blockCh)

	require.NotEmpty(t, observations, "expected at least one watchdog-deadline-stored observation")

	for i, obs := range observations {
		// The stored value must equal the true (pre-rounding) deadline
		// exactly — both are captured from the same trueNewExpiresAt
		// variable in production code, so this is not a timing-sensitive
		// comparison at all.
		require.True(t, obs.stored.Equal(obs.trueDeadline),
			"observation %d: stored watchdog deadline (%v) must equal the true pre-rounding deadline (%v), not something else",
			i, obs.stored, obs.trueDeadline)

		// The rounded (DB-persisted) value can only ever be >= the true
		// deadline (ceilUnixSeconds rounds up), by less than 1 full
		// second. The KEY assertion: the stored value must NOT be the
		// rounded one whenever rounding actually added anything — i.e.
		// whenever roundedDeadline != trueDeadline, stored must differ
		// from roundedDeadline too (and by the same non-zero delta,
		// since stored == trueDeadline exactly, per above).
		gap := obs.roundedDeadline.Sub(obs.trueDeadline)
		require.True(t, gap >= 0 && gap < time.Second,
			"observation %d: ceilUnixSeconds rounding gap must be in [0, 1s), got %v", i, gap)
		if gap > 0 {
			require.False(t, obs.stored.Equal(obs.roundedDeadline),
				"observation %d: stored watchdog deadline (%v) must NOT equal the rounded/DB-persisted deadline (%v) when rounding added %v of slack — storing the rounded value donates that slack to the executor instead of the safety margin (task #604)",
				i, obs.stored, obs.roundedDeadline, gap)
		}
	}
}
