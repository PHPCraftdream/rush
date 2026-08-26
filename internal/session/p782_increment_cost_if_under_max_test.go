package session_test

// Regression coverage for the IncrementCostIfUnderMax primitive behind K-2
// (task #782, 2026-08-26 review). The budget predicate lives inside the UPDATE's
// WHERE clause (cost + delta < max_cost), so SQLite's serialized writers make a
// joint overshoot impossible no matter how many chargers race. The agent-side
// red/green regression for the wiring is
// TestFireCacheKeepAlive_ChargeRefusedWhenDeltaWouldCrossMaxCost in
// internal/agent. This file is a behavioural pin of the primitive (a regression
// to an unconditional UPDATE turns it red), not part of the agent-level
// red/green cycle.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestIncrementCostIfUnderMax_ConcurrentChargesCannotJointlyOvershoot proves
// that the SQL primitive behind K-2 cannot be jointly overshot by concurrent
// chargers. The predicate lives inside the UPDATE's WHERE clause
// (cost + delta < max_cost), so SQLite's serialized writers guarantee that
// when multiple concurrent IncrementCostIfUnderMax calls would together cross
// the cap, only one lands and the others return charged=false — the old
// read-then-write pattern could instead let both observe cost < maxCost and
// then both land their delta, jointly overshooting (e.g. 0.09 -> 0.14 against
// a 0.10 max).
func TestIncrementCostIfUnderMax_ConcurrentChargesCannotJointlyOvershoot(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	q := db.New(conn)
	svc := session.NewService(q, conn)

	// Sequential sanity case first: from a fresh session, a charge that fits
	// succeeds; a second charge that would cross the cap is refused.
	t.Run("sequential", func(t *testing.T) {
		t.Parallel()
		sess, err := svc.Create(ctx, "k2-sequential")
		require.NoError(t, err)

		// First charge: 0.06 fits under max 0.10.
		sess, charged, err := svc.IncrementCostIfUnderMax(ctx, sess.ID, 0.06, 0.10)
		require.NoError(t, err)
		require.True(t, charged, "first charge must succeed (0.06 < 0.10)")
		require.InDelta(t, 0.06, sess.Cost, 1e-9, "cost must be charged")

		// Second charge: 0.06 + 0.06 = 0.12 >= 0.10, so must be refused.
		sess, charged, err = svc.IncrementCostIfUnderMax(ctx, sess.ID, 0.06, 0.10)
		require.NoError(t, err)
		require.False(t, charged, "second charge must be refused (0.06 + 0.06 >= 0.10)")
		require.InDelta(t, 0.06, sess.Cost, 1e-9, "cost must remain unchanged")
	})

	// Concurrent case: pre-charge to 0.04, then fire 8 concurrent charges of
	// 0.02 each against max 0.10. The cap can NEVER be overshot, no matter
	// how many racers land.
	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		sess, err := svc.Create(ctx, "k2-concurrent")
		require.NoError(t, err)

		// Pre-charge to 0.04, leaving 0.06 headroom.
		sess, err = svc.IncrementCost(ctx, sess.ID, 0.04)
		require.NoError(t, err)
		require.InDelta(t, 0.04, sess.Cost, 1e-9)

		const numChargers = 8
		const delta = 0.02
		const maxCost = 0.10

		var trues atomic.Int64
		var wg sync.WaitGroup
		wg.Add(numChargers)

		for i := 0; i < numChargers; i++ {
			go func() {
				defer wg.Done()
				_, charged, err := svc.IncrementCostIfUnderMax(ctx, sess.ID, delta, maxCost)
				require.NoError(t, err)
				if charged {
					trues.Add(1)
				}
			}()
		}

		wg.Wait()

		// Sanity: at least one charge must have succeeded (0.04 + 0.02 = 0.06 < 0.10).
		require.Greater(t, trues.Load(), int64(1),
			"the predicate must not refuse everything (0.04 + 0.02 = 0.06 fits)")

		// Final cost must be <= maxCost, never overshot.
		updated, err := svc.Get(ctx, sess.ID)
		require.NoError(t, err)
		require.LessOrEqual(t, updated.Cost, maxCost+1e-9,
			"the cap can NEVER be overshot, no matter how many racers land")

		// Final cost must equal pre-charge + delta * trues (no partial charges).
		require.InDelta(t, 0.04+delta*float64(trues.Load()), updated.Cost, 1e-9,
			"cost must reflect exactly the number of successful charges")
	})
}
