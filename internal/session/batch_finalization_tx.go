package session

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// validateBatchFinalizationTx runs before every reservation/projection effect.
// Legacy absence is a transaction-local observation, never an admission proof.
func validateBatchFinalizationTx(ctx context.Context, tx *sql.Tx, owners []string, admission *batchFinalizationAdmission) error {
	if len(owners) == 0 {
		if admission != nil {
			return ErrAgentFinalizationChanged
		}
		return nil
	}
	current, err := readFinalizationAncestry(ctx, tx, owners)
	if err != nil {
		return fmt.Errorf("batch writer finalization ancestry: %w", err)
	}
	if admission != nil && (!slices.Equal(current.owners, admission.ancestry.owners) || !slices.Equal(current.edges, admission.ancestry.edges)) {
		return ErrAgentFinalizationChanged
	}
	tickets := make(map[string]batchFinalizationTicket)
	if admission != nil {
		for _, ticket := range admission.tickets {
			owner := ticket.identity.SessionID
			if !ticket.identity.valid() || ticket.ticket == "" {
				return ErrAgentFinalizationChanged
			}
			if _, duplicate := tickets[owner]; duplicate {
				return ErrAgentFinalizationChanged
			}
			if _, affected := slices.BinarySearch(current.owners, owner); !affected {
				return ErrAgentFinalizationChanged
			}
			tickets[owner] = ticket
		}
	}
	for _, owner := range current.owners {
		ticket, admitted := tickets[owner]
		if !admitted {
			var exists bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
			if err != nil {
				return err
			}
			if exists {
				return ErrAgentFinalizationChanged
			}
			continue
		}
		record, err := readAgentFinalization(ctx, tx, ticket.identity)
		if err != nil {
			return err
		}
		if record.Cancelled {
			return context.Canceled
		}
		if !record.active() {
			return ErrAgentFinalizationChanged
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_tickets
			WHERE session_id=? AND logical_call_id=? AND generation=? AND ticket_id=? AND released=0)
			AND NOT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?
			AND phase IN ('executing', 'waiting_work', 'composing') AND (logical_call_id<>? OR generation<>?))`,
			ticket.identity.SessionID, ticket.identity.LogicalCallID, ticket.identity.Generation, ticket.ticket,
			ticket.identity.SessionID, ticket.identity.LogicalCallID, ticket.identity.Generation).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return ErrAgentFinalizationChanged
		}
	}
	return nil
}
