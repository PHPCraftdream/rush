package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// newFinalizationPhysicalHoldsFacts is the single assembly point W1 wires:
// it builds the composite physical-holds facts reader over the in-process
// providers of one installed batch runtime and folds in the host facts
// reader via the Sum combinator. The sources are exactly three: batch
// roots (ledger-attributed through the root lease store), tracked process
// trees (process-level conservative over-count), and host facts (the
// session-package transaction reader). A nil install or a nil host facts
// reader fails closed with an error instead of panicking or silently
// reading zero.
func newFinalizationPhysicalHoldsFacts(install *BatchInstall, hostFacts session.FinalizationPhysicalHoldsFacts) (session.FinalizationPhysicalHoldsFacts, error) {
	if install == nil {
		return nil, fmt.Errorf("%w: batch install is nil",
			ErrFinalizationHoldsRegistryUnreadable)
	}
	if hostFacts == nil {
		return nil, fmt.Errorf("%w: finalization physical holds host facts are nil",
			session.ErrFinalizationPhysicalHoldsUnreadable)
	}
	composite := session.NewFinalizationPhysicalHoldsFactsFromProviders(
		NewFinalizationBatchRootHoldsProviderForInstall(install),
		NewFinalizationTrackedProcessHoldsProvider(),
	)
	return session.NewFinalizationPhysicalHoldsFactsSum(composite, hostFacts), nil
}

// ErrFinalizationHoldsUnattributable reports a live physical hold whose
// owning session cannot be proven from the in-process registries, so the
// per-owner count is uncomputable and the provider fails closed.
var ErrFinalizationHoldsUnattributable = errors.New(
	"finalization physical hold is not attributable to the owner session " +
		"from the in-process registries")

// ErrFinalizationHoldsRegistryUnreadable reports provider wiring whose
// registries are nil, so the source cannot be observed at all.
var ErrFinalizationHoldsRegistryUnreadable = errors.New(
	"finalization physical holds registry is unreadable")

