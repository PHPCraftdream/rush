package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// NewFinalizationBindScopeHook builds the driver-start BindScope hook (lane
// L4). The returned hook binds the finalization scope of one logical call
// from an already trusted capability: it resolves the call identity through
// finalizationLogicalIdentity, refuses any conflict with that capability
// (fail-closed), and installs the trusted authorities in the driver registry
// through bindFinalizationDriverCall. A call without bound authorities is
// inert and reports no error, matching the legacy flag-off behavior.
//
// The hook must be invoked in the frozen S1 order: after
// restoreFinalizationCallContext validated the carried capability, and
// before the first provider step. It never duplicates or replaces that
// restore, and it changes identity transport only: frozen policy,
// credentials, tools, budgets and model settings stay untouched.
func NewFinalizationBindScopeHook(agent *sessionAgent) func(context.Context, SessionAgentCall) error {
	return func(ctx context.Context, call SessionAgentCall) error {
		if call.FinalizationAuthorities.Requirement() == nil {
			return nil
		}
		if agent == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		trusted, err := finalize.FinalizationLogicalIdentity(call.FinalizationAuthorities, call.SessionID, call.LogicalCallID)
		if err != nil {
			return err
		}
		if trusted != call.LogicalCallID {
			return session.ErrAgentFinalizationChanged
		}
		return agent.bindFinalizationDriverCall(call)
	}
}

// restoreFinalizationCallContext preserves trusted capabilities across mailbox
// handoffs without deriving authority from a session ID or driver generation.
func (a *sessionAgent) restoreFinalizationCallContext(ctx context.Context, call *SessionAgentCall) (context.Context, error) {
	if call == nil {
		return nil, gate.ErrFinalizationFactsUnavailable
	}
	guardCtx, cancel := context.WithTimeout(ctx, a.effectiveSessionPreambleMaxDuration())
	defer cancel()
	validated, err := call.FinalizationAuthorities.RestoreCall(guardCtx, call.SessionID, call.LogicalCallID)
	if err != nil {
		return nil, err
	}
	call.FinalizationAuthorities = session.CaptureAgentFinalizationContext(validated)
	// Preserve the caller's lifetime, not the bounded validation context.
	return call.FinalizationAuthorities.Restore(ctx)
}

// The three turnStream methods below moved verbatim out of the deleted
// finalization_execution.go (DM6 extracted the pure finalize cluster to
// runtime/batch/batchcore/finalize); they stay in the agent root because
// turnStream/sessionAgent are exec/agentrun domain (DM7 destination:
// finalization_methods.go).

func (ts *turnStream) beginFinalizationStep(ctx context.Context) error {
	ts.finalizationStamp = gate.Stamp{}
	if ts.finalization == nil {
		return nil
	}
	stamp, err := ts.finalization.Gate.BeginCandidate(ctx, ts.finalization.Read)
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	return nil
}

// markFinalizationStep runs under the assistant lock, before adding a final footer.
func (ts *turnStream) markFinalizationStep() error {
	stamp := ts.finalizationStamp
	if !stamp.BelongsTo(ts.finalization.Gate) || stamp.Serial() == 0 || ts.fenceCheckpoint == nil {
		return gate.ErrFinalizationFactsUnavailable
	}
	if err := ts.currentAssistant.MarkFinalizationCandidate(message.FinalizationIdentity{
		Key: ts.finalization.LogicalID, Generation: stamp.Epoch(),
		Revision: stamp.DurableRevision(), Serial: stamp.Serial(),
	}); err != nil {
		return err
	}
	ts.currentAssistant.CheckpointGeneration = ts.fenceCheckpoint()
	return nil
}

