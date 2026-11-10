package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/google/uuid"
)

type batchFinalizationTicket struct {
	identity AgentFinalizationIdentity
	ticket   string
}

// admitBatchCommitOwners resolves one ordered, distinct ticket set before SQL.
func admitBatchCommitOwners(store *BatchStore, ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func() error, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	owners = slices.Clone(owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if owners[0] == "" {
		return nil, errors.New("batch commit admission: missing owner")
	}

	if id.finalizationService == nil {
		if len(id.finalizationAuthorities) != 0 {
			return nil, errors.New("batch commit admission: authority service missing")
		}
		if id.AdmitOwners == nil {
			return nil, nil
		}
		release, err := id.AdmitOwners(ctx, transition, owners)
		if err == nil && release == nil {
			return nil, errors.New("batch commit admission: missing release")
		}
		return release, err
	}

	service := id.finalizationService
	if service.db == nil || store == nil || store.writer != service.db {
		return nil, errors.New("batch commit admission: authority and batch writer must share the service database")
	}
	authorities := make(map[string]*AgentFinalizationAuthority, len(id.finalizationAuthorities))
	for _, authority := range id.finalizationAuthorities {
		if authority == nil || authority.service != service || authority.service.db != service.db || !authority.identity.valid() {
			return nil, errors.New("batch commit admission: conflicting authority binding")
		}
		owner := authority.identity.SessionID
		if _, exists := authorities[owner]; exists {
			return nil, fmt.Errorf("batch commit admission: duplicate authority for owner %q", owner)
		}
		authorities[owner] = authority
	}

	for owner := range authorities {
		if _, affected := slices.BinarySearch(owners, owner); !affected {
			delete(authorities, owner)
		}
	}
	tickets := make([]batchFinalizationTicket, 0, len(owners))
	for _, owner := range owners {
		authority := authorities[owner]
		if authority == nil {
			exists, err := service.hasFinalizationScope(ctx, owner)
			if err != nil {
				return finalizationTicketRelease(ctx, service, tickets), fmt.Errorf("batch commit admission: inspect owner %q: %w", owner, err)
			}
			if exists {
				return finalizationTicketRelease(ctx, service, tickets), fmt.Errorf("batch commit admission: explicit authority required for owner %q", owner)
			}
			continue
		}
		if err := authority.revalidate(ctx); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), fmt.Errorf("batch commit admission: authority for owner %q: %w", owner, err)
		}
		ticket := uuid.NewString()
		store := NewAgentFinalizationStore(service.db)
		if _, err := store.Admit(ctx, authority.identity, ticket); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), fmt.Errorf("batch commit admission: admit owner %q: %w", owner, err)
		}
		tickets = append(tickets, batchFinalizationTicket{identity: authority.identity, ticket: ticket})
	}

	var callbackRelease func() error
	if id.AdmitOwners != nil {
		var err error
		callbackRelease, err = id.AdmitOwners(ctx, transition, owners)
		if err != nil {
			callbackReleases := combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets))
			if callbackRelease == nil {
				return callbackReleases, errors.Join(err, errors.New("batch commit admission: callback error without release"))
			}
			return callbackReleases, err
		}
		if callbackRelease == nil {
			return finalizationTicketRelease(ctx, service, tickets), errors.New("batch commit admission: missing release")
		}
	}
	return combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets)), nil
}

func finalizationTicketRelease(ctx context.Context, service *service, tickets []batchFinalizationTicket) func() error {
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
				if err := NewAgentFinalizationStore(service.db).Release(cleanupCtx, ticket.identity, ticket.ticket); err != nil {
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
		once.Do(func() {
			firstErr := first()
			secondErr := second()
			releaseErr = errors.Join(firstErr, secondErr)
		})
		return releaseErr
	}
}

func batchCommitReleaseError(primary, release error) error {
	if release == nil {
		return primary
	}
	if primary == nil {
		return release
	}
	return errors.Join(primary, release)
}