// diskOwnerKeysOfKind snapshots the manager's owner keys of the given kinds
// under one lock acquisition, releasing the lock before attribution runs.
func diskOwnerKeysOfKind(m *diskCapabilityManager, kinds ...diskOwnerKind) []diskOwnerKey {
	if m == nil {
		return nil
	}
	selected := make(map[diskOwnerKind]struct{}, len(kinds))
	for _, kind := range kinds {
		selected[kind] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]diskOwnerKey, 0, len(m.owners))
	for key := range m.owners {
		if _, ok := selected[key.kind]; ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// batchRootLeaseRoots snapshots the lease ledger's root ids under one lock
// acquisition — delegated to the lease's Roots read (batchflow port via
// batchRootLeaseStore.roots), one independent observation beside the caps
// owners snapshot in the caller.
func batchRootLeaseRoots(s *batchRootLeaseStore) map[jobbatch.ID]struct{} {
	if s == nil {
		return nil
	}
	return s.Roots()
}

// NewFinalizationBatchRootHoldsProvider counts the batch-root physical holds
// owned by one session from the live capability owner set and the lease
// ledger. It returns a session.FinalizationHoldsProvider.
//
// Attribution evidence: production root-kind owners are created only via
// batchRootLeaseStore.acquire (batch_ingress.go and batch_model_control.go
// acquire call sites, plus the CLI submit path in batch_cli_runtime.go),
// and acquire now records the OWNING SESSION in the ledger entry itself.
// That ledger attribution replaced the old CLI-only runner-adapter index
// (batchRunnerAdapter.owners): ownerForRoot is the single attribution
// reader, so model-control and ingress roots attribute the same way CLI
// roots always did.
//
// Execution over-count: an execution-kind owner carries no session
// attribution (there is no execution-to-session index), so every live
// execution is counted as a hold of the QUERIED owner too — a deliberate
// conservative over-count. It is the safe side: the seal defers until the
// physical dispose completes, then the count drops. Execution lifecycle:
// the engine releases execution owners via the Disposer path
// (internal/jobbatch/runner_control.go maybeDispose after terminate plus
// giveUp plus zero pins -> batchBindingExecution.Dispose -> the once
// release), NOT immediately at node-terminal; a dispose TIMEOUT or panic
// intentionally retains the owner (batch_exec.go), so the hold count stays
// elevated until the process restarts. That is truthful — physical
// disposal did not complete — never a silent zero. Call/construction/
// continuation kinds are capability scopes, not hold sources: construction
// pins are pinned-once app-level records that would block every
// finalization for the process lifetime, call owners are intra-invocation,
// and continuation is reserved and unused.
//
// Window safety: after the owner binds, the S1 fence refuses new
// reservations so batch roots only decrease, and cross-process roots stay
// covered by the durable batch_nodes projection read by the descendants
// facts reader.
//
// Locking: the function takes two separate snapshot acquisitions (caps
// owners, ledger entries) plus one ownerForRoot read, never nests them,
// and never holds either lock across attribution.
func NewFinalizationBatchRootHoldsProvider(
	leases *BatchRootLeaseStore,
	caps *DiskProviderRegistry,
) session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization batch root holds: %w", err)
		}
		if caps == nil || leases == nil {
			return 0, fmt.Errorf("%w: batch root hold registries are nil",
				ErrFinalizationHoldsRegistryUnreadable)
		}
		if ownerSessionID == "" {
			// Empty owner deterministically means zero owned holds.
			return 0, nil
		}
		keys := diskOwnerKeysOfKind(caps, diskOwnerRoot, diskOwnerExecution)
		if len(keys) == 0 {
			// A sound empty observation for every owner: the live owner
			// set holds no root or execution keys at all.
			return 0, nil
		}
		leaseRoots := batchRootLeaseRoots(leases)
		count := 0
		for _, key := range keys {
			if key.kind == diskOwnerExecution {
				// CONSERVATIVE OVER-COUNT: a live execution of ANY owner
				// counts as a hold of the queried owner too; there is no
				// execution-to-session index, so this is the safe side —
				// the seal defers until physical dispose, then passes.
				count++
				continue
			}
			if key.kind != diskOwnerRoot {
				continue
			}
			root := jobbatch.ID(key.instance)
			if _, ok := leaseRoots[root]; !ok {
				return 0, fmt.Errorf("%w: root %s holds a capability owner "+
					"outside the lease ledger",
					ErrFinalizationHoldsUnattributable, root)
			}
			owner := leases.OwnerForRoot(root)
			if owner == "" {
				// No owning session recorded — impossible in production
				// (acquire refuses an empty owner); the check stays as a
				// fail-closed guard against a forgotten entry.
				return 0, fmt.Errorf("%w: root %s has no owning session recorded",
					ErrFinalizationHoldsUnattributable, root)
			}
			if owner == ownerSessionID {
				count++
			}
		}
		return count, nil
	}
}

// NewFinalizationBatchRootHoldsProviderForInstall wires the provider to one
// installed batch runtime: live holds come from install.DiskBindings and
// attribution from install.RootLeases — the lease ledger the install's
// acquire call sites record owners into. RunnerHandle and any ownerFor
// delegate are no longer consulted. All fields are nil-guarded: a nil
// install or a dormant install (nil registries) fails closed at call time,
// never panics.
func NewFinalizationBatchRootHoldsProviderForInstall(install *BatchInstall) session.FinalizationHoldsProvider {
	var leases *BatchRootLeaseStore
	var caps *DiskProviderRegistry
	if install != nil {
		leases, caps = install.RootLeases, install.DiskBindings
	}
	return NewFinalizationBatchRootHoldsProvider(leases, caps)
}

// NewFinalizationTrackedProcessHoldsProvider counts the tracked process
// trees as a PROCESS-level conservative over-approximation: the pid-keyed
// registries in package proctrack (Job Objects on Windows, process
// groups elsewhere) carry no owning session, and the cliprovider spawn sites have
// no session index, so ANY live tracked tree defers every owner's seal
// until physical teardown, then the count returns to baseline. It returns
// a session.FinalizationHoldsProvider.
//
// No-self-deadlock guarantee for the owner's own current CLI stream, with
// code evidence: the spawn sites are
// internal/agent/cliprovider/provider_stream.go:424 (PTY) and :626 (pipe);
// unregistration is provider_stream.go:663 proctrack.UntrackProcessTree
// (childPid) inside visibleCleanup, deferred in the stream generator at
// :697, so it runs when the stream generator returns — before the turn
// pipeline observes stream completion. TrySeal only runs when a candidate
// is recorded (internal/session/finalization_seal.go refuses
// AgentFinalizationSealNoCandidate otherwise), and the candidate is
// persisted only after the provider step fully consumed the stream
// (internal/agent/agent_turn_step.go persistFinalizationCandidate ~:827).
// During wait no stream runs; during compose the seal follows that compose
// turn's own completed stream. Therefore the owner's own current CLI
// stream is always unregistered by seal time — no permanent self-block.
func NewFinalizationTrackedProcessHoldsProvider() session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization tracked process holds: %w", err)
		}
		if ownerSessionID == "" {
			// Empty owner deterministically means zero owned holds.
			return 0, nil
		}
		return session.CountFinalizationTrackedProcessHolds(), nil
	}
}