// sealFinalizationCandidate stays in the root because it has no production
// caller outside this package and its behavior is exercised by root white-box tests.
// It presents the persisted candidate to session-level TrySeal and must only
// run from the admission gate's publish step while the owner gate is held.
// Without an installed scope or facts reader it refuses without touching the database.
func sealFinalizationCandidate(ctx context.Context, execution *finalize.FinalizationExecution, messages message.Service, identity message.FinalizationIdentity, candidate gate.Candidate) (message.Message, error) {
	if execution == nil || execution.Scope.Store == nil || execution.LogicalID == "" {
		return message.Message{}, gate.ErrFinalizationFactsUnavailable
	}
	if execution.Scope.Store == nil || identity.Key != execution.Scope.Identity.LogicalCallID || execution.Scope.Identity.Generation <= 0 ||
		identity.Generation != uint64(execution.Scope.Identity.Generation) || identity.Revision > uint64(1<<63-1) {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	writer, ok := messages.(session.FinalizationSealWriter)
	if !ok {
		return message.Message{}, gate.ErrFinalizationFactsUnavailable
	}
	sealed, ok := session.CandidateOf(candidate.MessageID, candidate.Hash, identity)
	if !ok {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	outcome, err := execution.Scope.Store.TrySeal(ctx, execution.Scope.Identity, sealed, writer, execution.ScopeFacts)
	if err != nil {
		return message.Message{}, err
	}
	if outcome.Sealed || outcome.Accepted {
		return outcome.Message, nil
	}
	if outcome.Reason == session.AgentFinalizationSealCancelled {
		return message.Message{}, context.Canceled
	}
	// The owner composes or waits; never latch a failure for a deterministic
	// "not yet".
	return message.Message{}, gate.ErrFinalizationScopeChanged
}

// persistFinalizationCandidate replaces the plain step write for a candidate
// footer. A stale candidate is dropped (no candidate is recorded) and is not a
// turn error: the owner composes again.
func (ts *turnStream) persistFinalizationCandidate(snap message.Message) error {
	ts.finalizationCandidate = finalize.PersistedFinalizationCandidate{}
	finish := snap.FinishPart()
	if finish == nil || ts.finalization.Scope.Store == nil {
		return gate.ErrFinalizationFactsUnavailable
	}
	persisted, err := finalize.PersistFinalizationCandidate(ts.genCtx, ts.a.sessions, ts.a.messages, ts.finalization.Scope, finish.FinalizationIdentity(), snap)
	if errors.Is(err, gate.ErrFinalizationScopeChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	ts.finalizationCandidate = persisted
	return nil
}

// bindFinalizationIdentity changes only identity transport, not frozen policy,
// credentials, tools, budgets, model settings or construction owners.
func (a *sessionAgent) bindFinalizationDriverCall(call SessionAgentCall) error {
	requirement := call.FinalizationAuthorities.Requirement()
	if requirement == nil {
		return nil
	}
	for _, identity := range requirement.Authorities {
		if identity.SessionID != call.SessionID {
			continue
		}
		if call.DriverGeneration == 0 {
			if identity.NodeID != "" || call.FinalizationAuthorities.RequiresManagedDriver() || a.isSubAgent {
				return session.ErrAgentFinalizationDriverUnavailable
			}
			return nil
		}
		if a.coord == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		drivers := a.coord.SubAgentDrivers()
		if drivers == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		return drivers.BindFinalizationIdentity(a, call)
	}
	return nil
}

// Keep the agent-side names while the remaining finalization wiring stays
// here; these aliases bridge the established agent surface to finalize.
type (
	FinalizationAuthorityRegistry = finalize.FinalizationAuthorityRegistry
)

func NewFinalizationAuthorityRegistry() *FinalizationAuthorityRegistry {
	return finalize.NewFinalizationAuthorityRegistry()
}

func finalizationExecutionFor(ctx context.Context, call SessionAgentCall) *finalize.FinalizationExecution {
	return finalize.FinalizationExecutionFor(ctx, call)
}

func isFinalizationCandidate(m message.Message) bool {
	return finalize.IsFinalizationCandidate(m)
}

// ErrFinalizationHooksIncomplete marks the installation of a partially filled
// hooks set, which is refused instead of silently running with missing steps.
var ErrFinalizationHooksIncomplete = errors.New("finalization driver hooks incomplete")

// FinalizationDriverHooks is the single place W1 wires lanes L4 (BindScope,
// RecoverScope), L5 (FinalizeTurn) and L6 (OnCancel) into the driver. The zero
// value keeps the inert legacy behavior: no production call site invokes any
// hook today, and the flag-off path must stay byte-identical to main. With the
// flag on, W1 must install every hook; a partially filled set is an
// installation error (fail-closed), not a quiet no-op.
type FinalizationDriverHooks struct {
	// BindScope binds the finalization scope at driver start, before the
	// first step (L4).
	BindScope func(context.Context, SessionAgentCall) error
	// RecoverScope recovers the persisted candidate at start/restart
	// (RecoverCandidate + resumeCandidateSerial, L4).
	RecoverScope func(context.Context, *finalize.FinalizationExecution) (session.AgentFinalizationRecovery, error)
	// FinalizeTurn runs the wait/compose/seal loop at the end of a turn (L5).
	FinalizeTurn func(context.Context, *turnStream) error
	// OnCancel is the cancellation entry from the coordinator interrupt (L6).
	OnCancel func(sessionID string)
}

// IsZero reports whether no hook is installed.
func (h FinalizationDriverHooks) IsZero() bool {
	return h.BindScope == nil && h.RecoverScope == nil && h.FinalizeTurn == nil && h.OnCancel == nil
}

// Validate accepts the fully empty set (inert legacy behavior) and the fully
// installed set, and refuses a partially filled set by naming the unset hooks.
func (h FinalizationDriverHooks) Validate() error {
	var unset []string
	if h.BindScope == nil {
		unset = append(unset, "BindScope")
	}
	if h.RecoverScope == nil {
		unset = append(unset, "RecoverScope")
	}
	if h.FinalizeTurn == nil {
		unset = append(unset, "FinalizeTurn")
	}
	if h.OnCancel == nil {
		unset = append(unset, "OnCancel")
	}
	if len(unset) == 0 || len(unset) == 4 {
		return nil
	}
	return fmt.Errorf("%w: unset hooks: %s", ErrFinalizationHooksIncomplete, strings.Join(unset, ", "))
}

func (c *coordinator) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
	return newFinalizationQueueComponent(c).restoreFinalizationQueuedCall(ctx, rowID, data)
}

func (c *coordinator) finalizationQueueDriver(call SessionAgentCall) (SessionAgent, error) {
	return newFinalizationQueueComponent(c).finalizationQueueDriver(call)
}

func (c *coordinator) selectFinalizationQueueDriver(sessionID, logicalID string, requirement *session.AgentFinalizationRequirement, managed bool) (SessionAgent, uint64, error) {
	return newFinalizationQueueComponent(c).selectFinalizationQueueDriver(sessionID, logicalID, requirement, managed)
}

// A durable child continuation cannot silently become a root-agent run.
func (p finalizationQueueComponent) finalizationQueueDriver(call SessionAgentCall) (SessionAgent, error) {
	driver, _, err := p.selectFinalizationQueueDriver(call.SessionID, call.LogicalCallID, call.FinalizationAuthorities.Requirement(), call.FinalizationAuthorities.RequiresManagedDriver())
	return driver, err
}

func (p finalizationQueueComponent) selectFinalizationQueueDriver(sessionID, logicalID string, requirement *session.AgentFinalizationRequirement, managed bool) (SessionAgent, uint64, error) {
	if requirement == nil {
		return p.host.currentAgent(), 0, nil
	}
	if driver, ok := p.host.subAgentDrivers().Get(sessionID); ok && driver.Agent != nil {
		stored := driver.FinalizationAuthorities.Requirement()
		if driver.Call.SessionID != sessionID || driver.Call.LogicalCallID != logicalID || stored == nil ||
			stored.Version != requirement.Version || !slices.Equal(stored.Authorities, requirement.Authorities) {
			return nil, 0, session.ErrAgentFinalizationDriverUnavailable
		}
		return driver.Agent, driver.Generation, nil
	}
	if managed || len(requirement.Authorities) != 1 {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	identity := requirement.Authorities[0]
	if identity.SessionID != sessionID || identity.LogicalCallID != logicalID || identity.NodeID != "" {
		return nil, 0, session.ErrAgentFinalizationDriverUnavailable
	}
	return p.host.currentAgent(), 0, nil
}

func (p finalizationQueueComponent) restoreFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (session.AgentFinalizationContext, error) {
	accepted, _ := ctx.Value(finalizationQueueAcceptedKey{}).(*finalizationQueueAccepted)
	if accepted != nil {
		payload, err := finalizationQueuePayload(data)
		if err != nil {
			return session.AgentFinalizationContext{}, err
		}
		if accepted.rowID != rowID || accepted.payload != payload || accepted.sessionID != data.SessionID || accepted.logicalID != data.LogicalCallID {
			return session.AgentFinalizationContext{}, session.ErrAgentFinalizationChanged
		}
		return accepted.snapshot, nil
	}
	store, ok := p.host.sessionsService().(session.AgentFinalizationQueueService)
	if !ok {
		if data.FinalizationRequirement != nil {
			return session.AgentFinalizationContext{}, session.ErrAgentFinalizationChanged
		}
		return session.AgentFinalizationContext{}, nil
	}
	guardCtx, cancel := context.WithTimeout(ctx, sessionPreambleMaxDurationDefault)
	defer cancel()
	return store.RestoreAgentFinalizationQueue(guardCtx, rowID, data)
}

type finalizationQueueComponentHost interface {
	sessionsService() session.Service
	currentAgent() SessionAgent
	subAgentDrivers() *toolwrap.Registry
	readyWait() error
	rebuildSessionAgentCall() func(context.Context, string, session.SessionAgentCallData) (SessionAgentCall, error)
	runSessionAgentCall() func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
}

func newFinalizationQueueComponent(c *coordinator) finalizationQueueComponent {
	return finalizationQueueComponent{host: finalizationQueueComponentHostAdapter{coordinator: c}}
}

func enqueueFinalizationCall(ctx context.Context, sessions session.Service, rowID string, call SessionAgentCall, raw []byte) error {
	restored, err := call.FinalizationAuthorities.Restore(ctx)
	if err != nil {
		return err
	}
	return sessions.EnqueueRunQueueEntry(restored, rowID, call.SessionID, raw)
}

// FinalizationQueueCoordinator keeps marked queue admission on one driver era.
type FinalizationQueueCoordinator interface {
	RunFinalizationQueuedCall(context.Context, string, session.SessionAgentCallData) (*fantasy.AgentResult, error)
}

type finalizationQueueAcceptedKey struct{}

type finalizationQueueAccepted struct {
	rowID, sessionID, logicalID string
	payload                     [32]byte
	snapshot                    session.AgentFinalizationContext
	driver                      SessionAgent
	generation                  uint64
}

func finalizationQueuePayload(data session.SessionAgentCallData) ([32]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func finalizationQueueAdmissionError(err error) error {
	if errors.Is(err, session.ErrAgentFinalizationLeaseChanged) {
		return errors.Join(session.ErrCallQueuedNotExecuted, err)
	}
	return &ErrCallAlreadyAttempted{Err: err}
}

func (c *coordinator) RunFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (*fantasy.AgentResult, error) {
	return newFinalizationQueueComponent(c).runQueuedCall(ctx, rowID, data)
}

func (p finalizationQueueComponent) runQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (*fantasy.AgentResult, error) {
	return p.runFinalizationQueuedCall(ctx, rowID, data, p.host.rebuildSessionAgentCall(), p.host.runSessionAgentCall())
}

func (c *coordinator) runFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData,
	rebuild func(context.Context, string, session.SessionAgentCallData) (SessionAgentCall, error),
	run func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error),
) (*fantasy.AgentResult, error) {
	return newFinalizationQueueComponent(c).runFinalizationQueuedCall(ctx, rowID, data, rebuild, run)
}

func (p finalizationQueueComponent) runFinalizationQueuedCall(ctx context.Context, rowID string, data session.SessionAgentCallData,
	rebuild func(context.Context, string, session.SessionAgentCallData) (SessionAgentCall, error),
	run func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error),
) (*fantasy.AgentResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.host.readyWait(); err != nil {
		return nil, err
	}
	store, ok := p.host.sessionsService().(session.AgentFinalizationQueuePreparer)
	if !ok || data.FinalizationRequirement == nil {
		return nil, &ErrCallAlreadyAttempted{Err: session.ErrAgentFinalizationChanged}
	}
	guardCtx, guardCancel := context.WithTimeout(ctx, sessionPreambleMaxDurationDefault)
	prepared, err := store.PrepareAgentFinalizationQueue(guardCtx, rowID, data)
	guardCancel()
	if err != nil {
		return nil, finalizationQueueAdmissionError(err)
	}
	driver, generation, err := p.selectFinalizationQueueDriver(data.SessionID, data.LogicalCallID, prepared.Requirement(), prepared.RequiresManagedDriver())
	if err != nil || driver == nil {
		return nil, &ErrCallAlreadyAttempted{Err: session.ErrAgentFinalizationDriverUnavailable}
	}
	if driver.ShuttingDown() {
		return nil, ErrAgentShuttingDown
	}
	holdCtx, epoch, holdCancel, reserved := driver.ReserveExclusive(ctx, data.SessionID)
	if !reserved {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, session.ErrCallQueuedNotExecuted
	}
	handedOff := false
	defer func() {
		if !handedOff {
			driver.ReleaseExclusive(data.SessionID, epoch, holdCancel)
			return
		}
		holdCancel()
	}()
	if err := holdCtx.Err(); err != nil {
		return nil, &ErrCallAlreadyAttempted{Err: err}
	}
	if driver.ShuttingDown() {
		return nil, ErrAgentShuttingDown
	}
	consumeCtx, consumeCancel := context.WithTimeout(holdCtx, sessionPreambleMaxDurationDefault)
	snapshot, frozen, err := prepared.Consume(consumeCtx)
	consumeCancel()
	if err != nil {
		return nil, finalizationQueueAdmissionError(err)
	}
	payload, err := finalizationQueuePayload(frozen)
	if err != nil {
		return nil, &ErrCallAlreadyAttempted{Err: err}
	}
	accepted := &finalizationQueueAccepted{
		rowID: rowID, sessionID: frozen.SessionID, logicalID: frozen.LogicalCallID,
		payload: payload, snapshot: snapshot, driver: driver, generation: generation,
	}
	executionCtx := context.WithValue(holdCtx, finalizationQueueAcceptedKey{}, accepted)
	// Keep the preparation parent alive through Run, so old in-flight cancel
	// handles still cancel it. The outer defer owns the real holdCancel.
	executionCtx = WithReservedOwnership(executionCtx, frozen.SessionID, epoch, func() {}, func() { handedOff = true })
	call, err := rebuild(executionCtx, rowID, frozen)
	if err != nil {
		return nil, &ErrCallAlreadyAttempted{Err: err}
	}
	call.FinalizationAuthorities = snapshot
	call.DriverGeneration = generation
	if call.CallOptions != nil {
		defer call.CallOptions.BuildScope.Close()
	}
	if call.SessionID != frozen.SessionID || call.LogicalCallID != frozen.LogicalCallID {
		return nil, &ErrCallAlreadyAttempted{Err: session.ErrAgentFinalizationChanged}
	}
	if err := executionCtx.Err(); err != nil {
		return nil, &ErrCallAlreadyAttempted{Err: err}
	}
	result, err := run(executionCtx, call)
	if err == nil && executionCtx.Err() != nil {
		return nil, &ErrCallAlreadyAttempted{Err: executionCtx.Err()}
	}
	if err == nil && !handedOff {
		return nil, &ErrCallAlreadyAttempted{Err: gate.ErrFinalizationFactsUnavailable}
	}
	return result, err
}

func (c *coordinator) finalizationQueueDriverForRun(ctx context.Context, call SessionAgentCall) (SessionAgent, error) {
	return newFinalizationQueueComponent(c).finalizationQueueDriverForRun(ctx, call)
}

func (p finalizationQueueComponent) finalizationQueueDriverForRun(ctx context.Context, call SessionAgentCall) (SessionAgent, error) {
	driver, generation, err := p.selectFinalizationQueueDriver(call.SessionID, call.LogicalCallID, call.FinalizationAuthorities.Requirement(), call.FinalizationAuthorities.RequiresManagedDriver())
	if err != nil {
		return nil, err
	}
	accepted, _ := ctx.Value(finalizationQueueAcceptedKey{}).(*finalizationQueueAccepted)
	if accepted != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if accepted != nil && (accepted.sessionID != call.SessionID || accepted.logicalID != call.LogicalCallID || accepted.driver != driver || accepted.generation != generation) {
		return nil, session.ErrAgentFinalizationDriverUnavailable
	}
	return driver, nil
}

type finalizationQueueComponent struct {
	host finalizationQueueComponentHost
}

type finalizationQueueComponentHostAdapter struct{ coordinator *coordinator }

func (h finalizationQueueComponentHostAdapter) sessionsService() session.Service {
	return h.coordinator.sessions
}

func (h finalizationQueueComponentHostAdapter) currentAgent() SessionAgent {
	return h.coordinator.currentAgent
}

func (h finalizationQueueComponentHostAdapter) subAgentDrivers() *toolwrap.Registry {
	return h.coordinator.subAgentDrivers
}

func (h finalizationQueueComponentHostAdapter) rebuildSessionAgentCall() func(context.Context, string, session.SessionAgentCallData) (SessionAgentCall, error) {
	return h.coordinator.RebuildSessionAgentCall
}

func (h finalizationQueueComponentHostAdapter) runSessionAgentCall() func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
	return h.coordinator.RunSessionAgentCall
}

