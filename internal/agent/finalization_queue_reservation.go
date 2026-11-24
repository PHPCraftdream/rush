package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

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