const (
	// defaultFinalizationPollInterval covers changes that are invisible to the
	// in-process change signal (work in other processes, quiet durable moves).
	defaultFinalizationPollInterval = 2 * time.Second
	// defaultFinalizationMaxRecompositions bounds a runaway recompose loop.
	defaultFinalizationMaxRecompositions = 8
	// finalizationHeldQuestionText is the stable pause question surfaced to
	// the operator when the driver holds an open question.
	finalizationHeldQuestionText = "An owned question is awaiting an answer."
)

// FinalizationWaitPhase names one observable phase of the FinalizeTurn loop.
type FinalizationWaitPhase string

const (
	FinalizationPhaseSealing   FinalizationWaitPhase = "sealing"
	FinalizationPhaseWaiting   FinalizationWaitPhase = "waiting"
	FinalizationPhaseComposing FinalizationWaitPhase = "composing"
	FinalizationPhasePaused    FinalizationWaitPhase = "paused"
	FinalizationPhaseAccepted  FinalizationWaitPhase = "accepted"
	FinalizationPhaseFailed    FinalizationWaitPhase = "failed"
)

// FinalizationWaitStatus is one observable transition of the wait loop; the
// typed refusal cause is the seal reason derived from the facts the gate saw.
type FinalizationWaitStatus struct {
	Phase         FinalizationWaitPhase
	Reason        coresession.AgentFinalizationSealReason
	Descendants   int
	Debt          int
	PhysicalHolds int
	QueuedCalls   int
	HeldQuestion  bool
	Cancelled     bool
}

// FinalizationWaitDeps carries the collaborators of the FinalizeTurn hook.
type FinalizationWaitDeps struct {
	// Settler settles reaction debts once per wait entry (lane L3).
	Settler session.AgentFinalizationDebtSettler
	// Compose rebuilds the provisional answer; nil uses the built-in
	// recomposition (see finalization_recompose.go).
	Compose FinalizationComposeFunc
	// PollInterval bounds every wait; <=0 means defaultFinalizationPollInterval.
	PollInterval time.Duration
	// MaxRecompositions bounds recompositions; <0 means the default, 0
	// legally forbids recomposition.
	MaxRecompositions int
	// OnState is an optional observer of every phase transition.
	OnState func(FinalizationWaitStatus)
}

// ErrFinalizationRecomposeLimit refuses to keep recomposing a candidate that
// can never become fresh enough; it is a fail-closed stop, not a wait.
var ErrFinalizationRecomposeLimit = errors.New("finalization recomposition limit reached")

// mapFinalizationSealReason derives the typed seal refusal cause from the
// facts the gate observed; a held question is a pause path, not a reason.
func mapFinalizationSealReason(f gate.Facts) coresession.AgentFinalizationSealReason {
	switch {
	case f.Cancelled:
		return coresession.AgentFinalizationSealCancelled
	case !f.Complete:
		return coresession.AgentFinalizationSealScopeIncomplete
	case f.Descendants > 0:
		return coresession.AgentFinalizationSealDescendantsActive
	case f.Debt > 0:
		return coresession.AgentFinalizationSealReactionDebt
	case f.PhysicalHolds > 0:
		return coresession.AgentFinalizationSealPhysicalHolds
	case f.QueuedCalls > 0:
		return coresession.AgentFinalizationSealQueuedCalls
	default:
		return coresession.AgentFinalizationSealNone
	}
}

func (d FinalizationWaitDeps) status(phase FinalizationWaitPhase, f gate.Facts) FinalizationWaitStatus {
	return FinalizationWaitStatus{
		Phase:         phase,
		Reason:        mapFinalizationSealReason(f),
		Descendants:   f.Descendants,
		Debt:          f.Debt,
		PhysicalHolds: f.PhysicalHolds,
		QueuedCalls:   f.QueuedCalls,
		HeldQuestion:  f.HeldQuestion,
		Cancelled:     f.Cancelled,
	}
}