func (h finalizationQueueComponentHostAdapter) readyWait() error {
	return h.coordinator.readyWg.Wait()
}

var _ FinalizationQueueCoordinator = (*coordinator)(nil)

// ErrFinalizationComposeUnavailable refuses to rebuild the answer when there
// is nothing recomposable to rebuild; it is fail-closed, never a silent pass.
var ErrFinalizationComposeUnavailable = errors.New("finalization compose unavailable")

// FinalizationComposeInput carries what a recomposition needs: the turn, its
// execution, the cloned provisional answer, and a fresh stamp from
// gate.BeginCandidate.
type FinalizationComposeInput struct {
	Turn        *turnStream
	Execution   *finalize.FinalizationExecution
	Provisional message.Message
	Stamp       gate.Stamp
}

// FinalizationComposeFunc rebuilds the provisional answer under the fresh
// stamp and returns the persisted candidate; nil deps.Compose uses the
// built-in defaultComposeFinalizationCandidate.
type FinalizationComposeFunc func(context.Context, FinalizationComposeInput) (finalize.PersistedFinalizationCandidate, error)

// composeFinalizationCandidate rebuilds the provisional answer under a fresh
// stamp and writes the fresh stamp and persisted candidate back to the turn.
// FinalizeTurn runs after the callback sequence, so these fields are only
// touched here and by the loop itself.
func composeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalize.FinalizationExecution, deps FinalizationWaitDeps) error {
	ts.mu.Lock()
	if ts.currentAssistant == nil {
		ts.mu.Unlock()
		return fmt.Errorf("%w: no provisional assistant answer to recompose", ErrFinalizationComposeUnavailable)
	}
	snapshot := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	stamp, err := execution.Gate.BeginCandidate(ctx, execution.Read)
	if err != nil {
		return fmt.Errorf("finalization fresh stamp failed: %w", err)
	}
	var persisted finalize.PersistedFinalizationCandidate
	if deps.Compose != nil {
		persisted, err = deps.Compose(ctx, FinalizationComposeInput{
			Turn: ts, Execution: execution, Provisional: snapshot, Stamp: stamp,
		})
	} else {
		persisted, err = defaultComposeFinalizationCandidate(ctx, ts, execution, snapshot, stamp)
	}
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	ts.finalizationCandidate = persisted
	return nil
}

