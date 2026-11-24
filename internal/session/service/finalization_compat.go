package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session/service/components/finqueue"
	"github.com/PHPCraftdream/rush/internal/session/service/components/sessguard"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

type AgentFinalizationRequirement = finqueue.Requirement

func finalizationContinuationExistsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_continuations WHERE row_id=?)`, id).Scan(&exists)
	return exists, err
}

func (snapshot AgentFinalizationContext) Requirement() *AgentFinalizationRequirement {
	if len(snapshot.authorities) == 0 {
		return nil
	}
	requirement := &AgentFinalizationRequirement{Version: 1}
	for _, authority := range snapshot.authorities {
		requirement.Authorities = append(requirement.Authorities, authority.Identity)
	}
	slices.SortFunc(requirement.Authorities, func(a, b AgentFinalizationIdentity) int {
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return requirement
}

func CanonicalFinalizationRequirement(requirement *AgentFinalizationRequirement) (string, error) {
	return finqueue.CanonicalRequirement(requirement)
}

func FinalizationPayloadHash(data SessionAgentCallData) (string, error) {
	return finqueue.PayloadHash(data)
}

func FinalizationAncestryHash(ancestry finalizationAncestry) string {
	type edge struct {
		Node, Owner, Child string
		Token              int64
	}
	snapshot := struct {
		Owners []string
		Edges  []edge
	}{Owners: ancestry.owners}
	for _, item := range ancestry.edges {
		snapshot.Edges = append(snapshot.Edges, edge{Node: item.node, Owner: item.owner, Child: item.child, Token: item.token})
	}
	encoded, _ := json.Marshal(snapshot) // Only strings and integers.
	return finqueue.HashEncoded(encoded)
}

func trustedFinalizationDeclaration(ctx context.Context, writerService *service, data SessionAgentCallData) (string, error) {
	declaration, err := CanonicalFinalizationRequirement(data.FinalizationRequirement)
	if err != nil {
		return "", err
	}
	snapshot := CaptureAgentFinalizationContext(ctx)
	for _, authority := range snapshot.authorities {
		if authority == nil || authority.Service == nil || authority.Service.DB != writerService.DB {
			return "", ErrAgentFinalizationChanged
		}
		if authority.Identity.SessionID == data.SessionID && authority.Identity.LogicalCallID != data.LogicalCallID {
			return "", ErrAgentFinalizationChanged
		}
	}
	trusted, err := CanonicalFinalizationRequirement(snapshot.Requirement())
	if err != nil || declaration != trusted {
		return "", ErrAgentFinalizationChanged
	}
	return declaration, nil
}

type (
	AgentFinalizationIdentity    = finterm.AgentFinalizationIdentity
	AgentFinalizationRecord      = finterm.AgentFinalizationRecord
	AgentFinalizationCandidate   = finterm.AgentFinalizationCandidate
	AgentFinalizationStore       = finterm.AgentFinalizationStore
	AgentFinalizationRecovery    = finterm.AgentFinalizationRecovery
	AgentFinalizationSealReason  = finterm.AgentFinalizationSealReason
	AgentFinalizationSealOutcome = finterm.AgentFinalizationSealOutcome
	AgentFinalizationScopeFacts  = finterm.AgentFinalizationScopeFacts
	AgentFinalizationScopeReader = finterm.AgentFinalizationScopeReader
	FinalizationSealWriter       = finterm.FinalizationSealWriter
)

const (
	AgentFinalizationSealNone              = finterm.AgentFinalizationSealNone
	AgentFinalizationSealCancelled         = finterm.AgentFinalizationSealCancelled
	AgentFinalizationSealWrongPhase        = finterm.AgentFinalizationSealWrongPhase
	AgentFinalizationSealNoCandidate       = finterm.AgentFinalizationSealNoCandidate
	AgentFinalizationSealStaleCandidate    = finterm.AgentFinalizationSealStaleCandidate
	AgentFinalizationSealRevisionChanged   = finterm.AgentFinalizationSealRevisionChanged
	AgentFinalizationSealOpenTickets       = finterm.AgentFinalizationSealOpenTickets
	AgentFinalizationSealScopeIncomplete   = finterm.AgentFinalizationSealScopeIncomplete
	AgentFinalizationSealDescendantsActive = finterm.AgentFinalizationSealDescendantsActive
	AgentFinalizationSealReactionDebt      = finterm.AgentFinalizationSealReactionDebt
	AgentFinalizationSealPhysicalHolds     = finterm.AgentFinalizationSealPhysicalHolds
	AgentFinalizationSealQueuedCalls       = finterm.AgentFinalizationSealQueuedCalls
	AgentFinalizationSealCandidateDamaged  = finterm.AgentFinalizationSealCandidateDamaged
)

const (
	AgentFinalizationNoCandidate      = finterm.AgentFinalizationNoCandidate
	AgentFinalizationCandidateAdopted = finterm.AgentFinalizationCandidateAdopted
	AgentFinalizationCandidateFailed  = finterm.AgentFinalizationCandidateFailed
)

var (
	ErrAgentFinalizationChanged        = finterm.ErrAgentFinalizationChanged
	ErrAgentFinalizationAccepted       = finterm.ErrAgentFinalizationAccepted
	ErrAgentFinalizationCandidateStale = finterm.ErrAgentFinalizationCandidateStale
	ErrAgentFinalizationLeaseChanged   = errors.New("finalization_queue_lease_changed")
)

// The service package keeps its existing typed context proof API.
type agentFinalizationQueueLease = finqueue.Lease

func finalizationQueueLeaseChanged() error {
	return errors.Join(ErrAgentFinalizationChanged, ErrAgentFinalizationLeaseChanged)
}

func NewAgentFinalizationStore(conn *sql.DB) *AgentFinalizationStore {
	return finterm.NewAgentFinalizationStore(conn)
}

type BatchCommitIdentity struct {
	Host, Runner, Owner string
	OwnerFor            func(spec jobbatch.Spec) string
	AuthorityFor        func(owner string) *AgentFinalizationAuthority
	AdmitOwners         func(context.Context, jobbatch.Transition, []string) (func() error, error)
	inner               sessguard.BatchCommitIdentity
}

type BatchFinalizationAdmission = sessguard.BatchFinalizationAdmission

func (id BatchCommitIdentity) FinalizationService() *sessguard.FinalizationServiceIdentity {
	return id.inner.FinalizationService()
}

func (id BatchCommitIdentity) FinalizationBinding() (*sql.DB, []*sessguard.AgentFinalizationAuthority) {
	return id.inner.FinalizationBinding()
}

func componentIdentity(id BatchCommitIdentity) sessguard.BatchCommitIdentity {
	inner := id.inner
	inner.Host, inner.Runner, inner.Owner = id.Host, id.Runner, id.Owner
	inner.OwnerFor, inner.AdmitOwners = id.OwnerFor, id.AdmitOwners
	inner.AuthorityFor = nil
	if id.AuthorityFor != nil {
		inner.AuthorityFor = func(owner string) *sessguard.AgentFinalizationAuthority {
			a := id.AuthorityFor(owner)
			if a == nil || a.Service == nil || !a.Identity.Valid() {
				return nil
			}
			return a.Service.sessguardAuthority(a)
		}
	}
	return inner
}

func serviceIdentity(public BatchCommitIdentity, id sessguard.BatchCommitIdentity) BatchCommitIdentity {
	return BatchCommitIdentity{Host: id.Host, Runner: id.Runner, Owner: id.Owner, OwnerFor: id.OwnerFor, AuthorityFor: public.AuthorityFor, AdmitOwners: id.AdmitOwners, inner: id}
}

func AdmitBatchCommitOwners(writer *sql.DB, ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func() error, *BatchFinalizationAdmission, error) {
	return sessguard.AdmitBatchCommitOwners(writer, nil, ctx, componentIdentity(id), transition, owners)
}

func ValidateBatchFinalizationTx(ctx context.Context, tx *sql.Tx, owners []string, admission *BatchFinalizationAdmission) error {
	return sessguard.ValidateBatchFinalizationTx(ctx, tx, owners, admission)
}

func AdmitFinalizationMutation(ctx context.Context, writer *sql.DB, owners ...string) (func() error, *BatchFinalizationAdmission, error) {
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	if len(authorities) == 0 {
		return nil, nil, nil
	}
	first := authorities[0]
	if first == nil || first.Service == nil || first.Service.DB != writer {
		return nil, nil, ErrAgentFinalizationChanged
	}
	service := first.Service
	inner := make([]*sessguard.AgentFinalizationAuthority, 0, len(authorities))
	for _, authority := range authorities {
		if authority == nil || authority.Service != service || authority.Service.DB != writer {
			return nil, nil, ErrAgentFinalizationChanged
		}
		converted := service.sessguardAuthority(authority)
		if converted == nil {
			return nil, nil, ErrAgentFinalizationChanged
		}
		inner = append(inner, converted)
	}
	id := sessguard.BindFinalizationService(sessguard.BatchCommitIdentity{}, service.finalizationIdentity)
	id, err := sessguard.NewGuard(writer, service.readDB).WithAgentFinalizationAuthorities(id, inner...)
	if err != nil {
		return nil, nil, err
	}
	reader := first.Reader()
	if reader == nil {
		reader = service.readDB
	}
	return sessguard.AdmitBatchCommitOwners(writer, reader, ctx, id, jobbatch.Transition{}, owners)
}

func BatchCommitReleaseError(primary, release error) error {
	return sessguard.BatchCommitReleaseError(primary, release)
}

func (s *service) existingFinalizationQueueEnqueue(ctx context.Context, rowID, owner, raw string, payload finalizationIngressPayload) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	queueOwner, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, rowID)
	if err != nil || !exists {
		return false, err
	}
	if queueOwner != owner {
		return true, ErrAgentFinalizationChanged
	}
	if exists, err := finalizationOutboxExistsTx(ctx, tx, rowID); err != nil {
		return true, err
	} else if exists {
		return true, ErrAgentFinalizationChanged
	}
	if !payload.bound && existingRaw != raw {
		if _, err := validateFinalizationIngressTx(ctx, tx, owner, rowID, existingRaw, payload); err != nil {
			return true, err
		}
		return true, tx.Commit()
	}
	if _, err := verifyQueueCollisionTx(ctx, tx, rowID, owner, payload); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

func (s *service) enqueueFinalizationAware(ctx context.Context, rowID, owner string, raw []byte) (retErr error) {
	return finqueue.Enqueue(ctx, rowID, owner, raw, finqueue.EnqueuePort[finalizationIngressPayload, *BatchFinalizationAdmission]{
		Prepare: func(ctx context.Context, owner string, raw []byte) (finalizationIngressPayload, error) {
			return s.prepareFinalizationIngress(ctx, owner, raw)
		},
		Existing: s.existingFinalizationQueueEnqueue,
		Admit: func(ctx context.Context, owner string) (func() error, *BatchFinalizationAdmission, error) {
			return AdmitFinalizationMutation(ctx, s.DB, owner)
		},
		AncestryHash: func(payload finalizationIngressPayload, admission *BatchFinalizationAdmission) (string, error) {
			return finalizationIngressAncestryHash(payload, admission)
		},
		Begin: func(ctx context.Context) (*sql.Tx, error) { return s.DB.BeginTx(ctx, nil) },
		Validate: func(ctx context.Context, tx *sql.Tx, owner, id string, payload finalizationIngressPayload, ancestry string, admission *BatchFinalizationAdmission) error {
			return validateFreshFinalizationIngressTx(ctx, tx, owner, id, payload, ancestry, admission)
		},
		CheckOutbox:   finalizationOutboxExistsTx,
		CheckContinue: finalizationContinuationExistsTx,
		CheckQueue: func(ctx context.Context, tx *sql.Tx, id string) (string, string, bool, error) {
			return finalizationQueueRowTx(ctx, tx, id)
		},
		Enqueue: func(ctx context.Context, tx *sql.Tx, id, owner string, raw []byte) error {
			now := time.Now().Unix()
			_, err := s.q.WithTx(tx).EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{ID: id, SessionID: owner, CallData: string(raw), CreatedAt: now, UpdatedAt: now})
			return err
		},
		Persist: persistFinalizationIngressTx,
		Changed: ErrAgentFinalizationChanged,
	})
}

type FinalizationTransaction interface {
	WithFinalizationTx(context.Context, string, func(*sql.Tx) error) error
}

func (s *service) WithFinalizationTx(ctx context.Context, id string, accept func(*sql.Tx) error) error {
	return sessguard.NewFinalizationTransaction(s.DB, s.q).WithFinalizationTx(ctx, id, accept)
}

var _ FinalizationTransaction = (*service)(nil)

func CandidateOf(messageID, hash string, id message.FinalizationIdentity) (AgentFinalizationCandidate, bool) {
	return finterm.CandidateOf(messageID, hash, id)
}

func (s *service) bindFinalizationQueueLeaseTx(ctx context.Context, tx *sql.Tx, entry *RunQueueEntry) error {
	var data SessionAgentCallData
	if json.Unmarshal([]byte(entry.CallData), &data) != nil || data.FinalizationRequirement == nil {
		return nil
	}
	lease, err := finqueue.BindQueueLeaseTx(ctx, s.DB, tx, entry.ID, entry.LeasedBy, entry.LeasedAt, entry.Attempts, func() error { return ErrAgentFinalizationChanged }, BatchRequireRow)
	if err != nil {
		return err
	}
	entry.FinalizationLease = lease
	return nil
}

func WithFinalizationQueueLease(ctx context.Context, entry *RunQueueEntry) context.Context {
	if entry == nil || entry.FinalizationLease == nil {
		return ctx
	}
	return finqueue.WithQueueLease(ctx, entry.FinalizationLease)
}

var (
	ErrCallQueuedNotExecuted              = errors.New("run_queue_pump: call was queued into an already-owned session, not executed")
	ErrAgentFinalizationDriverUnavailable = errors.New("finalization_child_driver_unavailable")
)

func RecordFinalizationQueueFailure(ctx context.Context, entry *RunQueueEntry, runErr error) error {
	if entry == nil || entry.FinalizationLease == nil || runErr == nil {
		return nil
	}
	return finqueue.RecordQueueFailure(ctx, entry.FinalizationLease, runErr, ErrCallQueuedNotExecuted, ErrAgentFinalizationDriverUnavailable, ErrAgentFinalizationChanged)
}

type AgentFinalizationService interface {
	BindAgentFinalization(context.Context, AgentFinalizationIdentity) (*AgentFinalizationAuthority, error)
	WithAgentFinalizationAuthorities(BatchCommitIdentity, ...*AgentFinalizationAuthority) (BatchCommitIdentity, error)
}

type AgentFinalizationAuthority struct {
	Service  *service
	DB       *sql.DB
	Identity AgentFinalizationIdentity
	inner    *sessguard.AgentFinalizationAuthority
}

func (a *AgentFinalizationAuthority) Reader() *sql.DB {
	if a == nil {
		return nil
	}
	if a.inner != nil && a.Service != nil && a.inner.DB == a.Service.DB && a.inner.Identity == a.Identity && a.inner.ServiceIdentity() == a.Service.finalizationIdentity {
		return sessguard.NewGuard(a.Service.DB, a.Service.readDB).AuthorityReader(a.inner)
	}
	if a.Service != nil && a.Identity.Valid() {
		return a.Service.readDB
	}
	return nil
}

func (a *AgentFinalizationAuthority) Revalidate(ctx context.Context) error {
	if a == nil || a.inner == nil {
		return ErrAgentFinalizationChanged
	}
	return a.inner.Revalidate(ctx)
}

// sessguardAuthority bridges the public compatibility wrapper to its internal
// capability. The service instance, rather than the writer handle, is the
// public authority identity; legacy wrapper literals may omit DB.
func (s *service) sessguardAuthority(a *AgentFinalizationAuthority) *sessguard.AgentFinalizationAuthority {
	if s == nil || a == nil || a.Service != s || !a.Identity.Valid() {
		return nil
	}
	if a.inner != nil {
		if a.inner.DB != s.DB || a.inner.Identity != a.Identity || a.inner.ServiceIdentity() != s.finalizationIdentity {
			return nil
		}
		return a.inner
	}
	return sessguard.NewAgentFinalizationAuthority(s.DB, s.readDB, a.Identity, s.finalizationIdentity)
}

func (s *service) BindAgentFinalization(ctx context.Context, identity AgentFinalizationIdentity) (*AgentFinalizationAuthority, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("finalization authority service unavailable")
	}
	inner, err := sessguard.NewGuard(s.DB, s.readDB).BindAgentFinalization(ctx, identity)
	if err != nil {
		return nil, err
	}
	inner = sessguard.NewGuard(s.DB, s.readDB).AssociateFinalizationService(inner, s.finalizationIdentity)
	return &AgentFinalizationAuthority{Service: s, DB: s.DB, Identity: identity, inner: inner}, nil
}

func (s *service) WithAgentFinalizationAuthorities(identity BatchCommitIdentity, authorities ...*AgentFinalizationAuthority) (BatchCommitIdentity, error) {
	if s == nil || s.DB == nil {
		return BatchCommitIdentity{}, errors.New("finalization authority service unavailable")
	}
	if identity.FinalizationService() != nil {
		return BatchCommitIdentity{}, errors.New("batch identity already has a finalization authority binding")
	}
	inner := make([]*sessguard.AgentFinalizationAuthority, 0, len(authorities))
	for _, authority := range authorities {
		innerAuthority := s.sessguardAuthority(authority)
		if innerAuthority == nil {
			return BatchCommitIdentity{}, errors.New("finalization authority belongs to another service")
		}
		inner = append(inner, innerAuthority)
	}
	boundIdentity := sessguard.BindFinalizationService(componentIdentity(identity), s.finalizationIdentity)
	bound, err := sessguard.NewGuard(s.DB, s.readDB).WithAgentFinalizationAuthorities(boundIdentity, inner...)
	if err != nil {
		return BatchCommitIdentity{}, err
	}
	return serviceIdentity(identity, bound), nil
}

var _ AgentFinalizationService = (*service)(nil)

const (
	FinalizationAncestorLimit    = sessguard.FinalizationAncestorLimit
	FinalizationParentEdgesQuery = sessguard.FinalizationParentEdgesQuery
)

type finalizationAncestorReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type finalizationManagedEdge struct {
	node, owner, child string
	token              int64
}

type finalizationAncestry struct {
	owners []string
	edges  []finalizationManagedEdge
}

func ReadFinalizationAncestry(ctx context.Context, reader finalizationAncestorReader, owners []string) (finalizationAncestry, error) {
	got, err := sessguard.ReadFinalizationAncestry(ctx, reader, owners)
	result := finalizationAncestry{owners: got.Owners}
	for _, edge := range got.Edges {
		result.edges = append(result.edges, finalizationManagedEdge{node: edge.Node, owner: edge.Owner, child: edge.Child, token: edge.Token})
	}
	return result, err
}

// AgentFinalizationQueuePreparer validates without consuming a continuation.
type AgentFinalizationQueuePreparer interface {
	PrepareAgentFinalizationQueue(context.Context, string, SessionAgentCallData) (*AgentFinalizationQueuePreparation, error)
}

// AgentFinalizationQueuePreparation exposes only verified identity declarations
// until the existing driver owns the mailbox era and consumes the real lease.
type AgentFinalizationQueuePreparation struct {
	store    *service
	inner    *finqueue.Preparation[SessionAgentCallData, AgentFinalizationContext]
	snapshot AgentFinalizationContext
}

func (s *service) PrepareAgentFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData) (*AgentFinalizationQueuePreparation, error) {
	inner, err := finqueue.Prepare[SessionAgentCallData, AgentFinalizationContext](ctx, rowID, data,
		func(frozen []byte) (SessionAgentCallData, error) {
			var copy SessionAgentCallData
			if err := json.Unmarshal(frozen, &copy); err != nil {
				return SessionAgentCallData{}, err
			}
			if copy.FinalizationRequirement == nil {
				return SessionAgentCallData{}, ErrAgentFinalizationChanged
			}
			return copy, nil
		},
		func(ctx context.Context, rowID string, data SessionAgentCallData, consume bool) (AgentFinalizationContext, error) {
			return s.restoreFinalizationQueue(ctx, rowID, data, consume)
		}, ErrAgentFinalizationChanged)
	if err != nil {
		return nil, err
	}
	snapshot := inner.Snapshot()
	return &AgentFinalizationQueuePreparation{store: s, inner: inner, snapshot: snapshot}, nil
}

func (p *AgentFinalizationQueuePreparation) Requirement() *AgentFinalizationRequirement {
	if p == nil {
		return nil
	}
	return p.snapshot.Requirement()
}

func (p *AgentFinalizationQueuePreparation) RequiresManagedDriver() bool {
	return p != nil && p.snapshot.RequiresManagedDriver()
}

// Consume revalidates the full lease and provenance in the same CAS transaction.
// It returns detached frozen call data, never the caller's mutable pointers.
func (p *AgentFinalizationQueuePreparation) Consume(ctx context.Context) (AgentFinalizationContext, SessionAgentCallData, error) {
	if err := ctx.Err(); err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	if p == nil || p.store == nil || p.inner == nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, ErrAgentFinalizationChanged
	}
	data, err := p.inner.FrozenData(func(frozen []byte) (SessionAgentCallData, error) {
		var data SessionAgentCallData
		if err := json.Unmarshal(frozen, &data); err != nil {
			return SessionAgentCallData{}, err
		}
		return data, nil
	})
	if err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	snapshot, err := p.store.RestoreAgentFinalizationQueue(ctx, p.inner.RowID(), data)
	if err != nil {
		return AgentFinalizationContext{}, SessionAgentCallData{}, err
	}
	return snapshot, data, nil
}

var _ AgentFinalizationQueuePreparer = (*service)(nil)

// AgentFinalizationQueueService restores only a trusted, currently leased row.
type AgentFinalizationQueueService interface {
	RestoreAgentFinalizationQueue(context.Context, string, SessionAgentCallData) (AgentFinalizationContext, error)
}

// RestoreAgentFinalizationQueue consumes a continuation before model creation.
// A crash after consumption leaves the durable obligation unresolved, not ready
// for an automatic replay. Consumed receipts survive queue-row deletion.
func (s *service) RestoreAgentFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData) (AgentFinalizationContext, error) {
	return s.restoreFinalizationQueue(ctx, rowID, data, true)
}

func (s *service) restoreFinalizationQueue(ctx context.Context, rowID string, data SessionAgentCallData, consume bool) (AgentFinalizationContext, error) {
	return finqueue.Restore(ctx, s.DB, rowID, data, consume, finqueue.RestorePort[SessionAgentCallData, AgentFinalizationContext, finalizationAncestry]{
		HasRequirement: func(data SessionAgentCallData) bool { return data.FinalizationRequirement != nil },
		SessionID:      func(data SessionAgentCallData) string { return data.SessionID },
		LogicalCallID:  func(data SessionAgentCallData) string { return data.LogicalCallID },
		Canonical:      finalizationPayloadRequirement,
		PayloadHash:    FinalizationPayloadHash,
		Decode: func(raw []byte) (SessionAgentCallData, error) {
			var actual SessionAgentCallData
			err := json.Unmarshal(raw, &actual)
			return actual, err
		},
		ValidateLegacy: func(ctx context.Context, tx *sql.Tx, sessionID string) error {
			return ValidateBatchFinalizationTx(ctx, tx, []string{sessionID}, nil)
		},
		Lease: func(ctx context.Context) *finqueue.Lease {
			proof, _ := ctx.Value(finqueue.LeaseContextKey{}).(*finqueue.Lease)
			return proof
		},
		LeaseChanged: finalizationQueueLeaseChanged,
		Changed:      ErrAgentFinalizationChanged,
		LeasedStatus: RunQueueStatusLeased,
		Ancestry: func(ctx context.Context, tx *sql.Tx, owner string) (finalizationAncestry, string, error) {
			ancestry, err := ReadFinalizationAncestry(ctx, tx, []string{owner})
			return ancestry, FinalizationAncestryHash(ancestry), err
		},
		BuildSnapshot: func(ctx context.Context, tx *sql.Tx, encoded []byte, ancestry finalizationAncestry) (AgentFinalizationContext, error) {
			// Construct from the trusted stored declaration only after all provenance
			// checks. Payload IDs, public row-ID context and fake leases are not proof.
			var stored AgentFinalizationRequirement
			if err := json.Unmarshal(encoded, &stored); err != nil {
				return AgentFinalizationContext{}, err
			}
			snapshot := AgentFinalizationContext{managedContinuation: len(ancestry.edges) != 0}
			byOwner := make(map[string]bool, len(stored.Authorities))
			for _, identity := range stored.Authorities {
				if _, affected := slices.BinarySearch(ancestry.owners, identity.SessionID); !affected {
					return AgentFinalizationContext{}, ErrAgentFinalizationChanged
				}
				record, err := finterm.ReadAgentFinalization(ctx, tx, identity)
				if err != nil {
					return AgentFinalizationContext{}, err
				}
				if record.Cancelled {
					return AgentFinalizationContext{}, context.Canceled
				}
				if !record.Active() {
					return AgentFinalizationContext{}, ErrAgentFinalizationChanged
				}
				byOwner[identity.SessionID] = true
				snapshot.authorities = append(snapshot.authorities, &AgentFinalizationAuthority{Service: s, DB: s.DB, Identity: identity, inner: sessguard.NewAgentFinalizationAuthority(s.DB, s.readDB, identity, s.finalizationIdentity)})
			}
			for _, affected := range ancestry.owners {
				if byOwner[affected] {
					continue
				}
				var exists bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, affected).Scan(&exists); err != nil {
					return AgentFinalizationContext{}, err
				}
				if exists {
					return AgentFinalizationContext{}, ErrAgentFinalizationChanged
				}
			}
			return snapshot, nil
		},
		Consume: func(tx *sql.Tx, rowID, token string) (sql.Result, error) {
			return tx.ExecContext(ctx, `UPDATE session_agent_finalization_continuations SET consumed=1, failure_code=''
				WHERE row_id=? AND lease_token=? AND consumed=0`, rowID, token)
		},
		RequireRows: BatchRequireRow,
	})
}

func finalizationPayloadRequirement(data SessionAgentCallData) (string, error) {
	return CanonicalFinalizationRequirement(data.FinalizationRequirement)
}

var _ AgentFinalizationQueueService = (*service)(nil)