func (d FinalizationWaitDeps) emit(phase FinalizationWaitPhase, f gate.Facts) {
	st := d.status(phase, f)
	if d.OnState != nil {
		d.OnState(st)
	}
	slog.Debug("Finalization wait entered",
		"phase", string(phase),
		"reason", st.Reason.String(),
		"descendants", st.Descendants,
		"debt", st.Debt,
		"physical_holds", st.PhysicalHolds,
		"queued_calls", st.QueuedCalls,
		"held_question", st.HeldQuestion,
		"cancelled", st.Cancelled,
	)
}

// NewFinalizationFinalizeTurnHook builds the L5 FinalizeTurn hook: the
// wait/compose/seal loop that runs at the end of a finalization-aware turn.
func NewFinalizationFinalizeTurnHook(deps FinalizationWaitDeps) func(context.Context, *turnStream) error {
	return func(ctx context.Context, ts *turnStream) error {
		return runFinalizationWait(ctx, ts, deps)
	}
}

// runFinalizationWait loops trySeal until the candidate is accepted, the turn
// pauses on a held question, the context is done, or a fail-closed error stops
// the loop. Every wait is bounded by the poll timer and the context.
func runFinalizationWait(ctx context.Context, ts *turnStream, deps FinalizationWaitDeps) error {
	execution := ts.finalization
	if execution == nil {
		return fmt.Errorf("%w: finalization execution unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	poll := deps.PollInterval
	if poll <= 0 {
		poll = defaultFinalizationPollInterval
	}
	allowed := deps.MaxRecompositions
	if allowed < 0 {
		allowed = defaultFinalizationMaxRecompositions
	}
	// scopeChanged remembers that the last publication refused as a durable
	// "not yet"; that compose verdict must not burn recomposition budget.
	// The closure runs only inside gate.TrySeal under its mutex.
	scopeChanged := false
	publish := finalizationPublishClosure(ts, execution, &scopeChanged)
	for {
		// Status facts are read directly, without re-entering the gate:
		// that helper requires the gate's mutex, which this loop does not
		// hold; trySeal re-reads and refuses under the lock.
		facts, err := execution.Read(ctx)
		if err != nil {
			facts = gate.Facts{}
		}
		if err := ctx.Err(); err != nil {
			deps.emit(FinalizationPhaseFailed, facts)
			return ctx.Err()
		}
		deps.emit(FinalizationPhaseSealing, facts)
		verdict, err := execution.Gate.TrySeal(ctx, ts.finalizationStamp, ts.finalizationCandidate.Candidate, execution.Read, publish)
		if err != nil {
			// An already sealed gate answers Accepted plus ErrFinalizationSealed;
			// sealing twice is idempotent and must not publish again.
			if verdict == gate.Accepted && errors.Is(err, gate.ErrFinalizationSealed) {
				deps.emit(FinalizationPhaseAccepted, facts)
				return nil
			}
			deps.emit(FinalizationPhaseFailed, facts)
			return fmt.Errorf("finalization seal failed: %w", err)
		}
		switch verdict {
		case gate.Accepted:
			deps.emit(FinalizationPhaseAccepted, facts)
			return nil
		case gate.PauseQuestion:
			deps.emit(FinalizationPhasePaused, facts)
			return &AwaitingAnswerError{Question: finalizationHeldQuestionText, SessionID: ts.call.SessionID}
		case gate.Compose:
			if scopeChanged {
				scopeChanged = false
				deps.emit(FinalizationPhaseWaiting, facts)
				// The durable side said "not yet": time heals, recomposition
				// cannot, so settle debts and wait instead of burning budget.
				if err := finalize.SettleFinalizationDebts(ctx, deps.Settler, execution.SessionID); err != nil {
					return err
				}
				if err := waitFinalizationRound(ctx, poll, nil); err != nil {
					return err
				}
				continue
			}
			if allowed <= 0 {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("%w: at most %d recompositions are allowed", ErrFinalizationRecomposeLimit, allowed)
			}
			allowed--
			deps.emit(FinalizationPhaseComposing, facts)
			if err := composeFinalizationCandidate(ctx, ts, execution, deps); err != nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return err
			}
		case gate.WaitWork:
			deps.emit(FinalizationPhaseWaiting, facts)
			if err := finalize.SettleFinalizationDebts(ctx, deps.Settler, execution.SessionID); err != nil {
				return err
			}
			waitVerdict, changed, err := execution.Gate.WaitState(ctx, execution.Read)
			if err != nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("finalization wait state read failed: %w", err)
			}
			if waitVerdict != gate.WaitWork {
				// The facts moved while arming the signal; re-evaluate from the
				// top so trySeal sees the fresh state.
				continue
			}
			if changed == nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("%w: finalization change signal unavailable", gate.ErrFinalizationFactsUnavailable)
			}
			if err := waitFinalizationRound(ctx, poll, changed); err != nil {
				return err
			}
		}
	}
}

