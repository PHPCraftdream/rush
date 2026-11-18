package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/google/uuid"
)

// BatchCommitIdentity is the Runner identity a commit hook writes under:
// host identity plus the fresh Runner incarnation UUID, and the owner session
// whose Submit reservations this Runner creates (one Runner, one owner).
type BatchCommitIdentity struct {
	Host, Runner, Owner string

	// R11F (P1-1), minimal additive hunk: optional per-Submit owner
	// resolver. When non-nil, the Submit reservation is written under the
	// returned owner — the INVOKING session carried in the one-item spec
	// payload — instead of the fixed Owner (which remains only a dormant
	// default). An empty return keeps the fixed Owner. Non-Submit
	// transitions are unaffected: batchCommitProject already resolves each
	// root's owner from the row (RootOwner).
	OwnerFor func(spec jobbatch.Spec) string

	// AuthorityFor resolves the runner-wide finalization authority for one
	// owner at commit time (the process registry the composition root
	// installs). The commit hook runs on runner goroutines without the
	// submitting call's context, so a per-run authority is reachable only
	// through this lookup. Nil keeps the exact legacy behavior: an owner
	// with a bound scope but no static authority stays refused.
	AuthorityFor func(owner string) *AgentFinalizationAuthority

	// AdmitOwners runs after durable tickets and before batch writer I/O. Its
	// error-returning release is attempted after success or failure.
	AdmitOwners func(context.Context, jobbatch.Transition, []string) (func() error, error)

	finalizationService     *service
	finalizationAuthorities []*AgentFinalizationAuthority
}

type batchFinalizationTicket struct {
	identity AgentFinalizationIdentity
	ticket   string
}

type BatchFinalizationAdmission struct {
	ancestry finalizationAncestry
	tickets  []batchFinalizationTicket
}

// AdmitBatchCommitOwners resolves one ordered, distinct ticket set before SQL.
func AdmitBatchCommitOwners(writer *sql.DB, ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func() error, *BatchFinalizationAdmission, error) {
	if len(owners) == 0 {
		return nil, nil, nil
	}
	owners = slices.Clone(owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if owners[0] == "" {
		return nil, nil, errors.New("batch commit admission: missing owner")
	}

	if id.finalizationService == nil {
		if len(id.finalizationAuthorities) != 0 {
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

	service := id.finalizationService
	if service.DB == nil || writer == nil || writer != service.DB {
		return nil, nil, errors.New("batch commit admission: authority and batch writer must share the service database")
	}
	ancestry, err := ReadFinalizationAncestry(ctx, service.DB, owners)
	if err != nil {
		return nil, nil, fmt.Errorf("batch commit admission: resolve managed ancestors: %w", err)
	}
	owners = ancestry.owners
	authorities := make(map[string]*AgentFinalizationAuthority, len(id.finalizationAuthorities))
	for _, authority := range id.finalizationAuthorities {
		if authority == nil || authority.Service != service || authority.Service.DB != service.DB || !authority.Identity.Valid() {
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
			if release := finalizationTicketRelease(ctx, service, tickets); release != nil {
				_ = release() // A failed release remains a durable hold.
			}
			panic(value)
		}
	}()
	for _, owner := range owners {
		authority := authorities[owner]
		if authority == nil && id.AuthorityFor != nil {
			authority = id.AuthorityFor(owner)
			if authority != nil && (authority.Service != service || authority.Service.DB != service.DB || !authority.Identity.Valid()) {
				return finalizationTicketRelease(ctx, service, tickets), nil, errors.New("batch commit admission: conflicting authority binding")
			}
		}
		if authority == nil {
			exists, err := service.hasFinalizationScope(ctx, owner)
			if err != nil {
				return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: inspect owner %q: %w", owner, err)
			}
			if exists {
				return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: explicit authority required for owner %q", owner)
			}
			continue
		}
		if err := authority.revalidate(ctx); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: authority for owner %q: %w", owner, err)
		}
		ticket := uuid.NewString()
		store := NewAgentFinalizationStore(service.DB)
		if _, err := store.Admit(ctx, authority.Identity, ticket); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: admit owner %q: %w", owner, err)
		}
		tickets = append(tickets, batchFinalizationTicket{identity: authority.Identity, ticket: ticket})
	}

	var callbackRelease func() error
	if id.AdmitOwners != nil {
		var err error
		callbackRelease, err = id.AdmitOwners(ctx, transition, owners)
		if err != nil {
			callbackReleases := combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets))
			if callbackRelease == nil {
				return callbackReleases, nil, errors.Join(err, errors.New("batch commit admission: callback error without release"))
			}
			return callbackReleases, nil, err
		}
		if callbackRelease == nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, errors.New("batch commit admission: missing release")
		}
	}
	admission := &BatchFinalizationAdmission{ancestry: ancestry, tickets: slices.Clone(tickets)}
	return combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets)), admission, nil
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
				if err := NewAgentFinalizationStore(service.DB).Release(cleanupCtx, ticket.identity, ticket.ticket); err != nil {
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

func BatchCommitReleaseError(primary, release error) error {
	if release == nil {
		return primary
	}
	if primary == nil {
		return release
	}
	return errors.Join(primary, release)
}
