package sessguard

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/google/uuid"
)

type batchFinalizationTicket struct {
	identity AgentFinalizationIdentity
	ticket   string
}

type BatchFinalizationAdmission struct {
	ancestry finalizationAncestry
	tickets  []batchFinalizationTicket
}

func (a *BatchFinalizationAdmission) HasOwner(owner string) bool {
	if a == nil {
		return false
	}
	_, ok := slices.BinarySearch(a.ancestry.Owners, owner)
	return ok
}

func (a *BatchFinalizationAdmission) AncestryHash() string {
	if a == nil {
		return ""
	}
	type edge struct {
		Node, Owner, Child string
		Token              int64
	}
	snapshot := struct {
		Owners []string
		Edges  []edge
	}{Owners: a.ancestry.Owners}
	for _, item := range a.ancestry.Edges {
		snapshot.Edges = append(snapshot.Edges, edge(item))
	}
	encoded, _ := json.Marshal(snapshot)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func AdmitBatchCommitOwners(writer, reader *sql.DB, ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func() error, *BatchFinalizationAdmission, error) {
	if len(owners) == 0 {
		return nil, nil, nil
	}
	owners = slices.Clone(owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if owners[0] == "" {
		return nil, nil, errors.New("batch commit admission: missing owner")
	}
	boundDB, boundAuthorities := id.FinalizationBinding()
	if boundDB == nil {
		if len(boundAuthorities) != 0 {
			return nil, nil, errors.New("batch commit admission: authority service missing")
		}
		if id.AdmitOwners == nil {
			return nil, nil, nil
		}
		release, err := id.AdmitOwners(ctx, transition, owners)
		if err == nil && release == nil {
			return nil, nil, errors.New("batch commit admission: missing release")
		}
		return release, nil, err
	}
	if boundDB == nil || writer == nil || writer != boundDB {
		return nil, nil, errors.New("batch commit admission: authority and batch writer must share the service database")
	}
	if reader == nil {
		reader = id.finalizationReader
	}
	ancestry, err := ReadFinalizationAncestry(ctx, boundDB, owners)
	if err != nil {
		return nil, nil, fmt.Errorf("batch commit admission: resolve managed ancestors: %w", err)
	}
	owners = ancestry.Owners
	authorities := make(map[string]*AgentFinalizationAuthority, len(boundAuthorities))
	for _, authority := range boundAuthorities {
		if authority == nil || authority.DB != boundDB || authority.ServiceIdentity() != id.FinalizationService() || !authority.Identity.Valid() {
			return nil, nil, errors.New("batch commit admission: conflicting authority binding")
		}
		owner := authority.Identity.SessionID
		if _, exists := authorities[owner]; exists {
			return nil, nil, fmt.Errorf("batch commit admission: duplicate authority for owner %q", owner)
		}
		authorities[owner] = authority
	}
	tickets := make([]batchFinalizationTicket, 0, len(owners))
	defer func() {
		if value := recover(); value != nil {
			if release := finalizationTicketRelease(ctx, boundDB, tickets); release != nil {
				_ = release()
			}
			panic(value)
		}
	}()
	for _, owner := range owners {
		authority := authorities[owner]
		if authority == nil && id.AuthorityFor != nil {
			authority = id.AuthorityFor(owner)
			if authority != nil && (authority.DB != boundDB || authority.ServiceIdentity() != id.FinalizationService() || !authority.Identity.Valid()) {
				return finalizationTicketRelease(ctx, boundDB, tickets), nil, errors.New("batch commit admission: conflicting authority binding")
			}
		}
		if authority == nil {
			exists, err := NewGuard(boundDB, reader).HasFinalizationScope(ctx, owner)
			if err != nil {
				return finalizationTicketRelease(ctx, boundDB, tickets), nil, fmt.Errorf("batch commit admission: inspect owner %q: %w", owner, err)
			}
			if exists {
				return finalizationTicketRelease(ctx, boundDB, tickets), nil, fmt.Errorf("batch commit admission: explicit authority required for owner %q", owner)
			}
			continue
		}
		if err := authority.Revalidate(ctx); err != nil {
			return finalizationTicketRelease(ctx, boundDB, tickets), nil, fmt.Errorf("batch commit admission: authority for owner %q: %w", owner, err)
		}
		ticket := uuid.NewString()
		store := finterm.NewAgentFinalizationStore(boundDB)
		if _, err := store.Admit(ctx, authority.Identity, ticket); err != nil {
			return finalizationTicketRelease(ctx, boundDB, tickets), nil, fmt.Errorf("batch commit admission: admit owner %q: %w", owner, err)
		}
		tickets = append(tickets, batchFinalizationTicket{identity: authority.Identity, ticket: ticket})
	}
	var callbackRelease func() error
	if id.AdmitOwners != nil {
		var err error
		callbackRelease, err = id.AdmitOwners(ctx, transition, owners)
		if err != nil {
			callbackReleases := combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, boundDB, tickets))
			if callbackRelease == nil {
				return callbackReleases, nil, errors.Join(err, errors.New("batch commit admission: callback error without release"))
			}
			return callbackReleases, nil, err
		}
		if callbackRelease == nil {
			return finalizationTicketRelease(ctx, boundDB, tickets), nil, errors.New("batch commit admission: missing release")
		}
	}
	admission := &BatchFinalizationAdmission{ancestry: ancestry, tickets: slices.Clone(tickets)}
	return combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, boundDB, tickets)), admission, nil
}

