package sessguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type AgentFinalizationIdentity = finterm.AgentFinalizationIdentity

type AgentFinalizationAuthority struct {
	DB       *sql.DB
	Identity AgentFinalizationIdentity
	reader   *sql.DB
	service  *FinalizationServiceIdentity
}

// FinalizationServiceIdentity is an opaque token identifying one service instance.
type FinalizationServiceIdentity struct {
	marker byte
}

func NewFinalizationServiceIdentity() *FinalizationServiceIdentity {
	return &FinalizationServiceIdentity{marker: 1}
}

type BatchCommitIdentity struct {
	Host, Runner, Owner     string
	OwnerFor                func(spec jobbatch.Spec) string
	AuthorityFor            func(owner string) *AgentFinalizationAuthority
	AdmitOwners             func(context.Context, jobbatch.Transition, []string) (func() error, error)
	finalizationDB          *sql.DB
	finalizationService     *FinalizationServiceIdentity
	finalizationReader      *sql.DB
	finalizationAuthorities []*AgentFinalizationAuthority
}

func NewAgentFinalizationAuthority(writer, reader *sql.DB, identity AgentFinalizationIdentity, service ...*FinalizationServiceIdentity) *AgentFinalizationAuthority {
	authority := &AgentFinalizationAuthority{DB: writer, Identity: identity, reader: reader}
	if len(service) > 0 {
		authority.service = service[0]
	}
	return authority
}

func (g *Guard) AssociateFinalizationService(a *AgentFinalizationAuthority, service *FinalizationServiceIdentity) *AgentFinalizationAuthority {
	if a == nil {
		return nil
	}
	a.service = service
	return a
}

func (a *AgentFinalizationAuthority) ServiceIdentity() *FinalizationServiceIdentity {
	if a == nil {
		return nil
	}
	return a.service
}

func (g *Guard) AuthorityReader(a *AgentFinalizationAuthority) *sql.DB {
	if a == nil {
		return nil
	}
	return a.reader
}

func newBoundBatchCommitIdentity(writer *sql.DB, authorities ...*AgentFinalizationAuthority) BatchCommitIdentity {
	id := BatchCommitIdentity{finalizationDB: writer, finalizationAuthorities: append([]*AgentFinalizationAuthority(nil), authorities...)}
	for _, authority := range authorities {
		if authority != nil && authority.DB == writer {
			id.finalizationReader = authority.reader
			id.finalizationService = authority.service
			break
		}
	}
	return id
}

func BindFinalizationService(id BatchCommitIdentity, service *FinalizationServiceIdentity) BatchCommitIdentity {
	id.finalizationService = service
	return id
}

func (id BatchCommitIdentity) FinalizationService() *FinalizationServiceIdentity {
	return id.finalizationService
}

func AttachFinalizationAuthorities(id BatchCommitIdentity, service *FinalizationServiceIdentity, authorities ...*AgentFinalizationAuthority) BatchCommitIdentity {
	for _, authority := range authorities {
		if authority != nil && authority.service != service {
			return id
		}
	}
	id.finalizationService = service
	id.finalizationAuthorities = append([]*AgentFinalizationAuthority(nil), authorities...)
	return id
}

func (id BatchCommitIdentity) FinalizationBinding() (*sql.DB, []*AgentFinalizationAuthority) {
	return id.finalizationDB, append([]*AgentFinalizationAuthority(nil), id.finalizationAuthorities...)
}

func (g *Guard) BindAgentFinalization(ctx context.Context, identity AgentFinalizationIdentity) (*AgentFinalizationAuthority, error) {
	if g == nil || g.writer == nil {
		return nil, errors.New("finalization authority service unavailable")
	}
	if err := finterm.NewAgentFinalizationStore(g.writer).Bind(ctx, identity); err != nil {
		return nil, fmt.Errorf("bind finalization identity: %w", err)
	}
	a := NewAgentFinalizationAuthority(g.writer, g.reader, identity)
	if err := a.Revalidate(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *AgentFinalizationAuthority) Revalidate(ctx context.Context) error {
	if a == nil || a.DB == nil || !a.Identity.Valid() {
		return finterm.ErrAgentFinalizationChanged
	}
	store := finterm.NewAgentFinalizationStore(a.DB)
	record, err := store.Snapshot(ctx, a.Identity)
	if err != nil {
		return fmt.Errorf("read finalization authority: %w", err)
	}
	if !record.Active() {
		return finterm.ErrAgentFinalizationChanged
	}
	if record.Cancelled {
		return context.Canceled
	}
	var conflicting int64
	err = a.DB.QueryRowContext(ctx, `SELECT count(*) FROM session_agent_finalizations WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing') AND (logical_call_id<>? OR generation<>?)`, a.Identity.SessionID, a.Identity.LogicalCallID, a.Identity.Generation).Scan(&conflicting)
	if err != nil {
		return fmt.Errorf("inspect active finalization incarnation: %w", err)
	}
	if conflicting != 0 {
		return finterm.ErrAgentFinalizationChanged
	}
	return nil
}

func (g *Guard) WithAgentFinalizationAuthorities(id BatchCommitIdentity, authorities ...*AgentFinalizationAuthority) (BatchCommitIdentity, error) {
	if g == nil || g.writer == nil {
		return BatchCommitIdentity{}, errors.New("finalization authority service unavailable")
	}
	if id.finalizationDB != nil {
		return BatchCommitIdentity{}, errors.New("batch identity already has a finalization authority binding")
	}
	byOwner := make(map[string]*AgentFinalizationAuthority, len(authorities))
	for _, a := range authorities {
		if a == nil || a.DB != g.writer || a.service != id.finalizationService || !a.Identity.Valid() {
			return BatchCommitIdentity{}, errors.New("finalization authority belongs to another service")
		}
		owner := a.Identity.SessionID
		if prev, ok := byOwner[owner]; ok {
			if prev.Identity != a.Identity {
				return BatchCommitIdentity{}, fmt.Errorf("conflicting finalization authorities for owner %q", owner)
			}
			return BatchCommitIdentity{}, fmt.Errorf("duplicate finalization authority for owner %q", owner)
		}
		byOwner[owner] = a
	}
	id.finalizationDB = g.writer
	id.finalizationReader = g.reader
	id.finalizationAuthorities = append([]*AgentFinalizationAuthority(nil), authorities...)
	for _, authority := range authorities {
		if authority != nil && authority.service != id.finalizationService {
			return BatchCommitIdentity{}, errors.New("finalization authority belongs to another service")
		}
	}
	return id, nil
}
