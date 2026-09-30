// Negative cost deltas (`sessions reset` zeroes a session with
// IncrementCost(id, -cost)) and their effect on the child-to-parent ledger.

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// owedChildCost is the spend a child still owes its parent: cost above the
// ledger. Cost below the ledger is a reset that predates the ledger-aware
// decrement (decrementCost keeps them in step now): everything the child
// holds was spent after it, so all of it is new. The one rule shared by
// TransferChildCostToParent and decrementCost.
func owedChildCost(cost, accounted float64) float64 {
	owed := cost - accounted
	if owed < 0 {
		owed = cost
	}
	return owed
}

// decrementCost applies a negative delta and keeps parent_cost_accounted
// consistent with it, in one IMMEDIATE transaction (the writer connection
// takes the write lock at BEGIN, so nothing interleaves between the read and
// the writes, in this process or another).
//
// Order matters: the spend the parent was not yet charged for is real money,
// so it is charged FIRST; only then is cost lowered, and the ledger is set
// equal to the new cost (settled, so the next transfer charges only spend
// after the decrease). A reset computed from a stale
// read (delta = -previousCost) therefore keeps spend that landed since: it
// was charged just now and stays on the child, counted once. cost stops at
// zero (the column has CHECK cost >= 0; a second reset from the same stale
// read must not fail). A missing parent has nobody to charge and is skipped.
func (s *service) decrementCost(ctx context.Context, sessionID string, delta float64) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)
	row, err := qtx.GetSessionByID(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}

	chargedParent := ""
	if owed := owedChildCost(row.Cost, row.ParentCostAccounted); row.ParentSessionID.Valid && owed > 0 {
		_, perr := qtx.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
			ID:   row.ParentSessionID.String,
			Cost: owed,
		})
		switch {
		case perr == nil:
			chargedParent = row.ParentSessionID.String
		case !errors.Is(perr, sql.ErrNoRows):
			return Session{}, fmt.Errorf("charge parent before cost decrease: %w", perr)
		}
	}

	newCost := max(row.Cost+delta, 0)
	newAccounted := newCost // everything above was just charged: the ledger is settled
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET cost = ?, parent_cost_accounted = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		newCost, newAccounted, sessionID,
	); err != nil {
		return Session{}, fmt.Errorf("decrease cost: %w", err)
	}
	updated, err := qtx.GetSessionByID(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit cost decrease: %w", err)
	}

	sess := s.fromDBItem(updated)
	s.Publish(pubsub.UpdatedEvent, sess)
	if chargedParent != "" {
		if p, err := s.Get(ctx, chargedParent); err == nil {
			s.Publish(pubsub.UpdatedEvent, p)
		}
	}
	return sess, nil
}
