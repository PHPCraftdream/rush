package finalize

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// FinalizationExecution belongs to one logical call, not its inherited
// children.
type FinalizationExecution struct {
	SessionID string
	LogicalID string
	Gate      *gate.Gate
	Read      func(context.Context) (gate.Facts, error)
	// Scope is the durable record candidates are persisted against; without
	// it a candidate cannot be persisted and the step fails closed.
	Scope FinalizationPublicationScope
	// ScopeFacts is the durable scope-facts reader TrySeal consults; without
	// it the seal refuses (fail-closed) until a later slice installs the
	// reader.
	ScopeFacts session.AgentFinalizationScopeReader
}

type finalizationExecutionContextKey struct{}

// WithFinalizationExecution installs the execution in ctx.
func WithFinalizationExecution(ctx context.Context, execution *FinalizationExecution) (context.Context, error) {
	if execution == nil || execution.SessionID == "" || execution.LogicalID == "" || execution.Gate == nil || execution.Gate.Epoch() == 0 {
		return nil, gate.ErrFinalizationFactsUnavailable
	}
	return context.WithValue(ctx, finalizationExecutionContextKey{}, execution), nil
}

// FinalizationExecutionFor returns the execution installed for exactly this
// session/logical-call pair.
func FinalizationExecutionFor(ctx context.Context, call agentcall.SessionAgentCall) *FinalizationExecution {
	if ctx == nil {
		return nil
	}
	execution, _ := ctx.Value(finalizationExecutionContextKey{}).(*FinalizationExecution)
	if execution == nil || execution.SessionID != call.SessionID || execution.LogicalID != call.LogicalCallID {
		return nil
	}
	return execution
}

// IsFinalizationCandidate reports an unfinished provisional footer.
func IsFinalizationCandidate(m message.Message) bool {
	finish := m.FinishPart()
	return finish != nil && finish.Partial && finish.FinalizationKey != ""
}

// FinalizationLogicalIdentity reads an already trusted capability, never a
// payload declaration. It does not bind a scope or change an existing call ID.
func FinalizationLogicalIdentity(snapshot session.AgentFinalizationContext, sessionID, logicalID string) (string, error) {
	requirement := snapshot.Requirement()
	if requirement == nil {
		return logicalID, nil
	}
	for _, identity := range requirement.Authorities {
		if identity.SessionID != sessionID {
			continue
		}
		if logicalID != "" && logicalID != identity.LogicalCallID {
			return "", session.ErrAgentFinalizationChanged
		}
		return identity.LogicalCallID, nil
	}
	return logicalID, nil
}

// finalizationRecordPhaseActive reports whether a durable record phase still
// allows recovery work. The active phases mirror the frozen fencing SQL.
func finalizationRecordPhaseActive(phase string) bool {
	switch phase {
	case "executing", "waiting_work", "composing":
		return true
	default:
		return false
	}
}

// NewFinalizationRecoverScopeHook builds the start/restart RecoverScope hook.
func NewFinalizationRecoverScopeHook(messages message.Service) func(context.Context, *FinalizationExecution) (session.AgentFinalizationRecovery, error) {
	return func(ctx context.Context, execution *FinalizationExecution) (session.AgentFinalizationRecovery, error) {
		if err := ctx.Err(); err != nil {
			return session.AgentFinalizationNoCandidate, err
		}
		if execution == nil || execution.Gate == nil || execution.Scope.Store == nil {
			return session.AgentFinalizationNoCandidate, gate.ErrFinalizationFactsUnavailable
		}
		record, err := execution.Scope.Store.Snapshot(ctx, execution.Scope.Identity)
		if err != nil {
			return session.AgentFinalizationNoCandidate, err
		}
		if !finalizationRecordPhaseActive(record.Phase) {
			execution.Gate.ResumeCandidateSerial(uint64(record.CandidateSerial))
			return session.AgentFinalizationNoCandidate, nil
		}
		return recoverFinalizationCandidate(ctx, messages, execution.Scope, execution.Gate)
	}
}
