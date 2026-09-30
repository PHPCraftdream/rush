package session_test

// R7A-1: `sessions reset` zeroes a session's cost with a negative
// IncrementCost delta. The child-to-parent ledger (parent_cost_accounted)
// must follow, or the child's next spend is silently swallowed. Real
// migrations and the real IMMEDIATE writer connection (db.Connect).

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type costFixture struct {
	ctx    context.Context
	svc    session.Service
	conn   *sql.DB
	parent session.Session
	child  session.Session
}

func newCostFixture(t *testing.T) *costFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	svc := session.NewService(db.New(conn), conn)
	parent, err := svc.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(ctx, "child-1", parent.ID, "child")
	require.NoError(t, err)
	return &costFixture{ctx: ctx, svc: svc, conn: conn, parent: parent, child: child}
}

func (f *costFixture) cost(t *testing.T, id string) float64 {
	t.Helper()
	sess, err := f.svc.Get(f.ctx, id)
	require.NoError(t, err)
	return sess.Cost
}

func (f *costFixture) accounted(t *testing.T, id string) float64 {
	t.Helper()
	var v float64
	require.NoError(t, f.conn.QueryRowContext(f.ctx,
		`SELECT parent_cost_accounted FROM sessions WHERE id = ?`, id).Scan(&v))
	return v
}

func (f *costFixture) spend(t *testing.T, delta float64) {
	t.Helper()
	_, err := f.svc.IncrementCost(f.ctx, f.child.ID, delta)
	require.NoError(t, err)
}

func (f *costFixture) transfer(t *testing.T) {
	t.Helper()
	require.NoError(t, f.svc.TransferChildCostToParent(f.ctx, f.child.ID, f.parent.ID))
}

// TestTransferChildCostToParent_AfterReset_ChargesNewSpend is the operator
// scenario: the child spent and was charged, `sessions reset` zeroed its
// cost, and its next spend (below the old accounted amount) must still reach
// the parent. Before the fix the parent stayed at 1.0.
//
// Revert-check: with the clamp in TransferChildCostToParent and no accounted
// adjustment in the negative IncrementCost path the parent stays at 1.0
// (FAIL); restored, it is 1.6.
func TestTransferChildCostToParent_AfterReset_ChargesNewSpend(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 1.0)
	f.transfer(t)
	require.InDelta(t, 1.0, f.cost(t, f.parent.ID), 1e-9)

	f.spend(t, -1.0) // what `sessions reset` does
	f.spend(t, 0.6)
	f.transfer(t)

	require.InDelta(t, 1.6, f.cost(t, f.parent.ID), 1e-9,
		"the 0.6 spent after the reset must be charged to the parent")
	require.InDelta(t, 0.6, f.cost(t, f.child.ID), 1e-9)
	require.InDelta(t, 0.6, f.accounted(t, f.child.ID), 1e-9)
}

// TestIncrementCostNegative_ThenLargerSpend_ChargesAll pins why a
// "cost < accounted means reset" heuristic in the transfer alone is not
// enough: a post-reset spend larger than the old accounted amount is not
// detectable afterwards. The negative delta must move the ledger itself.
func TestIncrementCostNegative_ThenLargerSpend_ChargesAll(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 1.0)
	f.transfer(t)
	f.spend(t, -1.0)
	f.spend(t, 1.5)
	f.transfer(t)

	require.InDelta(t, 2.5, f.cost(t, f.parent.ID), 1e-9,
		"1.0 before the reset plus 1.5 after it")
}

// TestIncrementCostNegative_ChargesOwedSpendBeforeZeroing: cost the child
// accrued but the parent was not yet charged for is real spend; a reset must
// not erase it from the parent's books.
func TestIncrementCostNegative_ChargesOwedSpendBeforeZeroing(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 0.4)
	f.transfer(t)   // parent 0.4
	f.spend(t, 0.6) // owed, not yet transferred

	f.spend(t, -1.0) // reset

	require.InDelta(t, 1.0, f.cost(t, f.parent.ID), 1e-9, "the owed 0.6 is charged by the reset")
	require.InDelta(t, 0.0, f.cost(t, f.child.ID), 1e-9)
	require.InDelta(t, 0.0, f.accounted(t, f.child.ID), 1e-9)

	f.spend(t, 0.25)
	f.transfer(t)
	require.InDelta(t, 1.25, f.cost(t, f.parent.ID), 1e-9)
}

// TestIncrementCostNegative_StaleSnapshotKeepsConcurrentSpend: reset computes
// its delta from an earlier read (-previousCost); spend that landed in
// between stays on the child and is charged exactly once.
func TestIncrementCostNegative_StaleSnapshotKeepsConcurrentSpend(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 1.0)
	f.transfer(t)
	f.spend(t, 0.5) // lands after the reset command read cost=1.0
	f.spend(t, -1.0)

	require.InDelta(t, 1.5, f.cost(t, f.parent.ID), 1e-9, "the concurrent 0.5 is charged once")
	require.InDelta(t, 0.5, f.cost(t, f.child.ID), 1e-9)
	f.transfer(t)
	require.InDelta(t, 1.5, f.cost(t, f.parent.ID), 1e-9, "and not charged again")
}