// waitFinalizationRound sleeps until the change signal, the poll timer, or the
// context wakes the loop; a nil signal means only the timer and context.
func waitFinalizationRound(ctx context.Context, poll time.Duration, changed <-chan struct{}) error {
	timer := time.NewTimer(poll)
	defer timer.Stop()
	if changed == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

// FinalizationWiringDeps carries everything the composition root (internal/app)
// owns and the wiring needs. Enabled=false must produce a nil wiring and zero
// side effects.
type FinalizationWiringDeps struct {
	Enabled     bool
	Conn        *sql.DB
	Messages    message.Service
	NoticeStore *session.SessionNoticeStore
	DataDir     string
	Install     *BatchInstall
	// Authorities is the process-wide owner-to-authority registry the
	// composition root builds once when the flag is on and also hands to
	// the batch commit identity; the root bind registers the bound
	// authority here so runner-side commits can admit without a call
	// context. Optional for direct assembly, required by the root bind.
	Authorities *FinalizationAuthorityRegistry
	ResultGate  coresession.FinalizationTerminalGate
}

// finalizationWiring is the single production assembly of the finalization
// driver: durable stores, scope-facts readers, cancellation registry, and the
// L4/L5/L6 hooks. It is built once by the composition root and bound to the
// concrete coordinator.
type finalizationWiring struct {
	coord       *coordinator
	conn        *sql.DB
	store       *coresession.AgentFinalizationStore
	messages    message.Service
	noticeStore *session.SessionNoticeStore
	dataDir     string
	// resultGate is owned by the app-side batch gate; the wiring only carries
	// it and never reads it.
	resultGate  coresession.FinalizationTerminalGate
	descendants session.FinalizationDescendantsFacts
	queued      session.FinalizationQueuedCallsFacts
	holds       session.FinalizationPhysicalHoldsFacts
	debtFacts   session.FinalizationReactionDebtFacts
	settler     session.AgentFinalizationDebtSettler
	cancel      *finalize.FinalizationCancelRegistry
	authorities *FinalizationAuthorityRegistry
	poll        time.Duration
	hooks       FinalizationDriverHooks
}

// newFinalizationWiring assembles the wiring. A disabled flag yields a nil
// wiring with no side effects; a partially installable configuration fails
// closed instead of running with missing collaborators.
func newFinalizationWiring(deps FinalizationWiringDeps, coord *coordinator) (*finalizationWiring, error) {
	if !deps.Enabled {
		return nil, nil
	}
	var missing []string
	if deps.Conn == nil {
		missing = append(missing, "conn")
	}
	if deps.Messages == nil {
		missing = append(missing, "messages")
	}
	if deps.NoticeStore == nil {
		missing = append(missing, "notice store")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("finalization wiring: missing collaborators: %s",
			fmt.Sprint(missing))
	}
	// Without a batch runtime there is no batch-root holds source; the hold
	// facts degrade to the host driver facts, which report zero holds for
	// batch roots.
	holds := session.NewFinalizationHostDriverHoldsFacts(deps.DataDir)
	if deps.Install != nil {
		assembled, err := newFinalizationPhysicalHoldsFacts(deps.Install, holds)
		if err != nil {
			return nil, err
		}
		holds = assembled
	}
	// The debt store serves both as the durable facts reader and the settler.
	ds := session.NewFinalizationDebtStore(deps.Conn)
	w := &finalizationWiring{
		coord:       coord,
		conn:        deps.Conn,
		store:       coresession.NewAgentFinalizationStore(deps.Conn),
		messages:    deps.Messages,
		noticeStore: deps.NoticeStore,
		dataDir:     deps.DataDir,
		resultGate:  deps.ResultGate,
		descendants: session.NewFinalizationDescendantsReader(),
		queued:      session.NewFinalizationQueuedCallsFacts(),
		holds:       holds,
		debtFacts:   ds,
		settler:     ds,
		cancel:      finalize.NewFinalizationCancelRegistry(),
		authorities: deps.Authorities,
		// The wait loop re-reads durable facts at this cadence; changes made by
		// the batch runner never signal the in-process gate. The 5-minute
		// supervision window is far too coarse for a parent waiting on a child.
		poll: defaultFinalizationPollInterval,
	}
	w.hooks = FinalizationDriverHooks{
		BindScope:    w.bindScopeHook(),
		RecoverScope: finalize.NewFinalizationRecoverScopeHook(deps.Messages),
		FinalizeTurn: NewFinalizationFinalizeTurnHook(FinalizationWaitDeps{
			Settler:           ds,
			PollInterval:      w.poll,
			MaxRecompositions: -1,
		}),
		OnCancel: finalize.NewFinalizationOnCancelHook(w.cancel),
	}
	if err := w.hooks.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// scopeFacts builds the composite scope reader bound to one owner session; the
// interface itself carries no owner, so the owner is fixed here.
func (w *finalizationWiring) scopeFacts(ownerSessionID string) coresession.AgentFinalizationScopeReader {
	return session.NewAgentFinalizationScopeReader(ownerSessionID, w.descendants, w.queued, w.holds, w.debtFacts)
}

// readFacts is the production adapter from durable facts to the in-process
// finalizationFacts snapshot. The database runs with a single open connection,
// so the three reads below are strictly sequential and no read runs outside a
// transaction while one is open.
func (w *finalizationWiring) readFacts(ctx context.Context, identity coresession.AgentFinalizationIdentity) (gate.Facts, error) {
	if w == nil || w.store == nil || w.conn == nil || w.noticeStore == nil {
		return gate.Facts{}, gate.ErrFinalizationFactsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return gate.Facts{}, err
	}
	if identity.SessionID == "" || identity.LogicalCallID == "" || identity.Generation <= 0 {
		return gate.Facts{}, gate.ErrFinalizationFactsUnavailable
	}
	questions, err := w.noticeStore.PendingChildQuestions(ctx, identity.SessionID)
	if err != nil {
		return gate.Facts{}, fmt.Errorf("finalization facts: pending child questions: %w", err)
	}
	record, err := w.store.Snapshot(ctx, identity)
	if err != nil {
		return gate.Facts{}, fmt.Errorf("finalization facts: durable snapshot: %w", err)
	}
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return gate.Facts{}, fmt.Errorf("finalization facts: begin scope transaction: %w", err)
	}
	facts, scopeErr := w.scopeFacts(identity.SessionID).FinalizationSealScopeFacts(ctx, tx)
	if rollbackErr := tx.Rollback(); rollbackErr != nil && scopeErr == nil {
		scopeErr = fmt.Errorf("finalization facts: rollback scope transaction: %w", rollbackErr)
	}
	if scopeErr != nil {
		return gate.Facts{}, fmt.Errorf("finalization facts: scope facts: %w", scopeErr)
	}
	if !facts.Complete {
		// Fail closed: incomplete facts must never look quiet.
		return gate.Facts{}, fmt.Errorf("%w: finalization scope facts incomplete",
			gate.ErrFinalizationFactsUnavailable)
	}
	return gate.Facts{
		DurableRevision: uint64(record.Revision),
		Complete:        true,
		Own:             int(record.OpenTickets),
		Descendants:     facts.ActiveDescendants,
		PhysicalHolds:   facts.PhysicalHolds,
		Debt:            facts.ReactionDebt,
		QueuedCalls:     facts.QueuedCalls,
		HeldQuestion:    len(questions) > 0,
		Cancelled:       record.Cancelled,
	}, nil
}

// newExecution builds the per-call execution when the call owns a finalization
// authority of its own. Calls without a requirement, or descendants that only
// inherit a foreign authority, get no execution and stay inert.
func (w *finalizationWiring) newExecution(call SessionAgentCall) (*finalize.FinalizationExecution, error) {
	requirement := call.FinalizationAuthorities.Requirement()
	if requirement == nil {
		return nil, nil
	}
	var identity coresession.AgentFinalizationIdentity
	found := false
	for _, candidate := range requirement.Authorities {
		if candidate.SessionID == call.SessionID {
			identity = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	execution := &finalize.FinalizationExecution{
		SessionID:  identity.SessionID,
		LogicalID:  identity.LogicalCallID,
		Gate:       gate.New(uint64(identity.Generation)),
		Scope:      finalize.FinalizationPublicationScope{Store: w.store, Identity: identity},
		ScopeFacts: w.scopeFacts(identity.SessionID),
	}
	execution.Read = func(ctx context.Context) (gate.Facts, error) {
		return w.readFacts(ctx, identity)
	}
	return execution, nil
}

// bindScopeHook defers agent resolution to hook time: the wiring may be
// installed before any agent exists on the coordinator.
func (w *finalizationWiring) bindScopeHook() func(context.Context, SessionAgentCall) error {
	return func(ctx context.Context, call SessionAgentCall) error {
		if call.FinalizationAuthorities.Requirement() == nil {
			return nil
		}
		agent := w.rootAgent()
		if agent == nil {
			return coresession.ErrAgentFinalizationDriverUnavailable
		}
		return NewFinalizationBindScopeHook(agent)(ctx, call)
	}
}

// rootAgent resolves the current concrete session agent, if one is running.
func (w *finalizationWiring) rootAgent() *sessionAgent {
	if w == nil || w.coord == nil || w.coord.currentAgent == nil {
		return nil
	}
	agent, _ := w.coord.currentAgent.(*sessionAgent)
	return agent
}

// startExecution is the driver entry from Run: it binds the scope, builds the
// execution, recovers a persisted candidate, and registers cancellation. A nil
// wiring is the flag-off no-op.
func (w *finalizationWiring) startExecution(ctx context.Context, call SessionAgentCall) (context.Context, func(), error) {
	if w == nil {
		return ctx, func() {}, nil
	}
	if err := w.hooks.BindScope(ctx, call); err != nil {
		return nil, nil, err
	}
	execution, err := w.newExecution(call)
	if err != nil {
		return nil, nil, err
	}
	if execution == nil {
		return ctx, func() {}, nil
	}
	execCtx, err := finalize.WithFinalizationExecution(ctx, execution)
	if err != nil {
		return nil, nil, err
	}
	if w.hooks.RecoverScope != nil {
		recovery, err := w.hooks.RecoverScope(execCtx, execution)
		if err != nil {
			return nil, nil, err
		}
		// The verdict itself is informational here; adopt/fail side effects
		// already happened inside the hook.
		slog.Debug("Finalization recovery verdict",
			"session_id", execution.SessionID,
			"logical_call_id", execution.LogicalID,
			"recovery", uint8(recovery),
		)
	}
	w.cancel.Register(execution)
	return execCtx, func() { w.cancel.Unregister(execution) }, nil
}

// finalizeTurn is the driver exit from runTurn. A turn without an execution
// takes the legacy queue/drain/replacement path.
func (w *finalizationWiring) finalizeTurn(ctx context.Context, ts *turnStream) error {
	if w == nil || ts == nil || ts.finalization == nil || w.hooks.FinalizeTurn == nil {
		return nil
	}
	return w.hooks.FinalizeTurn(ctx, ts)
}

// finalizationOnCancel is the cancellation entry from the coordinator
// interrupt path; a nil wiring keeps it silent.
func (c *coordinator) finalizationOnCancel(sessionID string) {
	w := c.finalizationWiring()
	if w != nil && w.hooks.OnCancel != nil {
		w.hooks.OnCancel(sessionID)
	}
}

// finalizationWiring returns the wiring installed on the agent's coordinator.
func (a *sessionAgent) finalizationWiring() *finalizationWiring {
	if a == nil || a.coord == nil {
		return nil
	}
	finalization, ok := a.coord.Finalization().(coordinatorFinalizationAdapter)
	if !ok {
		return nil
	}
	return finalization.f
}

// passThroughExecution adapts the wiring driver start to the entry
// signature for calls that skip the root bind.
func passThroughExecution(ctx context.Context, w *finalizationWiring, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	execCtx, unregister, err := w.startExecution(ctx, call)
	return execCtx, call, unregister, err
}

// finalizationWiring returns the wiring installed on the coordinator.
func (c *coordinator) finalizationWiring() *finalizationWiring {
	if c == nil {
		return nil
	}
	return c.finalization
}