// defaultComposeFinalizationCandidate re-marks the cloned provisional answer
// with the fresh stamp identity and persists it; creds, role, tools and
// budgets are preserved by construction because it is the same turnStream.
func defaultComposeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalize.FinalizationExecution, snapshot message.Message, stamp gate.Stamp) (finalize.PersistedFinalizationCandidate, error) {
	finish := snapshot.FinishPart()
	if finish == nil || !finish.Partial || finish.FinalizationKey != execution.LogicalID {
		return finalize.PersistedFinalizationCandidate{}, fmt.Errorf("%w: the provisional answer carries no recomposable candidate footer", ErrFinalizationComposeUnavailable)
	}
	identity := message.FinalizationIdentity{
		Key: execution.LogicalID, Generation: stamp.Epoch(),
		Revision: stamp.DurableRevision(), Serial: stamp.Serial(),
	}
	if err := snapshot.MarkFinalizationCandidate(identity); err != nil {
		return finalize.PersistedFinalizationCandidate{}, err
	}
	return finalize.PersistFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.Scope, identity, snapshot)
}

// finalizationPublishClosure builds the publication gate.TrySeal calls back:
// it presents the turn's persisted candidate, verifies the accepted row, and
// reports a durable scope change through the scopeChanged flag so the loop can
// wait instead of recomposing. It runs only under the gate's mutex.
func finalizationPublishClosure(ts *turnStream, execution *finalize.FinalizationExecution, scopeChanged *bool) func(context.Context, gate.Candidate) error {
	return func(ctx context.Context, candidate gate.Candidate) error {
		persisted := ts.finalizationCandidate
		if candidate.MessageID != persisted.Candidate.MessageID || candidate.Hash != persisted.Candidate.Hash {
			*scopeChanged = true
			return gate.ErrFinalizationScopeChanged
		}
		accepted, err := finalize.PublishFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.Scope, persisted.Identity, candidate)
		if err != nil {
			if errors.Is(err, gate.ErrFinalizationScopeChanged) {
				*scopeChanged = true
			}
			return err
		}
		// A successful publication clears the flag: the flag survives only
		// while the durable side keeps answering "not yet".
		finish := accepted.FinishPart()
		if !accepted.IsFinished() || accepted.ID != candidate.MessageID || accepted.SessionID != ts.call.SessionID ||
			finish == nil || finish.Reason != message.FinishReasonEndTurn ||
			finish.FinalizationKey != persisted.Identity.Key ||
			finish.FinalizationGeneration != persisted.Identity.Generation ||
			finish.FinalizationRevision != persisted.Identity.Revision ||
			finish.FinalizationSerial != persisted.Identity.Serial ||
			finish.FinalizationHash != candidate.Hash {
			return fmt.Errorf("%w: publisher returned no matching accepted row", gate.ErrFinalizationFactsUnavailable)
		}
		ts.mu.Lock()
		*ts.currentAssistant = accepted
		ts.mu.Unlock()
		return nil
	}
}