// TestIncrementCostNegative_MissingParentStillResets: a child whose parent
// row is gone has nobody to charge; the reset itself must still succeed.
func TestIncrementCostNegative_MissingParentStillResets(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 0.75)
	_, err := f.conn.ExecContext(f.ctx, `DELETE FROM sessions WHERE id = ?`, f.parent.ID)
	require.NoError(t, err)

	f.spend(t, -0.75)
	require.InDelta(t, 0.0, f.cost(t, f.child.ID), 1e-9)
	require.InDelta(t, 0.0, f.accounted(t, f.child.ID), 1e-9)
}

// TestIncrementCostNegative_RootSessionZeroes: a session without a parent has
// no ledger to keep; the negative delta is a decrement that stops at zero
// (the cost column has CHECK cost >= 0, and two resets computed from the same
// stale read must not fail the second one).
func TestIncrementCostNegative_RootSessionZeroes(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 0.5)
	_, err := f.svc.IncrementCost(f.ctx, f.parent.ID, 2.0)
	require.NoError(t, err)
	_, err = f.svc.IncrementCost(f.ctx, f.parent.ID, -2.5)
	require.NoError(t, err)
	require.InDelta(t, 0.0, f.cost(t, f.parent.ID), 1e-9, "a decrement past zero stops at zero")
	require.InDelta(t, 0.0, f.accounted(t, f.parent.ID), 1e-9)
}

// TestTransferChildCostToParent_CostBelowAccounted_TreatedAsReset heals rows
// written before the negative-delta fix (cost 0, accounted 1.0 after an old
// reset): the whole current cost is new spend.
//
// Revert-check: restoring the delta<0 clamp charges nothing (FAIL).
func TestTransferChildCostToParent_CostBelowAccounted_TreatedAsReset(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	_, err := f.conn.ExecContext(f.ctx,
		`UPDATE sessions SET cost = 0.6, parent_cost_accounted = 1.0 WHERE id = ?`, f.child.ID)
	require.NoError(t, err)
	f.transfer(t)

	require.InDelta(t, 0.6, f.cost(t, f.parent.ID), 1e-9)
	require.InDelta(t, 0.6, f.accounted(t, f.child.ID), 1e-9)
	f.transfer(t)
	require.InDelta(t, 0.6, f.cost(t, f.parent.ID), 1e-9, "a second transfer is a no-op")
}

// TestIncrementCostIfUnderMax_NegativeDeltaFollowsTheLedger: the guarded
// writer takes the same reset path for a decrease (a decrease can never
// overshoot a budget).
func TestIncrementCostIfUnderMax_NegativeDeltaFollowsTheLedger(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	f.spend(t, 1.0)
	f.transfer(t)
	_, ok, err := f.svc.IncrementCostIfUnderMax(f.ctx, f.child.ID, -1.0, 5.0)
	require.NoError(t, err)
	require.True(t, ok)
	f.spend(t, 0.6)
	f.transfer(t)

	require.InDelta(t, 1.6, f.cost(t, f.parent.ID), 1e-9)
}

// TestCostResetRacingTransfers_ParentSeesEverySpendOnce hammers spends,
// transfers and resets (each computed from a stale read, as the CLI does)
// concurrently. Whatever the interleaving, after one final transfer the
// parent has been charged exactly the sum of all spends. Amounts are binary
// fractions so float addition is exact.
func TestCostResetRacingTransfers_ParentSeesEverySpendOnce(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t)

	const workers, rounds = 4, 40
	var spent atomic.Int64 // in quarters
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*3)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				switch (w + i) % 3 {
				case 0:
					if _, err := f.svc.IncrementCost(f.ctx, f.child.ID, 0.25); err != nil {
						errs <- err
						return
					}
					spent.Add(1)
				case 1:
					if err := f.svc.TransferChildCostToParent(f.ctx, f.child.ID, f.parent.ID); err != nil {
						errs <- err
						return
					}
				default:
					cur, err := f.svc.Get(f.ctx, f.child.ID)
					if err != nil {
						errs <- err
						return
					}
					if _, err := f.svc.IncrementCost(f.ctx, f.child.ID, -cur.Cost); err != nil {
						errs <- err
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	f.transfer(t)
	want := float64(spent.Load()) * 0.25
	require.InDelta(t, want, f.cost(t, f.parent.ID), 1e-9,
		"the parent is charged the sum of all child spends, none lost, none doubled")
	require.LessOrEqual(t, f.accounted(t, f.child.ID), f.cost(t, f.child.ID)+1e-9)
}