func finalizationTicketRelease(ctx context.Context, writer *sql.DB, tickets []batchFinalizationTicket) func() error {
	if len(tickets) == 0 {
		return nil
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			for _, ticket := range tickets {
				if err := finterm.NewAgentFinalizationStore(writer).Release(cleanupCtx, ticket.identity, ticket.ticket); err != nil {
					releaseErr = errors.Join(releaseErr, fmt.Errorf("batch commit admission: release owner %q: %w", ticket.identity.SessionID, err))
				}
			}
		})
		return releaseErr
	}
}

func combineTicketReleases(first, second func() error) func() error {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() { firstErr := first(); secondErr := second(); releaseErr = errors.Join(firstErr, secondErr) })
		return releaseErr
	}
}

func ValidateBatchFinalizationTx(ctx context.Context, tx *sql.Tx, owners []string, admission *BatchFinalizationAdmission) error {
	if len(owners) == 0 {
		if admission != nil {
			return finterm.ErrAgentFinalizationChanged
		}
		return nil
	}
	current, err := ReadFinalizationAncestry(ctx, tx, owners)
	if err != nil {
		return fmt.Errorf("batch writer finalization ancestry: %w", err)
	}
	if admission != nil && (!slices.Equal(current.Owners, admission.ancestry.Owners) || !slices.Equal(current.Edges, admission.ancestry.Edges)) {
		return finterm.ErrAgentFinalizationChanged
	}
	tickets := make(map[string]batchFinalizationTicket)
	if admission != nil {
		for _, ticket := range admission.tickets {
			owner := ticket.identity.SessionID
			if !ticket.identity.Valid() || ticket.ticket == "" {
				return finterm.ErrAgentFinalizationChanged
			}
			if _, duplicate := tickets[owner]; duplicate {
				return finterm.ErrAgentFinalizationChanged
			}
			if _, affected := slices.BinarySearch(current.Owners, owner); !affected {
				return finterm.ErrAgentFinalizationChanged
			}
			tickets[owner] = ticket
		}
	}
	for _, owner := range current.Owners {
		ticket, admitted := tickets[owner]
		if !admitted {
			var exists bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
			if err != nil {
				return err
			}
			if exists {
				return finterm.ErrAgentFinalizationChanged
			}
			continue
		}
		record, err := finterm.ReadAgentFinalization(ctx, tx, ticket.identity)
		if err != nil {
			return err
		}
		if record.Cancelled {
			return context.Canceled
		}
		if !record.Active() {
			return finterm.ErrAgentFinalizationChanged
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_tickets
			WHERE session_id=? AND logical_call_id=? AND generation=? AND ticket_id=? AND released=0)
			AND NOT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?
			AND phase IN ('executing', 'waiting_work', 'composing') AND (logical_call_id<>? OR generation<>?))`, ticket.identity.SessionID, ticket.identity.LogicalCallID, ticket.identity.Generation, ticket.ticket, ticket.identity.SessionID, ticket.identity.LogicalCallID, ticket.identity.Generation).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return finterm.ErrAgentFinalizationChanged
		}
	}
	return nil
}

func (g *Guard) AdmitFinalizationMutation(ctx context.Context, writer *sql.DB, authorities []*AgentFinalizationAuthority, owners ...string) (func() error, *BatchFinalizationAdmission, error) {
	if len(authorities) == 0 {
		return nil, nil, nil
	}
	if authorities[0] == nil {
		return nil, nil, finterm.ErrAgentFinalizationChanged
	}
	id := newBoundBatchCommitIdentity(writer, authorities...)
	return AdmitBatchCommitOwners(writer, g.reader, ctx, id, jobbatch.Transition{}, owners)
}

func BatchCommitReleaseError(primary, release error) error {
	if release == nil {
		return primary
	}
	if primary == nil {
		return release
	}
	return errors.Join(primary, release)
}