// rootFinalizationGeneration is the durable generation of a root run's
// finalization record: one root incarnation, generation 1.
const rootFinalizationGeneration = 1

// startExecution is the Run entry (agent_run.go): the root bind, then the
// wiring's driver start. A nil wiring (flag off) is the no-op pass-through:
// the context, the call and a no-op cleanup come back unchanged.
func (a *sessionAgent) startExecution(ctx context.Context, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	return a.finalizationWiring().bindRootFinalization(ctx, a, call)
}

// bindRootFinalization binds the owner's finalization record for a FRESH root
// call: retire stale active incarnations of the session, bind identity
// generation 1, carry the authority in the context and on the call, register
// it in the process registry so runner-side batch commits can admit, then
// run the existing driver start. A call that already carries a requirement
// for its own session (descendants, queue-managed calls), a sub-agent call,
// a driver-bound call, or a session without the finalization service skips
// the bind and passes straight through. Rebinding an identity whose record
// already left the active phases (an accepted rerun) applies no
// finalization and reports no error: the call runs the legacy path. Every
// other bind failure fails closed and refuses the run.
func (w *finalizationWiring) bindRootFinalization(ctx context.Context, a *sessionAgent, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	if w == nil {
		return ctx, call, func() {}, nil
	}
	if !rootBindEligible(a, call) {
		return passThroughExecution(ctx, w, call)
	}
	svc, ok := a.sessions.(session.AgentFinalizationService)
	if !ok {
		return passThroughExecution(ctx, w, call)
	}
	if w.authorities == nil {
		return nil, call, nil, errors.New("finalization wiring: authority registry unavailable")
	}
	if w.store == nil {
		return nil, call, nil, errors.New("finalization wiring: store unavailable")
	}
	if _, err := w.store.RetireStaleActive(ctx, call.SessionID, call.LogicalCallID); err != nil {
		return nil, call, nil, err
	}
	authority, err := svc.BindAgentFinalization(ctx, session.AgentFinalizationIdentity{
		SessionID: call.SessionID, LogicalCallID: call.LogicalCallID, Generation: rootFinalizationGeneration,
	})
	if err != nil {
		if errors.Is(err, session.ErrAgentFinalizationChanged) {
			// The logical call's record already left the active phases (an
			// accepted rerun): finalization is not applicable, the call
			// goes legacy without an execution.
			return ctx, call, func() {}, nil
		}
		return nil, call, nil, err
	}
	boundCtx, err := session.WithAgentFinalizationContext(ctx, authority)
	if err != nil {
		return nil, call, nil, err
	}
	call.FinalizationAuthorities = session.CaptureAgentFinalizationContext(boundCtx)
	unregisterAuthority := w.authorities.Register(call.SessionID, authority)
	execCtx, unregisterExecution, err := w.startExecution(boundCtx, call)
	if err != nil {
		unregisterAuthority()
		return nil, call, nil, err
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			unregisterAuthority()
			if execCtx != nil && execCtx.Err() != nil {
				// The run unwound through cancellation while its record is
				// still active: the incarnation is over, mark it cancelled
				// so the terminal vocabulary and the ticket fencing agree.
				retireCtx, retireCancel := context.WithTimeout(context.WithoutCancel(execCtx), 5*time.Second)
				defer retireCancel()
				if _, retireErr := w.store.RetireCancelledLogical(retireCtx, call.SessionID, call.LogicalCallID); retireErr != nil {
					slog.Warn("Failed to retire the cancelled finalization record",
						"session_id", call.SessionID, "err", retireErr)
				}
			}
			unregisterExecution()
		})
	}
	return execCtx, call, cleanup, nil
}

// rootBindEligible reports whether the call is a fresh root incarnation that
// may bind its own finalization record.
func rootBindEligible(a *sessionAgent, call SessionAgentCall) bool {
	if a == nil || a.isSubAgent || call.DriverGeneration != 0 {
		return false
	}
	if call.SessionID == "" || call.LogicalCallID == "" {
		return false
	}
	if requirement := call.FinalizationAuthorities.Requirement(); requirement != nil {
		for _, identity := range requirement.Authorities {
			if identity.SessionID == call.SessionID {
				return false
			}
		}
	}
	return true
}
